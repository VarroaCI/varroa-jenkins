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
  replica. No gRPC/OIDC.
- **`gateway/`** (`bin/varroa-gateway`) — terminates mTLS/gRPC from mite
  sidecars on `:9090` and bridges each `CommandStream` to NATS
  (`mite.<cluster>.<ns>.<name>.{in,out,content}`). Loads the shared CA from the `varroa-ca`
  Secret. Also serves an apikey-verify HTTP endpoint (default `:9092`). Does
  not reconcile controllers.
- **`bff/`** (`bin/varroa-bff`) — Backend-For-Frontend; REST API + SSE on
  `:8080`. Stateless — mite telemetry comes from the NATS bus, OIDC JWT needs
  no server-side session. Multiple replicas run behind a load balancer
  (leader election only for the parts that must be singleton). Wires
  `Deps.FleetPluginInventory` via `api.NewFleetInventoryReader(miteTransport)`.
- **`updatecenter/`** (`bin/varroa-updatecenter`) — opt-in in-cluster Jenkins
  update center on `:8080` (`VARROA_UC_LISTEN`); thin main over
  `internal/updatecenter`, configured entirely via `VARROA_UC_*` env vars
  (storage local|oci, pull-through, import token, declared-plugins file, upload
  cap, single-writer gate). Serves
  `update-center.actual.json` + plugin `.hpi` blobs from an `internal/oci`
  BlobStore; local storage mode is single-replica (PVC), oci mode is stateless.
  Built by `make build` into the same product image as the other backends.
- **`mite/`** (`bin/varroa-mite`) — sidecar injected into every Jenkins pod;
  own contract in `cmd/mite/AGENTS.md`. Reports health, state, observability,
  and full plugin inventories; applies desired-state and imperative commands.
- **`varroactl/`** (`bin/varroactl`) — CLI + MCP surface; own contract in
  `cmd/varroactl/AGENTS.md`.
- **`fakemite/`** — synthetic load generator: opens N real mTLS
  `CommandStream`s against a gateway (embedded self-contained NATS+gateway by
  default, or `--gateway ADDR` against a real one) to profile CPU/etcd QPS/
  mTLS handshake storms/shard rebalancing. Never shipped in the product image.
- **`protogen/`** — codegen helper that parses `.proto` files
  (`github.com/emicklei/proto`) and renders Go types via the templates in
  `protogen/templates/` for `internal/mite/proto/mitev1/`. Invoked by
  `make generate-proto`, not run standalone.
- **`bootstrapdeps/`** — repo-maintenance tool (never in the runtime image,
  deliberately not a `varroactl` subcommand) that asserts and records
  `varroa-mite-auth`'s mandatory dependency closure against a resolved plugin
  set. `--resolve` (network) is invoked per profile by
  `hack/gen-plugin-lock.sh` and writes the `bootstrap:` block into
  `internal/controller/pluginlock/lock.yaml`; `--check` (offline) re-verifies
  the committed block in `pr.yaml`. Presence-only — it records declared
  minimums verbatim and never compares versions.

`operator`, `gateway`, `bff`, `mite` are all compiled from the single
repo-root `Dockerfile` into one image; `varroactl` ships as a separate
standalone binary (not baked into that image). `protogen` and `bootstrapdeps`
are repo tooling: `go run` only, no Makefile build target, no image stage.

## Work Guidance

- Keep each binary's top-of-file doc comment current when its responsibility
  boundary shifts — it's the fastest way to answer "does X belong in operator
  or gateway" without reading the whole file.
- A new binary needs a Makefile build target (`build` or `build-cli` pattern)
  and, if it ships in the product image, a `Dockerfile` stage/COPY line.

## Verification

```bash
make build       # compiles bin/varroa-operator, bin/varroa-gateway, bin/varroa-bff, bin/varroa-mite
make build-cli    # compiles bin/varroactl (separate target, not part of `make build`)
make test         # all Go tests, race detector, repo-wide
make lint         # golangci-lint, repo-wide
```

## Child DOX Index

- [mite](mite/AGENTS.md) — the Jenkins-pod sidecar: gRPC mTLS client, command dispatch, drain/CA-trust recovery.
- [varroactl](varroactl/AGENTS.md) — the CLI + MCP surface: cobra command tree, BFF OpenAPI client, login flows.
