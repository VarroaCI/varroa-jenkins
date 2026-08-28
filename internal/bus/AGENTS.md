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
