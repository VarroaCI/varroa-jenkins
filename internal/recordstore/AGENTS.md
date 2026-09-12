# internal/recordstore

## Purpose

Home of the build-record store: the Postgres schema, the migrations, the
build-number and claim ledger, the `builds.>` ingest write path and its
durable consumer, the `varroa_activity` transport buffer and its durable
consumer, and the blob abstraction (`blob/`) that `varroa-recorder`
(`cmd/recorder/`) owns. This is the durable history of a Jenkins build and
of the fleet's audit activity, once a controller becomes disposable.

## Ownership

- Owns: schema `varroa` in Postgres database `varroa`, migration `00001`
  and every migration after it, the migration runner, and `EnsureGrants`.
- Is the single writer of the record tables. No other package writes to
  schema `varroa`. A reader (the BFF, the gateway in blob mode) opens its
  own connection with the read-only `varroa_client` role and never
  migrates.
- Does NOT own the Jenkins plugin, mite, or gateway code that produces the
  events this package records, and does NOT own the placement authority
  that picks a cell.

## Local Contracts

- **Single writer** — every insert and update against schema `varroa`
  goes through this package. A sibling package that needs record data
  reads through the `Reader` this package exports; it never opens its own
  write path.
- **Migrations** — `migrations/00001_initial_schema.sql` holds the seven
  build-record tables (`run_intents`, `runs`, `run_stages`, `run_tests`,
  `run_artifacts`, `log_chunks`, `job_numbers`). `00002_activity.sql` adds
  `activity` (monthly RANGE partitions on `ts`) plus the search GIN index
  on `runs`. `00003_job_schedules.sql` adds `job_schedules` and
  `job_schedule_cells`. `00005_tenant_placements.sql` adds
  `tenant_placements`, the placement-fencing authority: one row per
  `(cluster, tenant)`, holding the placement identity, the rehydration
  seed (`rehydrated_instance`, `pending_jobs`), and the cell-liveness
  signal (`cell_alive`, `cell_alive_at`, `cell_last_seen_at`,
  `dead_instances`) the interrupted-run sweep reads. `00006_fleet_locks.sql`
  adds `fleet_locks`, the fleet-lock authority (see the Fleet locks
  bullet), and the sequence `fleet_lock_fence_seq`. `00007_job_schedule_upstream.sql`
  adds `job_schedules`'s three upstream columns. A later migration
  never edits an applied one. `migrationsFS` (`migrations.go`) embeds
  every file under `migrations/` into the binary through `embed.FS`, so
  the recorder needs no separate deploy artifact. Monthly partition
  names on `runs`, `log_chunks`, and `activity` follow
  `<table>_p<YYYYMM>`; the retention round that adds later partitions
  must use the same pattern. The schema holds exactly twelve tables, no
  more and no fewer.
- **Migration runner** (`migrate.go`) — `RunMigrations` creates schema
  `varroa`, applies pending migrations under a Postgres advisory lock on a
  one-connection pool, then calls `EnsureGrants`. Two callers racing
  against one database both return nil; only one applies the migrations.
- **Grants, not a migration** (`grants.go`) — `EnsureGrants` grants role
  `varroa_client` its read privileges when that role exists, and is a
  no-op otherwise. It never creates a role. CloudNativePG creates the role
  from the chart's `spec.managed.roles`, on its own schedule, so the
  migration runner and the retention ticker both call `EnsureGrants` on
  every pass.
- **`VARROA_TEST_DSN` harness** (`testutil_test.go`) — a test that needs a
  live database reads this environment variable and calls `t.Skip` when it
  is unset, so `make test` stays green with no database present. Each such
  test drops and recreates schema `varroa` (and any role it created) during
  setup, so a repeated run starts clean.
- **`blob/`** — the storage abstraction for build logs and build
  artifacts, behind `gocloud.dev/blob`: the `pgblob` driver for tier one
  and S3/GCS/Azure/file wiring for tier two. See its own `AGENTS.md`.
- **Ledger and claim responder** (`ledger*.go`) — the design writes the
  claim types as `ledger.ClaimRequest`/`ledger.ClaimResponse`; this
  package holds them as `ClaimRequest`/`ClaimResponse` with no separate
  `ledger` subpackage. `Ledger.Claim` answers one claim in a single
  transaction: it inserts a new intent, or applies design D2's
  decision table to an existing one, then reserves a build number
  through `reserveBuildNumber`. `Fence` gates a claim before the
  transaction opens; `allowAllFence` is the C1 default.
  `RegisterClaimResponder` subscribes `bus.SubjectLedgerClaim` with
  queue group `recorder`. `NextBuildNumber` and
  `RaiseWatermarkForStartedBuild` are exported for `ingest_started.go`'s
  `run.started` write, which is their only caller in this package
  today.
- **Concurrency cancel** (`ledger_concurrency.go`, `concurrency_metrics.go`)
  — the concurrency-groups capability's fire-and-forget side effect of a
  granted claim. `Ledger.SetConcurrencyCancel` wires a `*bus.Conn` and
  metrics onto an existing `Ledger`; a `Ledger` no caller wires runs no
  cancel at all. On a granted claim, `Claim` starts `runConcurrencyCancel`
  in its own goroutine with its own background context, so it answers the
  caller before the cancel's own database read and publish can finish. A
  `cancelInProgress` tenant's group (`<cluster>/<tenant>/<job_path>`)
  publishes one `cancel_run` action per non-`COMPLETED` older run, oldest
  first, capped at eight; the rest count in
  `recorder_concurrency_cancel_skipped_total`.
- **Placement fence** (`ledger_fence_placement.go`,
  `ledger_fence_metrics.go`) — `PlacementFence` is the placement-fencing
  capability's `Fence`. `Check` reads the claim's `(cluster, tenant)` row
  from `varroa.tenant_placements` and applies `decidePlacement`, the
  pure, ten-order table: tenant, namespace, cell, cell instance, and
  generation, in that order, then the cross-tenant test, then the
  rehydration seed state (`rehydrated_instance` and `pending_jobs`). A
  missing row answers `NOT_READY` with no exemption. `FenceMetrics`
  holds `recorder_fence_veto_total`, labelled by reason; a
  `PlacementFence` built with a nil `*FenceMetrics` counts nothing.
  `cmd/recorder` still constructs its `Ledger` with a nil `Fence`, so
  `allowAllFence` stays live in production until a later task wires
  `NewPlacementFence` in.
- **Tenant purge responder** (`tenant_purge.go`) — `RegisterTenantPurgeResponder`
  subscribes `bus.TenantPurgeSubject` with queue group `recorder`. It answers
  the placement pass's request to delete one tenant's `job_schedules` rows
  after the tenant leaves a cell: it deletes only the rows matching the
  request's cluster and tenant, and whose stored `placement_generation` is at
  most the request's, so a delayed retry can never remove a row the tenant's
  new cell already reported. The delete is a plain `DELETE`, idempotent by
  construction. The operator side of this contract lives outside this
  package.
- **Tenant sync responder** (`tenant_sync.go`, `tenant_sync_types.go`) —
  `RegisterTenantSyncResponder` subscribes `bus.TenantSyncSubject` with
  queue group `recorder`. It answers two request kinds: `announce`
  upserts the tenant's `tenant_placements` row (never lowering
  `generation`; a generation-zero row seeds itself from the announced
  cell instance at once; a generation raise or a cell-instance change
  clears `rehydrated_instance` and `pending_jobs`, because the new cell
  holds no seed yet) and merges `dead_instances` into the stored list,
  keeping every entry a `RUNNING` run's `run_intents.cell_instance`
  names and trimming the rest to ten; `records` calls
  `ListRehydrationRecords`. An unknown kind answers an error and writes
  nothing. The wire types (`TenantSyncRequest`, `TenantSyncResponse`,
  and the nested job/record shapes) live in `tenant_sync_types.go`,
  beside `ledger_types.go`, because this responder dispatches on them.
  `handleTenantSyncAnnounce` runs `MarkMovedRunsForTenant` when `cell` or
  `cell_instance` changes, and runs `ReleaseConfirmedFleetLocks`
  (`fleet_lock_release.go`) on every announce, against the same
  `tenantPlacementLivenessRow` the announce just wrote: a lock's second
  release confirmation can become newly true on any announce, not only a
  cell or an instance change, because `cell_alive` can flip on its own.
- **Cross-tenant responder** (`crosstenant_*.go`) — `RegisterCrossTenantResponder`
  subscribes `bus.LedgerCrossTenantSubject` with queue group `recorder`. It answers
  eight kinds behind one `CrossTenantRequest`/`CrossTenantResponse` pair:
  `build` (`crosstenant_build.go`) reserves a run identifier against the ledger
  through `Reader.GetIntentByKey`, minting a fresh UUIDv7 and publishing one
  `LedgerActionTriggerRun` action (actor `svc-<caller-tenant>`) only when no
  `run_intents` row exists yet, and never writing that row itself; `status` and
  `last_successful` (`crosstenant_status.go`) and `artifact_list`/`artifact_url`
  (`crosstenant_artifact.go`) read a run's tenant and job path
  (`crosstenant_runrecord.go`) and run the `request`-path grant against them
  through `GrantEvaluator.Check` (`grant_evaluator.go`) before answering;
  `artifact_url` signs the recorded `blob_key`
  through `blob.Store.SignedURL`, mapping `blob.ErrUnsupported` to a message
  naming `recordStore.blobs.mode`. Every reply's `error`/`code` pair follows the
  claim path's rule: `UNAVAILABLE` for an unanswering Postgres, `INTERNAL` for
  every other fault, through the shared `classifyDBError` (`ledger.go`).
  `CrossTenantMetrics` (`crosstenant_metrics.go`) holds
  `recorder_crosstenant_requests_total` (labelled `kind`/`outcome`) and
  `recorder_crosstenant_grant_denied_total` (labelled `reason`, from
  `crossTenantGrantDenialReason`'s match on the veto message's added
  clause, since `GrantResult`'s shape stays fixed to the `FenceResult`
  fields `ledger_fence_placement.go`'s direct type conversion depends
  on); `dispatchCrossTenant` records both on every call.
- **Fleet locks** (`fleet_lock_acquire.go`, `fleet_lock_renew.go`,
  `fleet_lock_release.go`, `fleet_lock_metrics.go`) — the cross-tenant
  responder's `lock_acquire`/`lock_renew`/`lock_release` kinds over
  `varroa.fleet_locks` (migration `00006`). `splitLockName` resolves a
  wire-level lock name to its owning tenant and bare name: a name with no `/`
  is the caller's own; a name holding one is another tenant's namespace, and
  an acquire or a renew across that boundary also calls `GrantEvaluator.Check`
  with `resource_kind` `lock`. `acquireFleetLock` runs the five-step transaction
  (the derived `held`-to-`releasing` move for an `INTERRUPTED` run, the expiry
  delete, the waiting insert, the FIFO promotion by `(waiting_since, run_id)`,
  and the reply) with the one retry `isFleetLockOneHolderViolation` names after
  a unique violation on `fleet_locks_one_holder`; a second violation answers
  `waiting` with no further attempt. A release deletes the caller's row
  unconditionally and carries no fence: only acquire and renew do.
  `moveInterruptedHoldersToReleasing` (`fleet_lock_acquire.go`) is the one
  shared implementation of the derived move, behind the narrow
  `fleetLockExecer` interface: the acquire transaction's own step calls it
  with one lock name, and the maintenance round's `sweepFleetLocks`
  (`retention_fleet_locks.go`) calls it with an empty name to move every
  held row of an interrupted run fleet-wide, before its own bounded-batch
  expiry delete, which skips a `releasing` row exactly as the acquire's own
  expiry delete does. A `releasing` row leaves the table on exactly two
  confirmations: an ingested terminal `run.completed` for the holder
  (`ingest_completed.go`'s own `applyRunCompleted`, gated on the owner
  test, in the same transaction as the `runs` write, so a crash after
  commit never strands the lock), or `ReleaseConfirmedFleetLocks`
  (`fleet_lock_release.go`), the tenant-sync responder's own hook, which
  reuses `cell_liveness.go`'s `deadInstanceGone`/`currentInstanceGone`
  readings with no anchor test. `FleetLockMetrics`
  (`fleet_lock_metrics.go`) holds three observable gauges
  (`recorder_fleet_lock_held`/`_waiting`/`_releasing`, each a live
  `count(*)` read) and `recorder_fleet_lock_expired_total`, one instance
  shared by the maintenance round and the cross-tenant responder's
  acquire path so the gauges register once, never twice, on the meter.
- **Rehydration seed reads** (`rehydration.go`) — `ListRehydrationRecords`
  is not a `Reader` method: only the tenant sync responder's `records`
  kind calls it. An empty job-path list enumerates every distinct job
  path of the tenant holding a `job_numbers` row or a completed run,
  with its next build number and no record. A non-empty list, capped at
  32 paths, answers the newest completed runs per job path up to the
  caller's per-job limit, plus the newest `SUCCESS` run when it falls
  outside that limit, from one query with a window function per job
  path. The next build number always comes from `job_numbers` through
  `NextBuildNumber`, never from `run_intents` or `runs`, so retention
  never resets it.
- **Ingest write path** (`ingest*.go`) — `IngestEnvelope` (`ingest_envelope.go`)
  is the versioned JSON payload on `builds.<cluster>.<ns>.<cell>`; C2 maps
  its protobuf `BuildEvent` onto its field names. `Ingester.Apply`
  (`ingest_apply.go`) writes one envelope inside one transaction, dispatching
  by `Kind` to one `ingest_<kind>.go` file per D4's write table. Each write
  is a natural-key upsert; `runsStatusMergeSQL` (`ingest_common.go`) is the
  one CASE expression every `runs` writer reuses to keep `status` monotonic
  and `DUPLICATE` terminal. `ingest_duplicate.go` holds the duplicate
  backstop `run.started` runs in the same transaction, publishing a
  `cancel_run` ledger action (`ledgeraction.go`) through
  `PublishLedgerAction` on `bus.LedgerActionSubject`. `RunIngestConsumer`
  (`ingest_consumer.go`) is the durable pull consumer named `recorder`
  that applies D4's disposition table; `EnsureBuildsStream`
  (`ingest_stream.go`) creates the `varroa_builds` stream, and
  `cmd/recorder` is the only caller. `IngestMetrics`
  (`ingest_metrics.go`) holds the five `recorder_ingest_*` instruments on
  `otel.Meter("varroa-recorder")`. The `job.schedules` kind rides the same
  `varroa_builds` stream and the same `recorder` consumer as every
  `run.*` kind; it carries an empty `run_id`, so `Ingester.Apply` skips
  the intent lookup for it. `ingest_job_schedules.go` upserts every row
  the envelope carries, row-locks the cell's `job_schedule_cells`
  watermark inside the same transaction, rejects an envelope whose
  `observed_at` is not newer than the stored value, and deletes every row
  of the cell older than the accepted envelope, but only when the
  envelope is not truncated. `IngestMetrics` also holds
  `recorder_schedules_truncated_total` and `recorder_schedules_stale_total`.
  `applyRunCompleted` (`ingest_completed.go`) reports `owner`, whether its
  write was the run's real terminal write, not a foreign instance's
  payload-only merge; on `owner`, in the same transaction as the `runs`
  write, it deletes this run's `held`/`releasing` fleet_locks row, the
  first of the two release confirmations `fleet_lock_release.go`'s own
  bullet names. `Ingester.SetUpstreamFanout` (`ingest_apply.go`) is an
  optional setter, matching `Ledger.SetConcurrencyCancel`'s own
  convention: an `Ingester` no caller wires runs no fan-out. When wired,
  `Apply` calls `RunUpstreamFanout` (`upstream_fanout.go`) after the
  transaction that recorded an owner `run.completed` write has committed,
  never inside it, because the round reads `GrantEvaluator` over several
  queries and publishes on the bus. The round selects
  `job_schedules` rows by the producer's relative job name (stripped of
  the producer's own tenant prefix, matching `upstream_job`'s own
  storage convention), filters by the Jenkins result threshold and by
  `resolveFanoutTargetCluster`'s own mirror of
  `GrantEvaluator.resolveTargetPolicy`'s cluster resolution, orders by
  `(cluster, tenant, job_path)`, caps at 32, then runs the `fanout`-path
  `GrantEvaluator.Check` per row and publishes one `LedgerActionTriggerRun`
  per allowed row, actor `varroa-upstream/<producer-tenant>`. The
  intent-key seq is `t<digest>`, the first twelve hex characters of the
  SHA-256 of the subscriber's own `<cluster>/<tenant>/<job_path>`
  (`upstreamFanoutSeq`), stable across a redelivery because it never
  carries a position. `UpstreamFanoutMetrics`
  (`upstream_fanout_metrics.go`) holds `recorder_upstream_fanout_total`
  and `recorder_upstream_fanout_skipped_total` (labelled `reason`, over
  `over_cap`/`denied`/`unplaced`/`disabled`/`publish_failed`).
- **Activity ingest** (`activity_consumer.go`, `activity_ingest.go`,
  `activity_stream.go`) — a second, independent pipeline, not a mode of
  the build pipeline above. `EnsureActivityStream` owns the
  `varroa_activity` JetStream stream: a fixed 72-hour transport buffer
  (`activityStreamMaxAge`/`MaxMsgs`/`MaxBytes` in
  `internal/bus/jetstream.go`), never chart-configurable, applied only
  when the durable consumer `activity-recorder` carries no pending or
  unacknowledged message, so a limit change never deletes a message the
  consumer has not read yet. `RunActivityIngestConsumer` drains that
  consumer, decodes each message from its payload, never its subject, so
  a legacy `activity.<ns>.<ctrl>` message keeps whatever `cluster` it
  carries, calls `Event.Normalize`, computes `EventKey`
  (`ingest_activity_key.go`, a content key over the normalized fields, so
  a redelivery upserts idempotently with no replay table), and inserts
  into `activity`. `ActivityMetrics` holds
  `recorder_activity_duplicate_total`, `recorder_activity_expired_total`,
  `recorder_activity_unknown_total`, and the
  `recorder_activity_backlog_msgs` gauge. The recorder therefore runs
  **two** durable pull consumers side by side: `recorder` on
  `varroa_builds` (every `run.*` kind plus `job.schedules`) and
  `activity-recorder` on `varroa_activity`. Both obey the same
  single-writer rule: no other component writes to `activity`,
  `job_schedules`, or `job_schedule_cells`.
- **Search** — `activity.search_tsv` is a generated, stored `tsvector`
  column with a GIN index; the `runs` and `job_schedules` GIN expression
  indexes live in migrations `00002` and `00003`, because an expression
  index accelerates only a query that repeats the identical expression.
  Every index calls `to_tsvector('simple', ...)`, which applies no
  stemming, so a job-path segment such as `service` still matches.
  `Reader.SearchRecords` is the query seam the BFF `/search` and MCP
  `search` tool share for the `job`, `run`, and `activity` kinds; the
  four live kinds (controller, namespace, group, catalogitem) stay
  Kubernetes-sourced and never reach this package.
- **Retention and maintenance** (`retention*.go`) — `ParseRetention`
  accepts `<n>d`, `n` between 1 and 3650, and checks the range before it
  multiplies. `EnsurePartitions` creates the previous month, the current
  month, and the next two months for `runs` and `log_chunks`, matching
  migration `00001`'s own window and naming; `activity` also reaches
  backward to `recordStore.retention.activity`, because the first pass
  imports history that predates the current month. `RunMaintenanceRound`
  holds the whole round on one acquired connection, under
  `pg_try_advisory_lock` on `maintenanceLockKey` — a different key from
  `migrate.go`'s `migrateLockKey`, so a migration and a round never
  contend. `RunMaintenance` is the hourly ticker; it calls `EnsureGrants`
  on every tick itself, not inside the locked round, because every
  replica must notice the client role as soon as CloudNativePG creates
  it. Object deletes (`retention_objects.go`) take an `ObjectDeleter`
  (`Delete(ctx, key) error`), a narrow interface over `blob.Store`'s
  `Delete` method, and call `blob.LogKey`/`blob.LogIndexKey` for the two
  fixed log keys. `finalizeStaleLogs` (`retention_finalize.go`) is step
  10, the log-finalization sweep C3's `build-log-externalization`
  capability defines; it never lowers a stored `finalized` true. The
  exported `Liveness` interface answers whether a run's owning cell is
  gone; `HeartbeatLiveness` is C1's heartbeat-only implementation
  (15-minute bound), and `PlacementLiveness` (`retention_liveness.go`) is
  the placement-aware one C6 supplies in its place, reading
  `tenant_placements` through the same `cellLivenessGone` reading
  `interrupted.go`'s sweep uses. `sweepFleetLocks`
  (`retention_fleet_locks.go`) runs after `finalizeStaleLogs`: it moves
  every `held` fleet_locks row of an `INTERRUPTED` run to `releasing`
  fleet-wide, then deletes every expired row that is not `releasing`, in
  `deleteBatched`'s own bounded batches; see the Fleet locks bullet for
  the shared move it calls. `MaintenanceConfig.FleetLockMetrics` counts
  the round's own expired-row deletes. Step 5a
  (`retention_activity.go`) deletes expired `activity` rows in batches of
  5000, then step 5b drops every fully expired `activity` partition; both
  run against `recordStore.retention.activity`, which C1's numbering
  shifts down to steps 6 through 10. `job_schedules` and
  `job_schedule_cells` carry no retention step: a row leaves only when a
  later `job.schedules` envelope for the same cell omits it, or when C5's
  tenant purge removes it. A schedule row describes live state, not
  history, so a staleness sweep would delete a row the fleet still needs.
- **Interrupted-run marking** (`interrupted.go`, `cell_liveness.go`,
  `interrupted_metrics.go`) — design D9. `evaluateInterruption` applies
  the moved test, then the silent test, to one `RUNNING` run;
  `cellLivenessGone` is the one shared cell-liveness reading both this
  package's sweep and `retention_finalize.go`'s log-finalization sweep
  call, through `PlacementLiveness`. `MarkMovedRunsForTenant` is the fast
  path a `tenant.sync` announce triggers when `cell` or `cell_instance`
  changes; `SweepInterruptedRuns` is the periodic path, gated on the
  recorder's own `varroa_builds` consumer staying fresh
  (`VARROA_SWEEP_MAX_PENDING`, default 1000). Neither inserts an intent
  or touches `fleet_locks`; `afterMark` publishes the `cancel_run` and
  automatic `retry_run` actions a mark triggers.
- **Backstop cron scheduler and the interrupted sweep's round**
  (`backstop_round.go`, `backstop_policy.go`, `backstop_candidates.go`,
  `backstop_fire.go`, `backstop_metrics.go`, `action_metrics.go`,
  `concurrency_metrics.go`, `ledger_concurrency.go`) — design D10 and
  D11. `RunBackstopRound` holds one round on `backstopLockKey`, the third
  fixed advisory-lock key after `migrateLockKey` and `maintenanceLockKey`,
  and runs `SweepInterruptedRuns` and `runBackstopSchedule` inside the
  same hold, so one ticker and one lock cover both. The round computes no
  fire minute of its own: `isBackstopCandidate` admits only an occurrence
  Jenkins itself exported (`JobSchedule.NextFireTimes`) that is past the
  tenant's grace window, not older than 24 hours, holds no
  `cron:<job-path>:<occurrence-minute>` ledger intent, and holds no
  recent `build.scheduled.backstop` activity row (keyed on that row's
  `reason` field, which holds the occurrence minute). `selectFire`
  applies `Team.spec.pipelines.cronCatchUp`; `selectReadyFires` applies
  the `sha256(cluster|tenant|job_path) mod grace_seconds` spreading delay
  and the 100-fire-per-round bound. `readTenantSchedulePolicy` answers
  the backstop-scheduler capability's defaults (backstop on, catch-up
  `one`, grace `5m`) for a tenant with no `tenant_placements` row.
  `Ledger.SetConcurrencyCancel` wires the sibling concurrency-groups
  capability: on a granted claim for a `cancelInProgress` tenant, `Claim`
  starts `runConcurrencyCancel` in its own goroutine so it answers the
  caller before the cancel's own database read and publish. Every action
  this bullet's code publishes counts in `recorder_actions_published_total`,
  labelled by kind, alongside the interrupted-run marking's own publishes.
- **Reader** (`reader*.go`) — `Reader` is the BFF's only read path onto
  the record tables; `NewReader` wraps a `*pgxpool.Pool`. Every method
  calls `checkSchema` first and returns `ErrSchemaTooOld` when the
  applied `varroa.goose_db_version` is below `minGooseSchemaVersion`, or
  unreadable. `ListRuns` pages by keyset on `(created_at DESC, run_id
  DESC)`, never `OFFSET`; its `cause` filter joins `run_intents` instead
  of matching `runs.cause`, because `runs.cause` holds the free-form
  Jenkins `Cause` object and `run_intents.cause` holds the plain-text
  classification a claim request carries. `GetRun` resolves `created_at`
  from `run_intents` first, so its `runs` query prunes to one partition.
  `JobSummary` is record-derived only: no job definition, no trigger
  state, no job that has never run — the pipelines API supplies those
  from the catalog. `ListActivity` pages `activity` by keyset on
  `(ts, event_key)`, backward by default and forward for an explicit
  time range; its `ActivityQuery.Scope` is the D5 authorization
  superset pushed into SQL, and the per-event predicate still runs on
  every returned row. `ListJobSchedules` returns one cell or tenant's
  `job_schedules` rows, carrying `NextFireTimes`, `Namespace`, `Cell`, and
  `PlacementGeneration`, which the backstop cron scheduler reads directly
  from this package, not only from the BFF. `SearchRecords`
  answers the `job`, `run`, and `activity` kinds of the `search` tool
  and `GET /search`. `ActivityRetentionDays` reports the parsed
  `recordStore.retention.activity` value the BFF passed to `NewReader`;
  a nil `Backfill` (no DSN configured) never reaches this method.

## Work Guidance

- Every table lives in schema `varroa`. A column, a CHECK constraint, and
  an index must match `migrations/00001_initial_schema.sql`; a later change adds
  a new migration and never edits an applied one.
- A Postgres GRANT lives in `EnsureGrants`, never in a migration file.
- `EnsureGrants` runs on the recorder's own DSN, so its `ALTER DEFAULT PRIVILEGES` binds
  to `varroa_recorder`, the role that creates every partition and table. A partition created
  by another role gives `varroa_client` and `varroa_gateway` no `SELECT` on it.

## Verification

```bash
VARROA_TEST_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
  go test -p 1 -race -count=1 ./internal/recordstore/...
go test -race -count=1 ./internal/recordstore/...   # DSN unset: tests skip
```

`-p 1` is required with a DSN: every package under this tree shares schema
`varroa`, and the `recordstore` harness drops it, so two packages running at
the same time fail. The CI job uses the same flag.

## Child DOX Index

- [blob](blob/AGENTS.md) — the `Store` interface, `pgblob`, and the
  `gocloud.dev` adapter for build logs and build artifacts.
