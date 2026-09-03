# internal/bus

## Purpose

Wraps the shared NATS/JetStream connection and defines every subject, KV
bucket, and stream schema used to move state between `varroa-operator`,
`varroa-gateway`, `varroa-bff`, and mite sidecars across one or more
Kubernetes clusters ("brood"). This is the wire contract for the whole
control plane's async transport — no other package talks to NATS directly.

## Ownership

- Owns: connection/TLS setup (`conn.go`), subject naming (`subjects.go`),
  JetStream stream configs (`jetstream.go`), KV helpers (`kv.go`), the
  cluster directory (`clusters.go`, `cluster.go`), gateway↔operator
  request/reply schemas (`operator.go`, `config.go`), the mite presence
  record (`presence.go`), and the `ActivityPayload` wire schema
  (`activity.go`).
- Does NOT own: the JetStream-backed activity persistence/query API
  (`internal/api/activity/` — retention dial, backfill reads) or the live
  SSE fanout (`internal/api/sse/bus_fanout.go`, `BusFanout`) — those are
  consumers of this package's `Conn`/`ActivityPayload`/subjects and belong
  to `internal/api`.
- NATS server-side ACLs live in `charts/varroa/templates/nats-auth-config.yaml`
  (Helm, not Go) — keep subject prefixes here and ACL subjects there in sync.

## Local Contracts

- **Cluster identity** — `ClusterFromEnv()` (`cluster.go:19`) resolves
  `VARROA_CLUSTER_NAME`, defaulting to `DefaultCluster = "core"`; must be a
  DNS-1123 label ≤63 chars or the process fails fast at startup. Nearly every
  subject is namespaced by this cluster token.
- **Subjects** (`subjects.go`) — `mite.<cluster>.<ns>.<name>.{in,out,content}`
  (mite command/telemetry/content channels), `activity.<cluster>.<ns>.<ctrl>`
  / `activity.<cluster>._global` (`ActivitySubject`/`ActivityGlobalSubject`,
  wildcard `ActivityWildcard = "activity.>"`), `events.brood.>`
  (`WebhookSubject`/`WebhookWildcard`, hibernation webhook replay),
  `wake.<cluster>.<ns>.<ctrl>` (hibernation wake), `operator.<cluster>.*`
  (BFF→operator remote-cluster RPCs, see `config.go`).
- **JetStream streams** (`jetstream.go`) — `ActivityStreamName` /
  `ActivityStreamConfig`: subjects `activity.>`, `MaxAge=1h`,
  `MaxMsgsPerSubject=1000`, `MaxBytes=64MiB`, `AckWait=60s` — this is the
  *default* config; `internal/api/activity` overrides `MaxAge` per the
  `off|7d|30d|90d` retention dial (`internal/api/activity/retention.go`) when
  ensuring the stream. `WebhookStreamName = "varroa_webhooks"` backs
  hibernation replay. The legacy `DefaultStreamName` (`"varroa"`,
  `mite.*.*.*.out`, `WorkQueuePolicy`) is retained for one-shot imperative
  commands but the operator→mite command path now runs over the
  `mite_desired` last-value KV, not this stream.
- **KV buckets** (`kv.go`, `subjects.go`) — `KVSnapshotBucket = "mite_snapshots"`
  (latest mite state, key `SnapshotKey(cluster, ns, name)`),
  `KVClustersBucket = "varroa_clusters"` (cluster directory, see below), plus
  an observability-intent bucket keyed `"obs/<cluster>/<ns>/<name>"`.
  `EnsureKV`/`KV.Put`/`KV.Get`/`KV.Watch` wrap `nats.KeyValue` with
  `ErrKVKeyExists` normalized from `nats.ErrKeyExists`.
- **Cluster directory** (`clusters.go`) — `ClusterInfo` JSON value in
  `KVClustersBucket`, schema frozen per multicluster coordinator registry
  §4.2. `ClusterHeartbeatInterval = 30s`, `ClusterEntryTTL = 90s`.
  `ClusterStateActive|Draining|Drained`. `PutClusterHeartbeat`/`ListClusters`
  (sorted by name, unparseable entries skipped not fatal).
- **Presence** (`presence.go`) — `Presence` struct is the gateway-written
  liveness record per connected mite (`LastHeartbeat`, `CertExpiry`, `Epoch`,
  idle gauges); carries fields the generic `StateSnapshot` proto can't hold.
- **ActivityPayload** (`activity.go`) — single publish/consume schema for
  Jenkins activity events; published via `ActivitySubject(cluster, ns,
  controller)` by the gateway's bus bridge, consumed by the BFF's
  bus-routed ingest. `Name`/`Controller` are intentionally duplicate
  fields (routing alias vs. frontend display alias) — don't collapse them.
- **Replicas** — `Conn.Replicas()` derives JetStream replica count from
  `VARROA_JETSTREAM_REPLICAS`, clamped via `clampReplicas`; used by every
  `EnsureStream`/KV bucket config so R1→R3 topology changes are a single knob.
- **Credentials** — `Config.PasswordFile` is the production credential path:
  it is re-read on every connect attempt through `nats.UserInfoHandler`, and
  `Connect` always sets `nats.IgnoreAuthErrorAbort()`, so a client survives a
  server-side password rotation until its mounted Secret catches up. `Password`
  is the static fallback for tests and local runs only; the two are mutually
  exclusive (nats.go rejects a static user/password alongside a handler).
  `Connect` fails fast when `PasswordFile` is unreadable at startup; a read
  failure during a later reconnect only logs.
- **Logging** — a `Conn`'s logger is supplied only through `Config.Logger` and
  is installed by `Connect` before any handler closure exists. There is no
  exported logger field: the NATS callbacks read it from library goroutines and
  the credential handler fires inside `nats.Connect`, so assigning one after
  `Connect` returns was an unsynchronized write to a field already being read.
  Nothing outside this package writes it. `Connect` sets
  `nats.NoCallbacksAfterClientClose()`, so a graceful `Conn.Close()` logs only
  its own line: the `nats connection closed permanently` error stays reserved
  for a connection nats.go gave up on. With `IgnoreAuthErrorAbort` and
  `MaxReconnects(-1)` that is rare by design, so it is a genuine alarm and never
  a routine-shutdown or stale-credential artifact. A component holding a stale
  password retries forever instead, which is why the runbook keys on the
  recovery window rather than on any single log line.
- **Startup** — `Connect` sets `nats.RetryOnFailedConnect(true)` and then blocks
  until the connection is actually CONNECTED, bounded by
  `Config.StartupTimeout` (`DefaultStartupTimeout`, 3 minutes). A process that
  starts while the bus is down, or before a rotated password reaches its mounted
  Secret, therefore waits instead of exiting into a crash loop, and callers never
  receive a `Conn` whose JetStream/KV calls would fail. Config errors stay
  fail-fast and are checked before any dial: an unreadable `PasswordFile`, and a
  `PasswordFile` with no `Username`. No probe port listens while `Connect`
  blocks, so `DefaultStartupTimeout` is paired with the 240s `startupProbe`
  window on the operator/gateway/bff Deployments
  (`charts/varroa/templates/*/deployment.yaml`). **Changing this constant means
  changing that probe window in the same commit**, or kubelet kills the
  container mid-wait.
- **Bus state** — `Conn.Connected()` and `Conn.RegisterMetrics(meter, component)`
  (the `varroa.bus.connected` gauge, 1/0, attribute `component`) are the only
  bus-state surfaces. Readiness checks and gauges go through them; nothing
  outside this package reaches into `nc` to ask whether the bus is up.
- **ACLs** — gateway needs `$JS.ACK.varroa.>` publish permission to ack on
  the legacy `varroa` stream; BFF owns create/update/consumer/ack on
  `varroa_activity` and `varroa_webhooks`. Cross-check
  `charts/varroa/templates/nats-auth-config.yaml` when adding a new stream,
  subject prefix, or KV bucket — connections are denied by default.

## Work Guidance

- New subjects/streams/buckets must be added here first, then mirrored into
  the NATS auth config (chart) for every user (operator/gateway/bff) that
  needs them — a missing ACL entry fails silently as a connection-level
  permission denial, not a compile error.
- Keep `ActivitySubject`/`WebhookSubject`/`MiteInSubject` etc. as the single
  source of truth for subject strings; do not hand-format subject strings
  elsewhere in the codebase.

## Verification

```bash
go test -race -count=1 ./internal/bus/...
make lint
```
