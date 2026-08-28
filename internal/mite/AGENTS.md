# AGENTS.md — internal/mite

## Purpose

The mite gRPC service: the mTLS control channel between each Jenkins pod's
mite sidecar and Varroa's control plane. Owns registration/cert issuance, the
bidirectional `CommandStream`, two pluggable stream backends, bootstrap-token
signing/replay protection, and the Jenkins-facing JWT signer.

## Ownership

- Owns: `Server` gRPC wiring (`server.go`), the `StreamHandler` interface and
  its two impls (`registry_handler.go`, `bus_handler.go`), the in-memory
  `Registry` (`registry.go`), the mTLS auth interceptor (`auth.go`), bootstrap
  `TokenSigner` + replay stores (`token.go`, `consumed_token.go`,
  `consumed_token_kv.go`), and `MiteTokenSigner` (`jenkinstoken.go`).
- Does not own: cert issuance internals (`internal/ca`), NATS bus primitives
  `BusHandler` bridges into (`internal/bus`), or the operator reconciler that
  decides desired state and writes the `mite_desired` KV key
  (`internal/controller`). `proto/mitev1/` is generated protobuf, lint-excluded
  — a wire-contract boundary, not for hand-editing.

## Local Contracts

- **`Server`** (`server.go`) — run by `cmd/gateway` on `:9090` (also embedded
  by the BFF for local-registry mode). `NewServer` issues its own cert via
  `ca.CA.IssueServerCert`, requires client certs, installs `AuthInterceptor` as
  a unary interceptor (`CommandStream` does its own inline auth since it's
  streaming). Keepalive: `EnforcementPolicy{MinTime:15s}` +
  `Params{Time:30s,Timeout:10s}` — must stay compatible with the mite client's
  30s ping in `cmd/mite/agent.go`, or the server sends `GOAWAY
  ENHANCE_YOUR_CALM` and silently kills the stream. `streamHandler` is
  pluggable (nil → `RegistryHandler`); `TokenGrantFunc`, if set, answers
  `TokenRefreshRequest` synchronously (BFF mode) instead of only forwarding it.
- **`Register` RPC** (`server.go:141`) — first-time: validates bootstrap HMAC
  token + `ConsumedTokenStore.Consume` (single-use); renewal: verifies existing
  client cert CN. Either way calls `ca.CA.IssueMiteCert`. `ServerEndpoint`
  defaults to `varroa-varroa-gateway.varroa-system.svc.cluster.local:9090`.
- **`CommandStream`** (`server.go:209`) — first message must be `Hello`;
  identity comes from the mTLS leaf CN (`controllerName.namespace`, via
  `parseCN`, verified against the CA — never trusts `PeerCertificates[0]`
  directly). Read loop type-switches `MiteMessage` variants out to the active
  `StreamHandler`; sends go through one `sendMu`-guarded closure. Before each
  dispatch it calls `StreamHandler.IsCurrentConnection` with the token
  `OnConnect` returned and ends the stream with `codes.Aborted` once superseded
  — one central gate, rather than a token threaded through every inbound
  handler method. Without it a stream the mite already replaced keeps
  dispatching and writes a dead connection's version, health, idle gauges or
  plugin inventory into state the replacement owns (#514). The gate is a check,
  not a reservation — a supersede landing between it and the handler's write
  still lets that one message through, and a superseded stream that goes silent
  never reaches the gate at all — it writes nothing, but its transport lives
  until the peer closes it or the transport fails (keepalive only reaps a peer
  that stops answering pings). Reserving would mean holding a per-controller lock across
  the handler's NATS publish and KV `Put`, which the `presenceLocks` rule under
  Work Guidance rules out. The deferred `OnDisconnect` is token-guarded and
  no-ops for that stream.
- **`StreamHandler`** (`stream_handler.go`) — `RegistryHandler` backs the
  in-memory `Registry` (BFF/single-process; `OnContentResponse`/
  `OnJenkinsActivity` are no-ops — no bus to route to). `BusHandler` backs the
  gateway: bridges every event to NATS via `internal/bus` helpers. Command
  delivery is **last-value KV** (`watchDesiredState` watches `mite_desired`,
  never replays stale commands). Every bus→stream watcher establishes through
  `retryEstablish` — capped exponential backoff, retried for the life of the
  connection, never abandoned. Returning after a single setup error silently
  starves a connected mite of all desired state (#509); while a watch is down
  the controller is marked degraded in its `bus.Presence` record, which the
  operator surfaces as condition `MiteStreamDegraded`. Each watcher also
  re-establishes on *post-setup* death — `Updates()` closing, a lost durable
  consumer, a dead content subscription — rather than returning; the degraded
  mark is per watch kind, so a healthy watcher cannot clear another's failure.
  `OnConnect` returns a connection token that `OnDisconnect` must pass back: a
  superseded stream can tear down *after* the mite reconnected, and an
  unqualified teardown cancels the live connection's watch goroutines — the
  same starvation, reached from the other side. The same token also gates
  inbound dispatch through `IsCurrentConnection` (`Registry.IsCurrentEpoch` for
  `RegistryHandler`); both treat a zero token and an unclaimed key as current.
  Imperative one-shots (safe-restart, webhook
  replay) instead ride a durable JetStream pull consumer
  (`imp-<cluster>-<ns>-<name>`), acked only after a matching `CommandResult`
  (`pendingAcks`) so a gateway restart redelivers unfinished ones.
  `OnPluginInventory` publishes the inventory to `MiteInSubject` and writes it
  to KV under `bus.PluginInventoryKey`, mirroring `OnObservabilityReport`.
  `OnDisconnect` does **not** delete the desired-state KV key — only
  operator-side `ClearDesired` on Controller deletion does. `REPLAY_WEBHOOK`
  results route to `WebhookResultSubject`, bypassing the reconciler's shared
  result buffer. `watchContent` bridges synchronous content-fetch with a 15s
  TTL sweep (`contentSweeper`) against orphaned replies.
- **`Registry`** (`registry.go`) — keyed `"namespace/controllerName"`; each
  `Connection` has a monotonic `Epoch` (bumped every `Register`, detects stale
  reconnect races), a 32-buffered `Results` channel (`OnCommandResult`
  drops+logs on full rather than blocking the reader), an
  `InstalledPluginsHash` latched from heartbeats (never cleared by snapshot
  heartbeats — `UpdateHeartbeat` only writes it when non-empty), and a
  `PluginInventory` cached from `SetPluginInventory`. `Unregister` retains
  the record (clears `Stream`/`Conn` only) so `ObservabilityReport` and
  `PluginInventory` survive disconnect.
- **Bootstrap tokens** (`token.go`) — HMAC-SHA256 v2:
  `"v2"\x00jti\x00controller\x00namespace\x00expiry`+sig, base64-RawURL.
  `IsCurrentTokenFormat` is structural-only, used by the operator to decide
  rotation, never for auth. Single-use: `inMemoryConsumedStore` (per-replica,
  1-min sweep, default) or `kvConsumedStore` (JetStream KV, cross-replica,
  fail-open on KV error other than `ErrKVKeyExists`). Swap via
  `Server.SetConsumedTokenStore` before serving.
- **`MiteTokenSigner`** (`jenkinstoken.go`) — wraps `internal/signing.Signer`
  (RSA-2048, stable KID). `GenerateMiteJenkinsToken` issues an RS256 JWT
  (`iss: varroa-operator`, `sub: system:varroa-mite`, `aud: <ns>/<name>`) the
  mite presents to Jenkins; `VarroaSecurityRealm` (`plugin/`) verifies offline
  against the operator public key — no Dex round-trip. Distinct from the HMAC
  `TokenSigner` by design: different trust direction.
- **`AuthInterceptor`** (`auth.go`) — skips `/mitev1.Mite/Register` (self-auth).
  `parseCN` splits on the **last** `.` so dotted controller names parse right.

## Work Guidance

- New `CommandStream` message variants need a case in **both**
  `StreamHandler` impls plus `noopHandler` — a missing case compiles fine but
  silently drops behavior. Current handlers: `OnConnect`, `OnHeartbeat`,
  `OnSnapshot`, `OnCommandResult`, `OnTokenRefreshRequest`, `OnObservabilityReport`,
  `OnContentResponse`, `OnJenkinsActivity`, `OnPluginInventory`,
  `IsCurrentConnection`, `OnDisconnect`.
- `BusHandler` per-connection maps (`cancels`, `certExpiry`, `pendingAcks`,
  `replayCmds`, `pendingContent*`, `watchDegraded`, `miteVersion`) are all keyed
  `"ns/name"` under one `mu`; follow that pattern rather than adding a second
  mutex. The one exception is `presenceLocks` (`"ns/name"` -> `*sync.Mutex`),
  which guards no state — each entry orders that controller's presence KV
  *write* against the `OnDisconnect` *delete*. `putPresence` must not hold `mu`
  across that network round trip (it is on the heartbeat path for every
  controller on the gateway), so without a separate lock the connected-check
  and the `Put` straddle a window in which a departed mite gets resurrected as
  connected. Keyed, not handler-wide, so one slow JetStream write cannot stall
  every other controller's heartbeat. Guard I/O ordering there, never map state.
- `publishBroodEvent` maps brood events to activity events through
  `activityMessageForBroodEvent`, which admits lifecycle **transitions only**
  (`connected`/`disconnected`). Heartbeats and snapshots are per-interval
  presence telemetry carried by the raw brood event on `events.brood.>`;
  publishing them on `activity.*` floods the bounded audit stream and evicts
  real audit history — do not add new periodic event types to that switch.
- Wire changes go in `proto/mitev1/mite.proto` then regenerate; never hand-edit
  `mitev1_gen.go`.

## Verification

```bash
go test -v -race -count=1 ./internal/mite/...
make test
make lint
```

Key tests: `gateway_integration_test.go` (end-to-end mite↔gateway↔bus),
`superseded_stream_test.go` (reconnect races: dispatch gate + token/epoch checks),
`jenkins_activity_test.go`, `registry_test.go`, `token_test.go`,
`jenkinstoken_test.go`, `consumed_token_kv_test.go`.
