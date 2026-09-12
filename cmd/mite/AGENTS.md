# cmd/mite/

## Purpose

The mite sidecar: runs in every Jenkins pod, holds a long-lived gRPC mTLS
`CommandStream` to the gateway (`cmd/gateway`), reports heartbeats/state
snapshots, and applies operator-issued desired-state/imperative commands to
the local Jenkins.

## Ownership

Owns the client-side half of the mite↔gateway protocol, local mTLS identity
lifecycle, Jenkins Bearer-JWT caching, and drain/termination behavior. Does
not own the gRPC server, NATS bridging, or JWT signing — those are
`internal/mite/` (server) and `internal/ca/` (CA), driven by the operator.

## Local Contracts

- **`agent.go`** — the whole agent. Key types/funcs:
  - `Config`/`Agent` (`agent.go:61,73`) — env-driven config
    (`VARROA_ENDPOINT`, `JENKINS_URL`, `CONTROLLER_NAME`, `NAMESPACE`,
    `VARROA_CA_PEM`, optional `BOOTSTRAP_FILE`) read in `main.go`.
  - `Run` (`agent.go:226`) — outer loop: load/create identity once, then
    connect→(bootstrap-register if needed)→openStream→run session, with
    exponential backoff (5s→2m) on any failure — EXCEPT `Unauthenticated`
    register failures, which retry on a fixed 30s cadence and re-read the
    bootstrap token file (`readBootstrapToken`) first: the operator remints
    the Secret on every not-yet-connected reconcile tick (slow plugins-init
    routinely outlives the 15-min TTL), so replaying the startup-cached token
    would wedge registration forever. On a live session it spawns
    `sendHeartbeats`, `startTokenRefreshLoop`, `startHealthProbe`,
    `startObservabilityProbe`, `startFingerprintTicker`,
    `startActivityPoller`, `startPluginInventoryCollector`,
    `startCertRenewalLoop`, and `processCommands`.
  - **Send serialization** — `sendMu` (`agent.go:57`) guards every
    `stream.Send()` call; the heartbeat goroutine and `processCommands`
    (command results, content responses, token-refresh requests) all take it.
  - **Recv/ctx unblocking** — `processCommands` (`agent.go:896`) wraps
    `stream.Recv()` in a goroutine + channel so `ctx.Done()` can break the
    loop without waiting on a blocking Recv; on a Send error `sendHeartbeats`
    calls `a.conn.Close()` to unblock a stuck Recv.
  - **Command dispatch** — `commandMailbox` (`agent.go:834`): desired-state
    commands coalesce into a single replace-on-arrival slot (newest wins,
    never queued); imperative commands go through a bounded (cap 16) FIFO
    that must not silently drop — a full queue ends the stream session.
    `commandWorker` (`agent.go:994`) drains one command at a time so Jenkins
    writes never overlap desired-state and imperative work.
  - **`REPLAY_WEBHOOK` delivery identity** — `runCommand`'s replay case
    sets `X-Varroa-Delivery` from `ReplayWebhookPayload.delivery_id` after
    it copies every header the envelope carries, so a forged inbound
    header under that name cannot survive the replay; it sets nothing
    when `delivery_id` is empty. Correlation with the operator's own
    command tracking still rides `CommandId`, a separate field.
  - **gRPC keepalive** — `connect` (`agent.go:566`) dials with
    `grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30s, Timeout:
    10s})` (`agent.go:598-602`). Must stay compatible with the gateway's
    `KeepaliveEnforcementPolicy(MinTime: 15s)` (`internal/mite/server.go`) —
    a mismatch makes the server send `GOAWAY ENHANCE_YOUR_CALM` and silently
    kill the stream. TLS is fail-closed: `bootstrapTLSConfig`
    (`agent.go:620`) errors out with no `VARROA_CA_PEM` rather than
    connecting insecurely, so a misconfigured pod stalls in the connect
    retry loop instead of trusting an unauthenticated gateway.
  - **mTLS identity + CA-trust recovery** — `loadOrCreateIdentity`
    (`agent.go:340`): a saved cert is gated on
    `certTrustedByCurrentCA()` (`agent.go:437`), not expiry alone. If the
    saved leaf no longer chains to the current `VARROA_CA_PEM` (e.g. a hive
    cluster's control plane was reinstalled and regenerated the CA),
    `discardSavedIdentity()` (`agent.go:459`) wipes cert/key/ca from disk and
    the agent falls through to a fresh bootstrap — otherwise it would loop
    forever on `x509: certificate signed by unknown authority`.
  - **Jenkins Bearer JWT** — cached in-memory only, never on disk;
    `currentJenkinsToken()`/`currentJenkinsTokenExp()` (`agent.go:1702,1709`)
    read it, `startTokenRefreshLoop` (`agent.go:1782`) proactively requests a
    refresh when < 10 min from expiry, `requestTokenRefresh`
    (`agent.go:1804`) sends a `TokenRefreshRequest` and blocks (10s timeout)
    on a `tokenWaiter` until a fresher `TokenGrant` arrives on the stream.
    `jenkins.Client.OnTokenExpired` is wired to trigger the same reactive
    refresh on a 401.
  - **Termination drain** — `drainForTermination` (`agent.go:196`), invoked
    from `main.go`'s SIGTERM/SIGINT handler *before* `ctx` is cancelled:
    quiet-down → poll running builds up to the cached drain timeout → always
    writes the drain-done marker via a top-of-function `defer`, even on an
    early return (no token, zero timeout, quiet-down error), so the Jenkins
    preStop hook is never left hanging. `drainMu` serializes this against the
    SAFE_RESTART drain path in `commandWorker` so the two never race on the
    same Jenkins.
- **`main.go`** — flag/env parsing, logger + telemetry init, `Agent`
  construction, and the SIGTERM/SIGINT→drain→cancel sequencing described
  above.
- **`loopback.go`** — the HTTP listener the Jenkins plugin talks to,
  `127.0.0.1:9095` (`MITE_LOOPBACK_PORT` overrides), started and stopped
  alongside the agent run loop. Three core routes, plus four cross-tenant
  routes below: `POST /v1/events` forwards one
  or more `BuildEvent`s as `MiteMessage.build_event`, `POST /v1/claim`
  forwards one `ClaimRequest` as `MiteMessage.claim_request` and blocks for
  the reply, `POST /v1/log` (`handleLog`) reads nine `X-Varroa-*` headers
  (`parseLogChunkHeaders`) plus the raw body, stamps the cell's own pod
  identity onto `LogChunk.cell_instance` (the plugin never sends one), and
  forwards it as `MiteMessage.log_chunk`. Every request carries
  `Authorization: Bearer <token>`, checked
  with `crypto/subtle.ConstantTimeCompare` against the token the mite wrote
  to `/var/run/varroa/loopback-token` before the listener opened — the mite
  never re-reads that file itself; it is a write-once token for the life of
  the process, and the plugin is the side that re-reads on a `401`. The
  waiter map (`claimWaiterMap`, at most 64 concurrent entries) resolves a
  blocked `/v1/claim` call by `request_id` when `processCommands` reads the
  matching `OperatorMessage.claim_response`; a 65th concurrent claim gets
  `429` with no send, and stream teardown closes every open waiter so none
  leaks. The retained-event map (keyed by `run_id`/`event_seq`, bounded by
  `VARROA_EVENT_RETAIN_BYTES`) replays in order after every reconnect and
  releases an entry only on a matching `event_ack`. Metrics:
  `varroa.mite.claims.{granted,duplicate,vetoed,failed}` and
  `varroa.mite.build_events.{forwarded,acked,rejected,retained_bytes}`.
  - **The log retention buffer and `/v1/log` back pressure** — mirrors the
    event retention pattern above, at `VARROA_MITE_LOG_RETAIN_BYTES`
    (default 64 MiB, `logRetentionStore`). `handleLog` answers `503` once
    the bound is reached, and again while a per-run `BUSY` pause window from
    `handleLogAck` is open, so the plugin's own retry-with-backoff carries
    the back pressure with no new plugin rule. `handleLogAck`
    (`OperatorMessage.log_ack`) releases every retained chunk of a run at or
    below `ack_seq` on `STORED` or `REJECTED`, and re-arms the pause window
    on `BUSY`. Every retained chunk re-sends, in `seq` order, on the next
    stream open — a mite restart loses the buffer, and the gateway detects
    the resulting sequence gap.
  - **Cross-tenant routes** — four more routes on the same listener, gated
    by the same bearer token: `POST /v1/cross-tenant/build` accepts kind
    `build`; `.../status` accepts `status` and `last_successful`;
    `.../artifact` accepts `artifact_list` and `artifact_url`; `.../lock`
    accepts `lock_acquire`, `lock_renew`, and `lock_release`. Each route
    answers the response's `payload_json` as the HTTP body (the flat
    recorder fields), never the transport envelope. A `kind`
    outside a route's own set, or outside the eight allocated values, is
    refused with `400` before the request forwards as
    `MiteMessage.cross_tenant_request`. `crossTenantWaiterMap` is a
    separate type from `claimWaiterMap` (they key on different id types)
    but shares its shape and its 64-entry cap
    (`maxCrossTenantWaiters = maxClaimWaiters`); a 65th concurrent request
    gets `429` with no send, and stream teardown releases every open
    waiter. Every mite-generated failure (400/401/413/429/503/504)
    answers `{"code":"UNAVAILABLE"}` in `payload_json`, since
    `CrossTenantResponse` carries no dedicated code field.
    `crossTenantMiteTimeoutFromEnv` resolves
    `VARROA_CROSSTENANT_MITE_TIMEOUT_MS` (default 9500ms, clamped to the
    listener's own 10s write timeout) independently of
    `internal/mite.CrossTenantTimeoutFromEnv` — `cmd/mite` does not import
    `internal/mite`, so the two budgets are separate parsers with the same
    rule, not one shared function.
  - **`handleTailControl`** applies one `OperatorMessage.tail_control`: it
    calls the Jenkins plugin's `/varroa-log/tail` root action with the
    cached Jenkins bearer JWT, then answers `MiteMessage.tail_ack` with the
    same `control_id`. It runs in its own goroutine so a slow local Jenkins
    call never blocks `processCommands`' read loop from dispatching the next
    message.
- **`fingerprint.go`** — live-drift fingerprinting: hashes the currently
  applied JCasC/plugins/items/RBAC and compares against the last
  operator-issued desired-state hash, surfaced via `liveDrift` in heartbeats.
- **`activity_poller.go`** — polls the Jenkins plugin's drain endpoint for
  `IdleGauges` (queue/build activity) on a ticker and caches the latest
  result for `sendHeartbeats` to attach; emits
  `varroa.mite.activity.events.{dropped,forwarded}` metrics.
- **`job_schedule_poller.go`** — `startJobSchedulePoller` polls
  `GET /varroa-activity/schedules` every `jobSchedulePollInterval` (10
  minutes) and once immediately on every stream (re)connect, caching the
  decoded set for `sendHeartbeats` to attach as `StateSnapshot.job_schedules`.
  The hash gate: `sendHeartbeats` fills that field only when the cached
  set's hash (`computeJobScheduleHash`, over `jobPath`/`triggerType`/
  `spec`/`timezone`/`disabled`/`upstreamTenant`/`upstreamJob`/
  `upstreamResult`, sorted, excluding the fire times) differs
  from the hash of the last sent set, or the stream reconnected since the
  last send, or `jobScheduleResendInterval` (30 minutes) has passed. Every
  other heartbeat leaves the field unset. A poll failure keeps the last
  known set and hash in place and retries on the next tick. A row's
  `upstreamTenant`/`upstreamJob`/`upstreamResult` (empty for every row but
  a `varroa-upstream` trigger) carry through to the matching
  `mitev1.JobSchedule` fields unchanged, so an edit to just those three
  values still reopens the send gate.
  - **Plugin inventory collector** — `startPluginInventoryCollector`
    (`agent.go`): runs on the heartbeat tick, with its own timeout so a wedged
    Jenkins never blocks the heartbeat send path. Collects via
    `plugininv.CollectSelection` (API first, filesystem fallback), latches
    into an `atomic.Pointer[plugininv.Inventory]`, and pushes the full
    inventory via `MiteMessage_PluginInventory` only when the hash, source, or
    collection-failure state changes. Resend guard (20× heartbeat interval)
    prevents push storms on stream flap. `COLLECT_PLUGIN_INVENTORY` imperative
    command forces a fresh collection and unconditional push. A degradation
    ladder drops optional edges, then all edges, when the marshalled payload
    exceeds the NATS budget (~900 KiB).
- **`rehydrate.go`** — applies an operator-sent `RehydrationBundle` under
  `JENKINS_HOME`. `handleDesiredState` (`agent.go`) tracks it as the fourth
  desired-state section, alongside `config`, `rbac`, and `items`: it runs
  after config/RBAC and before items, so a job the items section is about to
  touch is already seeded, and reloads every `ReloadItems` path after the
  items apply completes. `checkRehydrationPath` accepts only the
  `jobs/<seg>/.../builds/<n>/{build.xml,changelog0.xml}` and
  `jobs/<seg>/.../nextBuildNumber` path shapes, confines the resolved path
  under `JENKINS_HOME`, and refuses a symbolic link anywhere in its parent
  chain. `applyRehydrationBundle` inflates each file's gzip content against
  its declared `raw_size` with a bounded reader, so a mismatched size can
  never force an unbounded inflation; it defers a job whose `config.xml`
  does not exist yet, never overwrites an existing build directory, and
  never lowers a stored `nextBuildNumber`. A deferred job's path returns in
  `DesiredStateResult.RehydrationDeferredJobs` for the operator to retry.

## Verification

```bash
go test -race -count=1 ./cmd/mite/...
```
