# cmd/

## Purpose

Parent hub for Varroa's compiled entrypoints. Each subfolder is a `package main`
that wires flags, config, and lifecycle around business logic that lives in
`internal/`. This doc is the contract for the entrypoints themselves — CLI
flags, ports, startup/shutdown sequencing — not the reconciler/gRPC/HTTP logic
they invoke (see root `AGENTS.md` Architecture Map for that).

## Ownership

Owns: `main()` wiring, flag definitions, env var contracts, signal handling,
and which Makefile/Dockerfile target produces which binary. Does not own
reconciler phases, gRPC server internals, or HTTP route handlers — those are
`internal/controller`, `internal/mite`, `internal/api`, etc.

## Local Contracts

One entrypoint per subfolder, each with its own top-of-file doc comment
stating what it is/is not responsible for:

- **`operator/`** (`bin/varroa-operator`) — registers the controller-runtime
  reconciler for `Controller` CRDs; drives the `Pending→Provisioning→Running→
  Connected→Failed` phase state machine over NATS and serves the per-controller
  hibernation wake interstitial on `VARROA_WAKE_PORT` (default `8082`) from every
  replica. No gRPC/OIDC. Every replica serves `operator.<cluster>.approverestart`
  with no queue group (`approve_restart.go`); only the replica that owns the
  controller's shard replies. `ApproveRestart` returns `ErrNotShardOwner` on any
  other replica, and `handleApproveRestart` then sends no reply. With no owner
  during a handoff, the BFF request times out. `controllerIssuer` (`main.go`) is the only choice of
  the issuer that controllers trust: the BFF URL in `local` and `ldap` modes,
  else the OIDC issuer. The operator exits in those two modes without
  `--bff-url`. One leader replica queue-subscribes
  `varroa.ledger.actions.<own cluster>` in the `operator-workers` group
  (`subscribeLedgerActions` in `ledger_action.go`). Never subscribe the
  `varroa.ledger.actions.>` wildcard: every cluster's operator shares the
  core NATS and the group name, so a wildcard puts all of them in one queue
  group and NATS gives a share of one cluster's actions to the others.
  `handleLedgerAction` maps `cancel_run`/`retry_run`/`trigger_run`/`resume_run`
  to the matching mite imperative command and `wake_cell` to a direct
  hibernation wake. `cancel_run` and `retry_run` copy the action's `actor`
  onto the command, so the operator's outcome row names the recorder. The one core recorder publishes the actions of every
  cluster, so a member operator receives its actions over the shared bus. It logs at Warn and counts an action that names another cluster
  (`reason="foreign_cluster"`) or an unknown kind (`reason="unknown_kind"`)
  rather than acting on it. `trigger_run` wakes a hibernated target through the same hibernation
  call before it sends the command; a connected target is not woken.
  Every replica also subscribes to `events.brood.<cluster>`
  (`brood_watch.go`): a `disconnected` event enqueues an on-demand
  reconcile for the named Controller, which performs the pod read, and
  wakes the `team` ticker through its buffered, size-one `wake` channel.
  A `connected` event calls `TriggerOwnedReconcile`, which enqueues only on
  the replica that owns the Controller and routes no wake patch, because
  every replica receives the broadcast. That reconcile reads the new epoch
  and pushes the desired state, which NATS does not keep.
  **The event decides nothing** — it only removes latency, so a foreign
  cluster, any other event value, and a missed message all cost one tick
  and no more, and the wake send never blocks.
- **`gateway/`** (`bin/varroa-gateway`) — terminates mTLS/gRPC from mite
  sidecars on `:9090` and bridges each `CommandStream` to NATS
  (`mite.<cluster>.<ns>.<name>.{in,out,content,desired}`). Loads the shared CA from the `varroa-ca`
  Secret. Also serves an apikey-verify HTTP endpoint (default `:9092`). Does
  not reconcile controllers.
- **`bff/`** (`bin/varroa-bff`) — Backend-For-Frontend; REST API + SSE on
  `:8080`. Stateless — mite telemetry comes from the NATS bus, OIDC JWT needs
  no server-side session. Multiple replicas run behind a load balancer
  (leader election only for the parts that must be singleton). Wires
  `Deps.FleetPluginInventory` via `api.NewFleetInventoryReader(miteTransport)`.
  Reads `VARROA_CELLS_MAX_TENANTS` into `Deps.CellMaxTenants`, unconditionally
  and not inside the record-store block: it is the chart tier of the cell
  usage reads' tenant cap, and one chart value renders it into this binary and
  into the operator, so the two agree.
- **`updatecenter/`** (`bin/varroa-updatecenter`) — opt-in in-cluster Jenkins
  update center on `:8080` (`VARROA_UC_LISTEN`); thin main over
  `internal/updatecenter`, configured entirely via `VARROA_UC_*` env vars
  (storage local|oci, pull-through, import token, declared-plugins file, upload
  cap, single-writer gate). `VARROA_UC_ARCHIVE_BASE_URL` is the one env var read
  through `os.LookupEnv` rather than `envDefault`, because its three states are
  distinct: unset keeps the built-in Maven-repository fallback, a value redirects
  it at an internal mirror, and an explicitly empty value disables it. Anything
  that collapses unset into empty silently turns the fallback off. Serves
  `update-center.actual.json` + plugin `.hpi` blobs from an `internal/oci`
  BlobStore; local storage mode is single-replica (PVC), oci mode is stateless.
  Built by `make build` into the same product image as the other backends.
- **`recorder/`** (`bin/varroa-recorder`) — the build-record store
  recorder; HTTP on `:8080` with only `/healthz`, `/readyz`, `/metrics`.
  The only writer of `internal/recordstore`'s Postgres tables, the only
  owner of its migrations, and the only creator of the `varroa_builds`
  and `varroa_activity` JetStream streams. Nine goroutines: the build
  ingest pull consumer, the activity ingest pull consumer, the
  `varroa.ledger.claim` responder, the owner responder, the tenant purge
  responder, the tenant sync responder, the `varroa.ledger.crosstenant`
  responder (dispatches eight request kinds: `build`, `status`,
  `last_successful`, `artifact_list`, `artifact_url`, `lock_acquire`,
  `lock_renew`, `lock_release`; an unknown kind is rejected before any
  database read), the hourly retention/maintenance ticker, and the
  once-a-minute backstop scheduler (D9's interrupted sweep and D11's
  cron backstop, one leader-elected round under a third advisory-lock
  key, plus the claim-expiry sweep and the unstarted-intent re-fire) (`runWorkers` in `run.go` takes
  a variadic `namedWorker` list, so a worker's name travels with its
  function). `confirmedGoneGraceFromEnv` reads
  `VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE` as a **Go duration string**
  and feeds it to the tenant-sync responder, the backstop round, and the
  retention liveness seam; it does not reuse `envDurationMillis`, whose
  millisecond integer reads badly in a chart value.
  One `recordstore.NewRunNotifier` value serves the whole process: the
  ingest path takes it through `SetRunNotifier`, the cross-tenant and
  tenant-sync responders take it as their own argument, and
  `MaintenanceConfig.LockNotifier` carries it into the hourly round, and
  `BackstopConfig.RunNotifier` carries it into the claim-expiry sweep, so
  every lock write publishes on one coalescing counter and one clock.
  Needs no Kubernetes API access. `run(ctx, cfg)` in `run.go` holds the
  whole wiring and shutdown sequence, so a test drives it without
  `main`'s flag parsing. Does not serve any REST or MCP surface — that is
  `bff/`, through `internal/recordstore.Reader`.
- **`mite/`** (`bin/varroa-mite`) — sidecar injected into every Jenkins pod;
  own contract in `cmd/mite/AGENTS.md`. Reports health, state, observability,
  and full plugin inventories; applies desired-state and imperative commands.
  Also runs a `127.0.0.1:9095` HTTP loopback listener for the co-located
  Jenkins plugin's build-event and claim traffic.
- **`varroactl/`** (`bin/varroactl`) — CLI + MCP surface; own contract in
  `cmd/varroactl/AGENTS.md`.
- **`fakemite/`** — synthetic load generator: opens N real mTLS
  `CommandStream`s against a gateway (embedded self-contained NATS+gateway by
  default, or `--gateway ADDR` against a real one) to profile CPU/etcd QPS/
  mTLS handshake storms/shard rebalancing. Never shipped in the product image.
- **`protogen/`** — codegen helper that parses `.proto` files
  (`github.com/emicklei/proto`) and renders Go types via the templates in
  `protogen/templates/` for `internal/mite/proto/mitev1/`. Invoked by
  `make generate-proto`, not run standalone. An `optional` scalar field
  generates a pointer (`optional int32` gives `*int`), because `omitempty`
  drops a zero value and a zero can be a real answer.
- **`bootstrapdeps/`** — repo-maintenance tool (never in the runtime image,
  deliberately not a `varroactl` subcommand) that asserts and records
  `varroa-mite-auth`'s mandatory dependency closure against a resolved plugin
  set. `--resolve` (network) is invoked per profile by
  `hack/gen-plugin-lock.sh` and writes the `bootstrap:` block into
  `internal/controller/pluginlock/lock.yaml`; `--check` (offline) re-verifies
  the committed block in `pr.yaml`. Presence-only — it records declared
  minimums verbatim and never compares versions.

`operator`, `gateway`, `bff`, `mite`, `updatecenter`, `recorder` are all
compiled from the single repo-root `Dockerfile` into one image; `varroactl`
ships as a separate standalone binary (not baked into that image). `protogen`
and `bootstrapdeps` are repo tooling: `go run` only, no Makefile build target,
no image stage.

**Bus credentials and health, shared by `operator`/`gateway`/`bff`/`recorder`:** each takes
`-bus-pass-file`, which `internal/bus` re-reads on every reconnect so a rotated
NATS Secret is followed without a restart. The chart always passes it; the
`BUS_PASSWORD` env is the no-file fallback for local runs. Each passes its
logger in `bus.Config` (never assigned after `Connect`) and registers the
`varroa.bus.connected` gauge. Every long-lived subscription goes through
`bus.Conn.ServeRequests` or `bus.Conn.Listen`, which subscribe again after
NATS denies the subscription at start or on an authorization reload. Operator
readiness (`/readyz`) fails while the bus is down; operator liveness
(`/healthz`) does not, so a bus outage never becomes a restart loop. Gateway and
BFF `/healthz` report a `bus` field but always return 200, because the BFF still
serves the dashboard and the gateway's replicas share one credential.

## Work Guidance

- Keep each binary's top-of-file doc comment current when its responsibility
  boundary shifts — it's the fastest way to answer "does X belong in operator
  or gateway" without reading the whole file.
- A new binary needs a Makefile build target (`build` or `build-cli` pattern)
  and, if it ships in the product image, a `Dockerfile` stage/COPY line.

## Verification

```bash
make build       # compiles bin/varroa-operator, bin/varroa-gateway, bin/varroa-bff, bin/varroa-mite, bin/varroa-updatecenter, bin/varroa-recorder
make build-cli    # compiles bin/varroactl (separate target, not part of `make build`)
make test         # all Go tests, race detector, repo-wide
make lint         # golangci-lint, repo-wide
```

## Child DOX Index

- [mite](mite/AGENTS.md) — the Jenkins-pod sidecar: gRPC mTLS client, command dispatch, drain/CA-trust recovery.
- [varroactl](varroactl/AGENTS.md) — the CLI + MCP surface: cobra command tree, BFF OpenAPI client, login flows.
