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
  `consumed_token_kv.go`), `MiteTokenSigner` (`jenkinstoken.go`), and the
  build-event/claim bridge (`build_events.go`, `claim_bridge.go`) behind
  `StreamHandler.OnBuildEvent`/`OnClaimRequest`, and the cross-tenant
  bridge (`crosstenant_bridge.go`, `crosstenant_timeout.go`) behind
  `StreamHandler.OnCrossTenantRequest`.
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
  plugin inventory into state the replacement owns. The gate is a check,
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
  starves a connected mite of all desired state; while a watch is down
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
  replay, build trigger, build cancel) instead ride a durable JetStream pull
  consumer (`imp-<cluster>-<ns>-<name>`), acked only after a matching
  `CommandResult` (`pendingAcks`) so a gateway restart redelivers unfinished
  ones. Each `pendingAcks` entry carries the connection token that received it,
  and `OnDisconnect` **defers** `releasePendingImperatives` to nak that token's
  entries. That release moves the bookkeeping under `h.mu` and naks after the
  unlock, because `h.mu` is on every controller's heartbeat path; the ordering
  survives, because the pump's cancel already ran under `h.mu`. Deferred, so it covers the superseded-stream path the early return
  skips, and so the nak lands after this stream's pump is cancelled. A pod roll
  often reconnects the mite before the old stream tears down, so a release
  placed after the superseded check never runs and the command waits out the
  consumer's 30-second ack timeout instead. The token scope is what makes the early
  release safe: `pendingAcks` is keyed by controller, so an unscoped nak would
  also redeliver a command the replacement connection already runs.
  `forwardImperative` (one fetched message) refuses a cancelled context and
  naks it, and it re-reads the context **under `h.mu`** before it files the
  ack, because `OnConnect` and `OnDisconnect` both cancel a pump while they
  hold that lock — the same rule `markWatchDegraded` follows. A nak makes the
  message available at once, so the dying pump's own outstanding fetch can
  receive back the command the disconnect just released; `server.go`'s deferred
  `OnDisconnect` still holds an open gRPC transport, so that send can even
  succeed. A redelivered trigger carries the same run id and
  intent key, so the run claim keeps it to one build.
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
- **Build-event/claim bridge** (`build_events.go`, `claim_bridge.go`) —
  `BusHandler.OnBuildEvent`/`OnClaimRequest` are the two `StreamHandler`
  methods this bridge adds. `OnBuildEvent` maps one inbound `BuildEvent` onto
  one or more of C1's ingest envelope kinds and only enqueues onto a
  per-connection publish worker (`newBuildEventWorker`, started in
  `OnConnect`, stopped in `OnDisconnect`); the `CommandStream` read loop in
  `server.go` never publishes inline, because one `COMPLETED` event can
  expand into many stage envelopes. The worker publishes every kind but
  `HEARTBEAT` synchronously and retries a rejected publish forever; a
  `HEARTBEAT` publish is the one droppable kind. A full queue makes
  `OnBuildEvent` wait up to `VARROA_EVENT_PUBLISH_WAIT` (default 10s) then
  return an error, which ends the stream so the mite reconnects and replays
  from its own retention — this package declares no drop path for anything
  else. `OnClaimRequest` runs each claim on its own goroutine, capped at 32
  in-flight per controller connection epoch (never by controller alone, so a
  goroutine that outlives its stream cannot underflow a replacement
  connection's counter), and requests C1's `bus.LedgerClaimSubject` with the
  `VARROA_LEDGER_TIMEOUT_MS` budget (default 4s). The ledger body copies
  the mite request's `origin` as it is; the recorder marks a vetoed run
  only when the origin is `VARROA_INTENT`. Both methods read
  `cell_instance` from the registered identity, never from the event or
  request payload. Both also resolve `tenant` (and, for a claim,
  `placement_generation`) through `CellTenantResolver.ResolveJobTenant`,
  keyed on the event's or the claim's own job path — one cell hosts many
  tenants. An unresolved tenant is an admission refusal, not a retry: a
  build event is dropped and, for anything but a `HEARTBEAT`, acked
  `REJECTED`; a claim answers at once with the ledger's unavailable code
  and makes no ledger request. `respondClaimUnavailable` logs every
  unavailable answer at `WARN` with `request_id`, the controller, and the
  error text; it is the one gateway record of a failed claim. The bridge copies
  the ledger reply's `home_slot` onto `ClaimResponse.home_slot` unchanged: a
  `NOT_HOME` veto names the job's home slot, slot 0 included, and every other
  reply carries none. `startedPayload`/`completedPayload`
  (`build_events.go`) copy `BuildEvent`'s four template-resolution fields
  (`template_name`, `template_version`, `marker_path`, `marker_revision`)
  onto the `run.started`/`run.completed` payload via `addTemplateFields`,
  omitting any field the event does not carry; a `HEARTBEAT` event never
  carries them. `startedPayload` alone writes `replay_of`, and only for a
  non-empty value, so no other phase and no ordinary build sends the key. A `COMPLETED` event publishes `run.tests` when it carries a
  test counter, a test case, or a test report, so a report whose case count
  is zero still reaches the store. `shrinkToBudget`'s `cases` rung runs after
  the `error` truncation and drops the **last** case, because the plugin
  sends the cases in priority order.
- **Command outcomes** (`bus_handler.go`, `build_events.go`) — a
  `CommandResult` for a pending imperative publishes one envelope on the
  same `builds.<cluster>.<ns>.<cell>` subject the build events use.
  `recordCommandOutcome` decodes the pending message once through
  `pendingCommand` (no second map) and switches on the command type.
  `TRIGGER_BUILD` publishes `run.trigger_failed` when `success` is false,
  or `run.trigger_accepted` when `success` is true. `RESUME_BUILD`
  publishes `run.resume_failed` for a failed result and nothing for a
  successful one, because a resumed build proves itself by its own
  heartbeat. `REPLAY_BUILD` publishes nothing and logs a refused result,
  because a refused replay leaves no run to record. **A resume result always acks, whatever the publish
  answers**, which is the opposite of the trigger rule below: a
  redelivered resume can succeed while the recorder holds no dispatched
  resume, and only the named target's heartbeat clears a resume record,
  so that live build would read `INTERRUPTED` at the arm window with no
  cancel. A lost publish leaves the run on its 6-minute resume deadline
  instead. `publishResumeFailed` maps the mite's own error text onto one
  of six reason codes with `reasonCodeForResumeError`, which tests
  `RESUME_LOAD_FAILED` and `RESUME_NOT_BUILDING` **first** so a deadline
  that wraps a load failure still reads as the load failure and the
  recorder keeps the dispatch. The token test is bounded (`token + ": "`,
  `hasResumeToken`), never a plain substring: a Jenkins folder may carry
  a token in its own name, and the mite writes a job path into the text
  of an unnamed failure. Every publisher resolves the tenant through
  `CellTenantResolver.ResolveJobTenant`.
  `publishTriggerFailed` maps `CommandResult.http_status` onto a reason code
  with `reasonCodeForStatus` (`404` to `JOB_NOT_FOUND`, `409` to
  `JOB_NOT_BUILDABLE`, everything else and `0` to `TRIGGER_REJECTED`);
  `publishTriggerAccepted` carries no reason code and no error, only
  `http_status` and `actor`, because Jenkins accepted the trigger. **Neither
  publisher may consult `job_schedules.go`'s stream-missing negative
  cache.** That cache is armed by a publish failure, so it cannot tell a
  missing stream from a transient one; an armed cache would acknowledge the
  redelivery of a command whose first publish failed and lose the record.
  The gateway reads `recordStoreEnabled` instead, which `cmd/gateway` sets
  once through `SetRecordStoreEnabled`, from the **opened blob store** and
  never from `VARROA_RECORDSTORE_DSN`: the chart gives the gateway that DSN
  only in postgres blob mode, and `VARROA_BLOBSTORE_URL` in every mode, so a
  DSN-based flag is false on an s3-mode install whose record store is
  enabled. With no record store the gateway publishes nothing, counts
  nothing, and acknowledges. With one, **only the publish call makes the
  pending ack conditional**: any publish error leaves the imperative
  unacknowledged, so the stream redelivers it and the next result publishes.
  An unresolved tenant, an unparsable pending message, and a marshal fault
  all still ack, because a redelivery would trigger the same build again and
  change nothing.
- **Cross-tenant bridge** (`crosstenant_bridge.go`, `crosstenant_timeout.go`) —
  `BusHandler.OnCrossTenantRequest` relays a `varroa.build`/`lastSuccessful`/
  `copyArtifact`/`lock` step call to the record store's
  `bus.LedgerCrossTenantSubject`, for the closed set of eight `kind` values
  design.md D4 defines. It rejects an unknown kind and enforces a per-stream
  cap of 8 in-flight requests (`crossTenantCounts`, cleared on disconnect the
  same way `claimCounts` is) before it ever starts a goroutine or calls NATS.
  The request's JSON payload is decoded only far enough to read
  `caller_job_path`; `CellTenantResolver.ResolveJobTenant` on that path then
  overwrites `cluster`, `namespace`, `cell`, `caller_tenant`, and
  `placement_generation` — whatever the cell sent — plus `kind` from the
  validated proto field. An unresolved caller tenant fails closed with no
  ledger request, the same rule `OnClaimRequest` follows. The reply is never
  parsed: `payload_json` carries the record store's raw JSON body straight
  through, because this package relays a contract it does not own.
  `CrossTenantTimeoutFromEnv` is the whole-number-milliseconds parser
  behind this package's own `VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS` budget
  (default 9s); it warns once per call when the resolved value is at least
  as long as the mite's 9.5s default, because that leaves the mite's whole
  budget consumed before this hop even gives up. `cmd/mite` never imports
  this package — the gateway server would pull into the sidecar binary —
  so its own hop (`VARROA_CROSSTENANT_MITE_TIMEOUT_MS`) keeps a second,
  independent copy of the same parsing rule in `cmd/mite/loopback.go`.
- **`CellTenantResolver`** (`tenant_resolver.go`) — the production
  implementation `NewCellTenantResolver` builds, wired by `cmd/gateway`.
  It reads the owning `Controller` (namespace/cell from the mTLS
  identity) to decide `spec.cell.enabled`, and — only for a cell — the
  named `Team`'s `status.placement`, each cached 30 seconds
  (`cellTenantResolveTTL`). A non-cell controller always resolves to the
  permanent `<namespace>/<cell>` placeholder. A cell resolves the first
  path segment of the job path to a `Team` only when **some member of
  that Team** names this exact `(namespace, cell)`: the entry caches the
  whole `status.placement.members` cell set, so a cell holding slot 1 or
  a standby resolves too. The claim fence, not the gateway, decides a
  job's home and answers `NOT_HOME`. A placement with an empty member
  list falls back to the scalar `cell`. A Team that holds no member on
  this cell, an unplaced Team, and a read error all resolve to the empty,
  unknown tenant, never a guess. `job_schedules.go`'s
  `ResolveJobTenant` calls already existed before this resolver did; they
  needed no change to pick it up.
- **Log blob writer** (`log_writer.go`) — `BusHandler.OnLogChunk` appends one
  console-log chunk to C1's blob store (`logs/<run_id>`) and its per-chunk
  index (`logindex/<run_id>`), then acks only once both writes land durably;
  `RegistryHandler.OnLogChunk` is a no-op (no blob store in that mode). Each
  run gets a `runLogState`, drained by `drainRun` one chunk at a time so an
  append and its index entry always land together. `sendLogAck` answers
  `STORED`/`BUSY`/`REJECTED` (`OperatorMessage.log_ack`); a rejection sets
  one of the reason tokens `empty-run-id`, `finalize-deadline`, or
  `foreign-run`. `VARROA_GATEWAY_LOG_PENDING_BYTES` (default 2 GiB) bounds
  total pending bytes across every run; over the bound `OnLogChunk` answers
  `BUSY` with a jittered pause instead of blocking the read loop.
  `checkOwner` (`log_owner.go`) verifies a chunk's `(cluster, namespace,
  cell, cell_instance)` against C1's ledger over the `varroa.ledger.owner`
  request-reply subject, cached 30 minutes on a match and 60 seconds on a
  refusal (`ownerCache`, bounded LRU) — a foreign or unverifiable run is
  rejected, never silently accepted. `startHeartbeat`
  (`logHeartbeatInterval`, 30s) keeps a terminal run's pending chunks
  draining after the mite stream that sent them may already be gone, until
  `VARROA_RECORDSTORE_LOG_FINALIZE_WAIT` (default 60s) passes with chunks
  still unstored, at which point `abandonRun` rejects the rest with reason
  `finalize-deadline`. `markRunTerminal` starts the heartbeat before it
  schedules the deadline, and the deadline — inline for an anchor already
  past it, or from the `time.AfterFunc` timer otherwise — always runs
  through `runDeadlineFired`, never a bare `cancel()`: besides cancelling
  `appendCtx`, it calls `abandonRun` itself whenever the run is idle (no
  drain goroutine running) with a pending entry or an open hole, because a
  run that goes idle right after a hole opens and never gets another
  chunk has no other path to abandonment — the drain goroutine that would
  otherwise notice the deadline already exited. `abandonRun` is
  idempotent (an `st.rejectedForever` check and the flag it guards share
  one critical section), so the timer's idle-path call and a concurrent
  drain-triggered call from `appendChunk` returning `appendDeadlineHit`
  never both act; `drainRun`'s post-append dequeue re-checks
  `rejectedForever` too, so an abandon that lands between a chunk's
  successful append and its dequeue never slices an already-cleared
  queue. `BuildLogManifest` (`log_manifest.go`) is the
  computed-never-stored view a reader calls: it walks the index, marks any
  byte range the index does not cover as a hole, and reports `incomplete`
  with a reason token (`no-summary` when the run carries no `LogSummary` at
  all) — it is never itself written back to storage.
- **Run-state store** (`run_state.go`) — `BusHandler.OnRunStateChunk` stores
  one replicated file of a running Pipeline build as one whole blob object
  (`runstate/<run_id>/<pusher_epoch>/<state_gen>/<relpath>`, class `state`),
  through `blob.Store.Replace`. **The key carries the pusher epoch and the
  push generation, and that pair is what orders two writes.** The chart runs
  two gateway replicas, each with its own `stateRuns` map: a superseded
  process reconnecting through the other replica would otherwise land its
  `Replace` on the live object, and the 30-second refresh repairs only
  `program.dat` and `build.xml`. A `workflow/<n>.xml` and a node config go
  once per process, so a stale overwrite of either lasts for the run. **A
  gateway rollout leaves two replicas that forward chunks of one process**,
  which the epoch alone cannot separate: both chunks name that process, so
  the generation is what keeps those two writes on two keys.
  It applies six ordered steps: a path check, the same `checkOwner` a log
  chunk takes, the 64 MiB per-run bound against the chunk's declared
  `object_size`, a buffer keyed `(run_id, relpath, state_gen)`, a write only
  once every sequence is present, and one `run.state` publish. Each chunk is
  gzipped on its own, so a missing middle chunk would inflate into a
  truncated file: the gateway drops the buffer and answers `incomplete`
  instead. It charges one amount per **completed object**, never per chunk,
  because a push replaces a whole object. The per-run entry is dropped on a
  terminal owner tuple and on `OnDisconnect`; without both drops an entry
  lives for the gateway's whole lifetime. The entry also remembers the
  push it last stored per path, keyed on the chunk's `pusher_epoch` and
  its generation, so a chunk repeating both answers stored instead of
  opening a fresh buffer and reading as `incomplete`. The epoch is part
  of that identity because a generation counts inside one Jenkins
  process: a restart starts it again at 1 while the entry lives on, and
  an identity a restart could repeat would freeze the run's stored state
  at its pre-restart copy. The entry also holds the newest epoch it has
  seen for the run: a **lower** epoch is a chunk a retired Jenkins
  process sent, delivered late, and it is discarded rather than allowed
  to drop the live buffer or overwrite the stored object. It is answered
  as stored, because the sender is gone and the loopback's reason set is
  closed. Only a higher epoch replaces, and a rise retires every buffer
  below it so their bytes leave the gateway-wide bound at once. **The
  fence runs before the shape test and the cap test**, because both drop
  the path's buffer: a bad chunk from a retired process must never throw
  away the live process's partial push. That fence, and `newestEpoch`
  itself, bound this replica's buffers alone. **Each run owns a write
  monitor**, which keeps two writes of one key apart: `bucketStore.Replace`
  deletes the key and then writes part 0, so a redelivery of the push a
  replica is still writing must not interleave with it. The retired-epoch
  test inside `storeRunStateObject` is an optimization, not the ordering
  rule: it skips a write this replica already knows is superseded.
  `OnRunStateRequest` refuses a request whose `pusher_epoch` or
  `state_gen` is not positive, because the key names both. It serves a
  resume's fetch through `NewRangeReader` (the chunked read `Put` pairs
  with, never `NewObjectReader`), and calls `ownerCache.invalidate` on a
  run's **first** fetch: a resume moves a run's owner with no claim behind
  it, so `observeGrant` never replaces the entry. The invalidation only
  forces a re-read of the recorder and sets no owner. Answers ride
  `OperatorMessage.run_state_ack`, which mirrors `LogAck`; the mite records
  the answer and raises its own `503` on that run's next post.
- **Live tail** (`tail.go`) — `runTailLease` is a 90-second lease
  (`tailLeaseSeconds`) per run, held in `BusHandler.tailLeases`.
  `watchTail` subscribes `bus.TailSubject` (request-reply, queue group
  `"gateway"`) per connection; `handleTailStart`/`handleTailStop` push
  `OperatorMessage.tail_control` to the mite and reply without waiting for
  `TailAck`. `startOrRenewTailLease` looks a run's lease up (or creates
  it) and activates it with a fresh `control_id` in one `h.tailMu`
  critical section, `lease.mu` nested inside — the same lock order
  `expireTailLeaseIfCurrent` uses. `handleTailStart` calls it for both a
  first start and a renewal; the lookup and the activation must share one
  critical section, because a caller that released `tailMu` in between
  would let `expireTailLeaseIfCurrent` delete the lease from
  `h.tailLeases` right after validating its still-old `control_id`, so the
  renewal would go on to activate an orphaned lease object and report
  success while nothing in `h.tailLeases` holds it any more. `resetTailTimers` arms two independent timers per push: a
  90-second no-renewal expiry (`onTailLeaseExpired`, pushes `stop`) and a
  10-second ack-wait (`onTailAckTimeout`, clears the lease and keeps a kept
  error for 5 minutes). `OnTailAck`, `stopTailLease` (used by
  `handleTailStop`), and `clearTailLeaseOnFinal` each hold `h.tailMu` for
  their whole lookup-validate-delete sequence, `lease.mu` nested inside —
  the same lock order `expireTailLeaseIfCurrent` and
  `startOrRenewTailLease` use. A version that looked a lease up, released
  `tailMu`, then deleted by run id later would let an expiry delete that
  lease and a renewal install a fresh one under the same run id in
  between; the later unconditional delete would then remove the
  renewal's lease instead of the one it validated. `OnTailAck` also
  ignores an ack whose `control_id` is not the lease's current one, so a
  late reply can never clear a newer lease. `flushContiguousLogIndex`
  calls `publishLiveLogChunk` for each index entry right after that
  entry becomes durable, using the chunk's bytes held in
  `runLogState.pendingData` (a `pendingEntries` sibling, same lifecycle).
  This is later than the chunk's own arrival for an out-of-order chunk,
  so the relay message never leads the blob: a viewer that misses it and
  re-reads the store finds the same bytes. `publishLiveLogChunk` is a
  no-op unless the run's lease is active; its payload
  (`liveLogChunkPayload`, `internal/mite/tail.go`) is the entry's own
  fields under camelCase names plus base64 `data`.

## Work Guidance

- New `CommandStream` message variants need a case in **both**
  `StreamHandler` impls plus `noopHandler` — a missing case compiles fine but
  silently drops behavior. Current handlers: `OnConnect`, `OnHeartbeat`,
  `OnSnapshot`, `OnCommandResult`, `OnTokenRefreshRequest`, `OnObservabilityReport`,
  `OnContentResponse`, `OnJenkinsActivity`, `OnPluginInventory`, `OnBuildEvent`,
  `OnClaimRequest`, `OnLogChunk`, `OnTailAck`, `OnCrossTenantRequest`,
  `OnRunStateChunk`, `OnRunStateRequest`, `IsCurrentConnection`,
  `OnDisconnect`.
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
`jenkinstoken_test.go`, `consumed_token_kv_test.go`, `log_writer_test.go`
(blob-writer pipeline, ownership, pending bounds, the finalize deadline —
including the idle-run case with no later chunk to trigger it),
`tail_test.go` (lease table, renewal-versus-expiry races),
`crosstenant_bridge_test.go` (identity overwrite, unhosted-caller and
unknown-kind fail-closed paths, the 8 in-flight cap, the timeout budget
and its warning), `imperative_redelivery_test.go` (a pod roll in both
orders — the old stream tears down first, and the replacement connects
first — plus the cancelled pump's own nak, before and under `h.mu`).
