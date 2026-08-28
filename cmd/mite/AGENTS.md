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
- **`fingerprint.go`** — live-drift fingerprinting: hashes the currently
  applied JCasC/plugins/items/RBAC and compares against the last
  operator-issued desired-state hash, surfaced via `liveDrift` in heartbeats.
- **`activity_poller.go`** — polls the Jenkins plugin's drain endpoint for
  `IdleGauges` (queue/build activity) on a ticker and caches the latest
  result for `sendHeartbeats` to attach; emits
  `varroa.mite.activity.events.{dropped,forwarded}` metrics.
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

## Verification

```bash
go test -race -count=1 ./cmd/mite/...
```
