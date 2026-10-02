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
- **`Reader.ListTenantPlacements`** (`reader_placements.go`) answers the
  slot-0 coordinate of every `tenant_placements` row of one tenant, one per
  cluster. It is the core BFF's only view of a Team that a member cluster
  places, because that Team lives on the member's API server.
  `Reader.ListAllTenantPlacements` answers the same coordinate plus the
  generation for every row of every tenant; the BFF's `GET /pipelines`
  landing list reads it. A generation-0 row names a torn-down tenant.
- **The pipeline mesh** (`mesh_takeover.go`, `resume_target.go`,
  `ingest_state.go`, `ingest_resume_failed.go`) — C3's run-state
  replication and cross-cell resume.
  `applyRunState` merges each `run.state` outcome into
  `runs.payload->'mesh'`; the recorder alone owns the 64 MiB cap, and the
  cap path clears `mesh.state_at`, `mesh.resume`, and the run's stored
  objects together. **A dispatched resume holds that whole path**: while
  `mesh.resume` names a target, the cap records `mesh.cap_warned_at`
  alone, because the target reads exactly the objects the cap deletes.
  The cap cannot bound the bytes by refusing the entry instead: the
  gateway writes the object before it reports it, so a refused entry
  frees nothing and loses the record of an object that exists. The
  record therefore keeps every entry, and the first report after the
  resume clears ends the replication. The target's own heartbeat, a
  resume failure, and the resume deadline are the three exits that
  bound the hold. Each `mesh.files` entry carries the pusher `epoch` and
  the `state_gen` of the object it names, and `applyRunStateFile` decides
  precedence under a `SELECT … FOR UPDATE` of the run row: the higher epoch
  wins, and inside one epoch the higher generation wins. The lock is what
  makes that safe against a second recorder replica; `UPDATE … RETURNING`
  cannot answer it, because that array is already the written one. **The
  object key carries the epoch and the generation together**, so a decision
  that supersedes another push sets `dispatchResult.stateKeyCleanup`, and
  `Apply` deletes that exact key after the commit, beside the terminal
  cleanup. A redelivery repeats both numbers, names the winner's own
  key, and deletes nothing. The cap counts the record's entries, one per
  path, so a run's stored objects can hold one superseded push per path
  until that delete lands; the retention round's prefix delete removes the
  rest.
  **A terminal cleanup keeps the run's build.xml**, because a replay reads
  it. `deleteRunStateExceptScript` (`retention_objects.go`) deletes every
  other entry of the record's `mesh.files` list by key, and no prefix
  listing exists. `RunScriptObjectKey` (`run_script_key.go`) is the one
  selection of the kept entry (highest epoch, then generation, then the
  shortest path), and the BFF's script read calls it too. A nil list and a
  list that names no build record delete the whole prefix. The run-state
  cap alone passes nil (`dispatchResult.stateCleanupFiles`), because its
  purpose is to free the run's storage; a terminal write, the interruption
  marking, and the hourly backstop read the column and pass an empty
  non-nil list for a NULL one. `deleteRetainedRunScripts` removes the kept
  object with the run's whole prefix on the `retention.runs` cutoff,
  immediately before the `runs` partition drop.
  `runMeshRecord.hasState` refuses a record
  holding an entry whose epoch or generation is not positive: such an entry
  names no object, so the run takes the ordinary confirmed-loss path.
  `LedgerAction.Files` is `[]ResumeFile{RelPath, Epoch, StateGen}`, and the
  build record stays last. `TakeOverLostRuns` claims a lost run in two steps: it
  arms every `RUNNING` run whose owner is confirmed gone and whose anchor
  is fresh, then dispatches once `resumeTargetForRun` answers a seeded
  member. `resumeTargetForRun` is the **only** reader of the placement row
  for a takeover, and C4 extends that one body. The dispatch moves
  `run_intents`, `fleet_locks`, and `runs` in one transaction, and only the
  `runs` statement's row count gates the publish: the other two may move
  zero rows for a legitimate run. `resumeArmWindow` (15 minutes) and
  `resumeDeadline` (6 minutes) are constants, never settings. Their sum is
  exported as `ResumeAdoptBound`, and `silentHeartbeatWindow` as
  `SilentCellWindow`. The controller sizes its tenant agent pod ages
  from both, and its test fails when a window outgrows those ages. The
  heartbeat ingest clears `mesh.resume` once the named target's own
  heartbeat arrives **after** the dispatch, which is the only proof a
  resumed build gives: a
  record left in place would shield the run from every interruption test
  through a later death of the target itself. The clear, the resume
  guard, and the marking write all compare the recorder's own receive
  stamp against `mesh.resume.requested_at`, so one clock answers.
  `ingest_resume_failed.go` answers a resume the target could not apply.
  `applyResumeFailed` clears `instance`, `requested_at`, and `deadline`
  from `mesh.resume` and **keeps the arm**, so the next round can pick
  another target while the original `arm_expires` holds. It never moves
  that value: a failure buys the run no time, and the window bounds how
  long the run holds its fleet lock. It records the failed instance in
  `mesh.resume.failed`, and `resumeTargetForRun` never picks an instance
  on that list — the bound on re-dispatch, because a further dispatch
  then needs a pod that rolled or a job home that moved. **Two reason
  codes are held and write nothing at all**, `RESUME_LOAD_FAILED` and
  `RESUME_NOT_BUILDING`: both prove the target's own Jenkins process
  reached the build load, and a second target must never adopt a build
  the first may hold. Such a run keeps its dispatch and takes the
  ordinary deadline path, which publishes the cancel. The write opens no
  transaction — one guarded statement commits on its own — and its guard
  reads the run's status and its stored `mesh.resume.instance`, so a
  repeat and a late result from an earlier attempt both change no row.
  **The run's owner is not moved back**: the failed target keeps it, and
  the next dispatch moves it again.
- **Migrations** — `migrations/00001_initial_schema.sql` holds the seven
  build-record tables (`run_intents`, `runs`, `run_stages`, `run_tests`,
  `run_artifacts`, `log_chunks`, `job_numbers`). `00002_activity.sql` adds
  `activity` (monthly RANGE partitions on `ts`) plus the search GIN index
  on `runs`. `00003_job_schedules.sql` adds `job_schedules` and
  `job_schedule_cells`. `00010_run_state_object_class.sql` widens
  `log_chunks_object_class_check` to accept the class `state`, which the
  pipeline mesh's run-state objects carry. `00005_tenant_placements.sql` adds
  `tenant_placements`, the placement-fencing authority: one row per
  `(cluster, tenant)`, holding the placement identity, the rehydration
  seed (`rehydrated_instance`, `pending_jobs`), and the cell-liveness
  signal (`cell_alive`, `cell_alive_at`, `cell_last_seen_at`,
  `dead_instances`) the interrupted-run sweep reads.
  `00011_tenant_members.sql` adds `members` and `job_homes` to that row:
  the tenant's member list and its job path to slot map. Both default to
  an empty value, and an empty `members` list answers one synthetic
  slot-0 member built from the row's scalar columns. `members.go` holds
  the only three readers of a member's identity, `memberForJob`,
  `memberForInstance`, and `memberForCell`, plus the Lamping and Veach
  jump consistent hash the recorder homes a job with. `homeSlotForJob`
  resolves a job's slot for `memberForJob`: a stored `job_homes` entry
  wins, an empty path answers slot 0, and a path with no entry answers its
  computed hash slot over the active slot count. That fallback equals the
  value the next recompute stores, so the dispatcher, the fence, and the
  schedule gate read one home from the moment a job exists. `resolveJobHome`
  (`job_homes.go`) joins `readTenantPlacementMembers` and `memberForJob`
  into one read of a job's home member. `pgReader.JobHome`, the upstream
  fan-out, and the backstop fire call it; the two rounds call it through
  `jobHomeCache`, which reads each placement once for each round. The
  interrupted-run automatic retry calls `jobHomeOf` over the placement its
  mark decision already read.
  `job_schedules.cell` names the job's home. Every home write moves the
  rows in its own transaction: `rehomeJobSchedules` (`job_homes.go`) runs
  after `writeTenantPlacement` in the announce and after the map update in
  `refreshOneTenantJobHomes`. It sets a moved row's `namespace` and `cell`
  to the home member's values and its `placement_generation` to the
  placement's generation, so a delayed tenant purge at an older
  generation does not delete it. It keeps
  `observed_at` and `next_fire_times`, and it moves no row whose home slot
  holds no cell. The column is an input to the ingest delete rule and the
  cell purge only. An action that must reach a job's home never reads it
  for a placed tenant.
  `00006_fleet_locks.sql`
  adds `fleet_locks`, the fleet-lock authority (see the Fleet locks
  bullet), and the sequence `fleet_lock_fence_seq`. `00007_job_schedule_upstream.sql`
  adds `job_schedules`'s three upstream columns.
  `00008_trigger_failed_result.sql` widens `runs_result_check` so
  `runs.result` accepts `TRIGGER_FAILED`; its down step deletes every
  such row first, because a row left behind fails the narrower
  constraint. `00009_run_intents_refired_at.sql` adds the nullable
  `run_intents.refired_at`, the unstarted-intent re-fire's at-most-once
  marker, and `run_intents.refire_attempts`, the bound on the retry loop
  a rejected trigger opens. `00012_runs_heartbeat_received_at.sql` adds
  `runs.heartbeat_received_at`, the recorder's own receive time for a
  run's newest heartbeat, and backfills it from `last_heartbeat_at` for
  every `QUEUED` or `RUNNING` run. That backfill is mandatory: a live run
  with a null column loses its liveness anchor, and the first sweep after
  the upgrade marks the whole fleet interrupted.
  `00013_tenant_placement_incarnation.sql` adds `tenant_placements.incarnation`,
  the `metadata.uid` of the tenant object the row describes. It defaults to
  the empty string, which means unknown, and a row keeps that value until
  its tenant announces again.
  `00015_runs_summary.sql` adds the nullable `runs.summary`, the one-line
  text a run attaches to itself through `varroa.summary`. It carries no
  default and no index, and no reader treats NULL as an empty string.
  `00016_runs_inputs.sql` adds the nullable `runs.inputs`, the pending input
  steps a `run.input` event reports. It carries no default and no index, and
  an empty set stores NULL, so no reader tells NULL from an empty array.
  `00017_run_test_cases.sql` adds the build-record table `run_test_cases`,
  one row per reported failed or skipped case, keyed
  `(run_id, suite, class_name, name)`. It carries `cluster`, `tenant`, and
  `job_path` for the case history index, no CHECK on `status`, no
  partitioning, and no foreign key to `runs`. It also adds the nullable
  `runs.test_report_key`, the blob key of the archived test report.
  `00018_runs_replay_of.sql` adds the nullable `runs.replay_of` uuid, the
  run a replayed build copied its script from. Only `run.started` writes
  it, through `COALESCE`, and it carries no default and no index.
  `00019_fleet_locks_owner_tenant.sql` adds `fleet_locks.owner_tenant`,
  the tenant that owns the lock, backfills it from each name's first
  segment, and indexes `(cluster, owner_tenant)`.
  `00020_run_stages_drop_log_columns.sql` drops `run_stages.log_offset`
  and `run_stages.log_length`. No writer filled them; a per-stage log
  resolves through the stage tags of the log index.
  `00021_runs_trigger_failed_intent_key.sql` adds the partial index
  `runs_trigger_failed_intent_key_idx` on `runs (intent_key)` for
  `TRIGGER_FAILED` rows only. A query must name the same literal
  predicate to use it.
  `00022_tenant_placements_agent_namespace.sql` adds
  `tenant_placements.agent_namespace`, the tenant's agent namespace as its
  own cluster announces it. It defaults to the empty string, and every
  announce overwrites it with the announced value.
  `00023_run_intents_granted_at.sql` adds the nullable
  `run_intents.granted_at`, the time of the last grant to the instance
  `cell_instance` names. A null value counts as an old grant.
  `00024_tenant_placements_retired_incarnations.sql` adds
  `tenant_placements.retired_incarnations`, a jsonb list of the
  incarnations a take-over replaced, newest first. It defaults to `[]`.
  A later migration never edits an applied one. `migrationsFS` (`migrations.go`) embeds
  every file under `migrations/` into the binary through `embed.FS`, so
  the recorder needs no separate deploy artifact. Monthly partition
  names on `runs`, `log_chunks`, and `activity` follow
  `<table>_p<YYYYMM>`; the retention round that adds later partitions
  must use the same pattern. The schema holds exactly thirteen tables, no
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
  test drops and recreates schema `varroa` during setup, so a repeated run
  starts clean. The `blob` test binary runs at the same time and owns a
  sibling database, `<db>_blob`, that it creates on first use; no other
  test binary may share the DSN's database with this harness. Seed a
  monthly-partitioned table only inside the window its migration creates
  (`00001` for `runs` and `log_chunks`, `00002` for `activity`). That
  window runs from the previous month to two months ahead of the clock.
- **Cluster-wide roles in a grants test** (`grants_test.go`) — a Postgres
  role is instance-wide, not per-database. A grants test creates
  `varroa_client`/`varroa_gateway` only when the role does not already
  exist, and cleans up with `DROP OWNED BY <role>` scoped to its own
  database. It never runs `DROP ROLE`, even conditionally: several test
  databases on one shared instance (parallel worktrees) may hold grants
  to the same role, so dropping it fails with a dependency error for a
  database that still holds one, and a conditional drop races a
  concurrent worktree's own check-then-grant sequence. The role-absent
  behavior is asserted only when the role happens to already be absent,
  which every CI job's fresh instance guarantees on its first run.
- **`blob/`** — the storage abstraction for build logs and build
  artifacts, behind `gocloud.dev/blob`: the `pgblob` driver for tier one
  and S3/GCS/Azure/file wiring for tier two. See its own `AGENTS.md`.
- **Responder subscriptions** — every `Register*Responder` (claim, owner,
  tenant purge, tenant sync, cross-tenant, fleet lock release) subscribes
  through `bus.Conn.ServeRequests` and returns a `*bus.Responder`. NATS can
  deny the subscription at start or remove it on an authorization reload.
  The `Responder` subscribes again, so the recorder needs no restart.
  The two pull consumers (`RunIngestConsumer`, `RunActivityIngestConsumer`)
  need no such path. Their only subscription is the private
  `_INBOX_recorder.>` inbox, whose grant no deploy changes. Each fetch is a
  new publish, so a denied fetch times out and the loop retries by itself.
  A denied ack causes a redelivery after the ack wait.
- **Ledger and claim responder** (`ledger*.go`) — the design writes the
  claim types as `ledger.ClaimRequest`/`ledger.ClaimResponse`; this
  package holds them as `ClaimRequest`/`ClaimResponse` with no separate
  `ledger` subpackage. `Ledger.Claim` answers one claim in a single
  transaction: it inserts a new intent, or applies design D2's
  decision table to an existing one, then reserves a build number
  through `reserveBuildNumber`. **A repeat claim for the same run
  identifier answers `duplicate` once the run started** (`intentStarted`:
  the intent is `CONSUMED`, or a `runs` row records the build), and
  writes nothing: only a queue item claims, so such a claim is a
  redelivered command, and `granted` would start the run again on a
  replacement pod. That answer skips `markDuplicateClaim`, because the
  row it would mark is the started run's own row. `reanswerSameRun`
  serves only a run that has not started, which keeps the re-fire and
  the ownership-move refresh working. It also reserves a fresh number, from any cell
  instance, for a `GRANTED` intent whose number is null (a `run.started` took it) or
  whose number equals the request's `number_conflict` and sits below the watermark.
  **The number Jenkins used is the truth at `run.started`**: `takeUnstartedGrant`
  (`ingest_started.go`) nulls another unstarted `GRANTED` intent that holds the
  reported number before the write; only a holder that already started leaves the
  unique-constraint retry, which stores a null number and logs both run ids.
  `granted_build_number` on the payload is compared alone and counted in
  `recorder_ingest_number_mismatch_total`. `ClaimBudget` (3.5s) bounds one claim so
  the recorder answers inside the gateway's 4s. A resumed build never claims.
  **The start mark** (`ledger_start_mark.go`, `run_intents.start_marked_at`,
  migration `00025`): a `ClaimRequest` with `StartMark` set goes from `Claim` to
  `MarkStart`, which checks the fence, locks the intent row, and stores the mark only
  for the stored owner of the stored run; any other caller reads `duplicate`. The
  plugin releases a queue item only after a granted mark, so a null mark proves no
  start. A marked intent never moves: a repeat from another instance answers
  `duplicate` before the hold, Row 3 skips it, and the re-fire skips it. `EndMarkedLostIntents`
  ends its run once the owner is confirmed gone.
  `recorder_start_marks_total` counts the answers by `decision`. **A repeat from another
  instance for an unmarked intent waits on the grant hold** (`ledger_grant_hold.go`): a same-`run_id`
  repeat of a `GRANTED`, unstarted intent from any instance other than the
  named owner, an unnamed one included, answers `NOT_READY` and writes nothing. Release test (a)
  is a pod-read `confirmed_gone_at` for the owner older than the
  confirmed-gone grace, which the hold raises to the ingest `ackWait` when
  it is shorter. Release test (b) is a `granted_at` at least
  `movedHeartbeatWindow` old. A release also needs a fresh `varroa_builds`
  consumer: `sweepConsumerSkip` passes and `NumRedelivered` is zero. An
  unreadable consumer keeps the hold. **The consumer read never runs inside
  a claim transaction**: `holds` answers `errConsumerReadNeeded`, `Claim`
  rolls back, reads the consumer under the claim's context, and runs
  `claimAttempt` again with the reading. Test (b) compares `granted_at` with
  the database clock that `existingKeyDecision` reads; test (a) and the
  consumer test use the recorder clock, as the re-fire does. `Ledger.SetGrantHold`
  wires it, `cmd/recorder`'s `wireGrantHold` is the only caller, and a
  `Ledger` it never touches holds nothing. `Fence` gates a claim before the
  transaction opens; `allowAllFence` is the C1 default.
  `RegisterClaimResponder` subscribes `bus.LedgerClaimSubject` with
  queue group `recorder`. `handleClaim` (`ledger_responder.go`) logs a
  vetoed decision at info level, once, as `claim refused` with
  `reason_code`, `tenant`, `run_id`, `job_path`, `cell`, and `message`.
  It never logs `params_digest`, `actor`, or `cause`. `ClaimMetrics`
  (`ledger_claim_metrics.go`) counts every veto in
  `recorder_claims_vetoed_total`, labelled `reason`, the decode-failure
  `INVALID_KEY` included; a nil `*ClaimMetrics` counts nothing.
  **A permanent veto marks the vetoed run at once** (`claim_veto.go`).
  After the refusal log, `handleClaim` calls `Ledger.markVetoedRun`
  before it replies. The mark runs in its own transaction, after the
  claim transaction rolled back, so it holds no placement row lock. It
  marks only a code `markableVeto` admits (`FENCED`, `CROSS_TENANT`,
  `INVALID_KEY`, `THROTTLED`). That set mirrors the plugin's
  `VETOED_OTHER` cancel branch, so change both together. It never marks
  on `NOT_READY` or `NOT_HOME`. It marks only a claim with origin
  `VARROA_INTENT`, so a Jenkins-native trigger gets no synthetic row. It
  always names `req.RunID`, never `resp.RunID`: a `FENCED` answer from
  the intent table names the stored run. One upsert on `runs` covers
  both orders of the race with `run.trigger_accepted`: the insert writes
  the failed row, and the update marks a `QUEUED` row. Both halves apply
  `unclaimedQueuedRunSQL`, the age-free half of the claim-expiry guard,
  so a run with an intent or a build number stays as it is. The same
  transaction writes one `build.claim_vetoed` activity row through
  `insertRunActivityTx`. After the commit it publishes one run
  notification, and the responder counts the mark in
  `recorder_claim_veto_marked_total`, labelled `reason`. A failed mark
  logs at `WARN` and never changes the answer. A `Ledger` that
  `SetVetoMarking` never touches marks nothing. A `duplicate` decision
  also marks the
  requesting run's own row `DUPLICATE` through `markDuplicateClaim`, in
  the same transaction and guarded on `status = 'QUEUED'` plus
  `noBuildRecordedSQL`: that run never builds, and the interrupted sweep
  leaves a QUEUED row alone while its accepting instance is alive, so
  nothing else would ever resolve it. `applyTriggerAccepted` writes the
  same row `DUPLICATE` at once when `run_intents` already holds its
  intent key under another run identifier, because an accepted trigger
  can land on either side of its claim. A forwarded build keeps its run
  identifier, so a second cell can accept the same run: a newer accept
  from another cell moves a `QUEUED` row that still holds `accepted_at`
  onto its cell, namespace, and accepted keys. An older accept never moves
  the row back. `NextBuildNumber` and
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
  pure, twelve-order table: the row, the member namespace, the member
  cell, the announced instance, the standby test, the home slot test,
  the two generation orders, the cross-tenant test, and the home
  member's own seed state (`rehydrated_instance` and `pending_jobs`). A
  missing row answers `NOT_READY` with no exemption. A claim from a
  standby or a non-home slot answers the permanent `NOT_HOME`, and that
  veto alone carries `FenceResult.HomeSlot`, which `Ledger.Claim` copies
  onto `ClaimResponse.home_slot`. A `NOT_HOME` veto writes no
  `run_intents` row. The
  generation orders answer the retryable `NOT_READY`, because orders 2
  and 3 already refuse a cell the tenant has left, so a request that
  reaches them comes from a live member with a stale resolver cache. `FenceMetrics`
  holds `recorder_fence_veto_total`, labelled by reason; a
  `PlacementFence` built with a nil `*FenceMetrics` counts nothing.
  It counts fence vetoes only; `recorder_claims_vetoed_total` counts
  every veto.
- **Tenant purge responder** (`tenant_purge.go`) — `RegisterTenantPurgeResponder`
  subscribes `bus.TenantPurgeSubject` with queue group `recorder`. It answers
  the placement pass's request to delete one tenant's `job_schedules` rows
  after the tenant leaves a cell: it deletes only the rows matching the
  request's cluster and tenant, and whose stored `placement_generation` is at
  most the request's, so a delayed retry can never remove a row the tenant's
  new cell already reported. A request that names a cell narrows the delete
  to that cell's rows, so a slot move purges one member and leaves every
  other slot's rows; the narrow delete tests `namespace` **and** `cell`,
  because a row carries both and two namespaces can hold a cell of one name.
  A request that names a cell and no namespace is refused, rather than
  deleted on the name alone. A teardown names no cell and keeps the
  whole-tenant delete. The delete is a plain `DELETE`, idempotent by
  construction. A teardown also resets that tenant's
  `tenant_placements.generation` to zero
  (`resetTenantPlacementGeneration`), under the same at-most test, because
  the announce never lowers a stored generation and a `Team` created again
  under the old name restarts at generation 1. The row itself survives the
  reset: the interrupted-run sweep still reads its cell-liveness fields
  for a run the teardown left behind. A purge that names a cell is a slot
  move and resets nothing. **A teardown tests the incarnation first, and
  one transaction holds both of its writes** (`purgeTornDownTenant`): the
  request carries the `Team`'s `metadata.uid`, the purge reads the row's
  `incarnation` column under `FOR UPDATE`, and a request that names
  another incarnation than the stored one logs at info and writes nothing.
  A teardown purge can arrive up to ten seconds after the finalizer
  clears, and a tenant of the same name created in that window owns the
  row by then. Its own `job_schedules` rows sit at generation 1, which the
  old tenant's generation test admits, so the incarnation test is the only
  thing that holds them. A refused purge writes nothing at all: the
  take-over announcement already deleted what the old tenant left (see the
  tenant sync responder below). An empty value on either side means
  unknown and matches, and a tenant with no placement row is purged too,
  because the schedule rows can outlive it. The operator side of this
  contract lives outside this package.
- **Tenant sync responder** (`tenant_sync.go`, `tenant_sync_types.go`) —
  `RegisterTenantSyncResponder` subscribes `bus.TenantSyncSubject` with
  queue group `recorder`. It answers two request kinds: `announce`
  upserts the tenant's `tenant_placements` row (never lowering
  `generation`, except on a take-over: an announcement whose `incarnation`
  differs from the stored one comes from a tenant created again under one
  name, so it adopts the announced generation, even below the stored one,
  and clears every seed like a generation raise — without it the new
  tenant would wait for the teardown purge to reset the row. A row that
  names no incarnation takes the second proof instead: an announced
  generation **below** the stored one, because a tenant never lowers its
  own generation. That second test is what keeps a row written before the
  column existed from fencing the tenant that follows it.
  `keptIncarnation` never lets an announcement naming none erase the
  stored value. **A take-over retires the stored incarnation**
  (`retireIncarnation`): it adds that value to `retired_incarnations`.
  The list keeps eight entries and has no time limit, because a
  `metadata.uid` never comes back. A teardown purge retires nothing,
  because a `Team` that drops `spec.pipelines` keeps its uid and can add
  the field again. **A take-over deletes every `job_schedules` row of the
  name** (`deleteTenantJobSchedules`), inside the same transaction: a cell
  exports a tenant's jobs only after the announce that placed it, so at
  that moment every row belongs to the tenant before this one, and a row
  left behind keeps firing the old tenant's cron. The first announcement
  to stamp a row that names no incarnation runs the same delete, once per
  tenant, because such a row can hold a gone tenant's rows at the very
  generation this tenant announces; the live cell exports its own set
  again on its next poll; a generation-zero row seeds itself from the announced
  cell instance at once; a generation raise or a cell-instance change
  clears `rehydrated_instance` and `pending_jobs`, because the new cell
  holds no seed yet; at the same generation and the same instance
  `keepAppliedSeed` keeps a stored `rehydrated_instance` over an empty
  report, for the scalar and for each member, because the operator can
  read its own rehydration state one tick late and an empty report never
  proves a seed gone. **`keepDeferredJobs` holds the two halves of one
  seed report together**: a report `keepAppliedSeed` judged stale never
  clears a deferred job either, because the cell clears its own
  `Deferred` list and reports `Applied` false together
  (`preSendEntry`) when it starts a fresh bundle, and taking that list
  would admit the job through order 10 **and** order 11 with no record on
  the cell. `resolveAnnouncedMembers` mirrors that whole rule per member,
  **the generation arm included**: a teardown and a new tenant of one name
  can share a cell and a live pod, so the instance test alone would hand
  the new tenant the old one's seed) and merges
  `dead_instances` into the stored list,
  keeping every entry a `QUEUED` or `RUNNING` run names through
  `candidatesQuery`'s own owning-instance expression
  (`run_intents.cell_instance`, else `payload->>'accepted_instance'`),
  every entry a pending re-fire names, and every entry a `held` or
  `releasing` fleet lock's `cell_instance` names (the run of a releasing
  lock is `INTERRUPTED`, so the run arm misses it), and trimming the rest
  to ten. **Each entry names its `cell` and `namespace`**; the merge keeps
  a stored cell when the incoming entry names none, and the announce reply
  answers `unconfirmed_dead_instances` (`unconfirmedDeadInstances`): every
  stored entry with a cell and no `confirmed_gone_at`. The stored row is
  the operator's durable list of pods to read again; `records` calls
  `ListRehydrationRecords`. An unknown kind answers an error and writes
  nothing. The wire types (`TenantSyncRequest`, `TenantSyncResponse`,
  and the nested job/record shapes) live in `tenant_sync_types.go`,
  beside `ledger_types.go`, because this responder dispatches on them.
  **The announce also derives dead instances** (`dead_instances_derive.go`).
  `deriveDeadInstances` compares the stored member list with the announced
  one inside the same transaction. A member whose cell reports a new
  instance leaves its old instance, tagged `derived: rolled`. A member whose
  cell the request drops leaves its instance, tagged `derived: dropped`.
  Each entry carries the recorder's `now` as `last_seen_at` and no
  `confirmed_gone_at`, so the moved window and the anchor test still apply.
  The function never adds an instance that the merged list holds or that
  the request names, so a repeat adds nothing. **`staleAnnounce` refuses a
  stale announce before any write**: a take-over is never stale; otherwise a
  generation below the stored one, or a request that names an instance the
  stored list proves replaced (`confirmed_gone_at` set, or `derived:
  rolled`) and no stored member holds. A request that names a retired
  incarnation is stale before the take-over test, so a late announcement
  of a replaced object can never take the row back; the log line carries
  `reason=retired incarnation`. A `dropped` entry proves nothing,
  because a tenant can return to the live pod of a cell it left. A request
  that confirms a stored member's instance gone is never stale on that
  test. A stale announce writes no column, runs no sweep, and answers
  `stale: true` with no `ok`, so the operator caches nothing and writes no
  placement status. An operator entry for the same
  instance clears the tag in `mergeDeadInstances`.
  **An accepted announce removes every unconfirmed entry for an instance
  it names** (`pruneNamedUnconfirmed`, between the merge and the
  derivation): the request proves that pod live. A confirmed entry stays.
  Without the removal, a `rolled` entry for a live slot-1 pod releases its
  fleet lock. A returned pod that rolls later is then never derived again.
  `trimDeadInstances` sorts its input before it trims, because the derived
  entries join the end of the merged list.
  `fleetLockOwnerConfirmedGone` counts an entry only with a confirmation
  older than the grace, or with the `rolled` tag past the moved window. Any
  other entry with no confirmation releases no lock, because the cell the
  tenant left can still be alive.
  `handleTenantSyncAnnounce` runs `MarkMovedRunsForTenant` when `cell` or
  `cell_instance` changes or the derivation added an entry, and runs `ReleaseConfirmedFleetLocks`
  (`fleet_lock_release.go`) on every announce, against the same
  `tenantPlacementLivenessRow` the announce just wrote: a lock's second
  release confirmation can become newly true on any announce, not only a
  cell or an instance change, because `cell_alive` can flip on its own.
  A confirmation counts only once the grace elapses, so the announce that
  brings it never releases the lock: `ReleaseConfirmedFleetLocksRound`
  repeats the check once a minute in the backstop round, against the
  stored row of every tenant with a releasing lock.
- **Cross-tenant responder** (`crosstenant_*.go`) — `RegisterCrossTenantResponder`
  subscribes `bus.LedgerCrossTenantSubject` with queue group `recorder`. It answers
  eight kinds behind one `CrossTenantRequest`/`CrossTenantResponse` pair:
  `build` (`crosstenant_build.go`) reserves a run identifier against the ledger
  through `Reader.GetIntentByKey`, minting a fresh UUIDv7 and publishing one
  `LedgerActionTriggerRun` action (actor `svc-<caller-tenant>`) only when no
  `run_intents` row exists yet, and never writing that row itself. The `build`
  kind is the one kind that reads `CrossTenantRequest.IntentKey`, and only when
  `CallerTenant` equals `TargetTenant`: an intra-tenant forward preserves the
  claimer's own key, so two members that fire one cron in one minute reach one
  ledger row. A request with a supplied key is a forward, and it keeps its
  `caller_run_id` as the run identifier (`INVALID_KEY` when that value is
  not a UUIDv7): the API answered that identifier, so a minted one leaves the
  answered run `QUEUED`. A forward answers `NOT_READY` and publishes nothing
  when the home slot holds no cell or the home is the calling cell, because
  its claimer cancels its item on an answer with no reason code. `crossTenantSuppliedKey` validates it with `ParseIntentKey`
  (`reason_code=INVALID_KEY` for an unparsable key) and answers `code=INTERNAL`
  for a cross-tenant request that carries one, because grant order 8 tests the
  scheme of a key the recorder derived. `status` and
  `last_successful` (`crosstenant_status.go`) and `artifact_list`/`artifact_url`
  (`crosstenant_artifact.go`) read a run's tenant and job path
  (`crosstenant_runrecord.go`) and run the `request`-path grant against them
  through `GrantEvaluator.Check` (`grant_evaluator.go`) before answering.
  A run that failed to trigger holds no `run_intents` row, so with no intent
  row `readCrossTenantRun` reads its `TRIGGER_FAILED` `runs` row by
  `run_id` or by `intent_key`, in the same statement as the intent read so
  a redelivered trigger's new intent always wins. The grant then runs against that
  row's tenant and job path, and a `status` reply carries
  `trigger_reason_code` and `trigger_error` apart from `reason_code`,
  which vetoes the request itself;
  `artifact_url` signs the recorded `blob_key`
  through `blob.Store.SignedURL`, mapping `blob.ErrUnsupported` to a message
  naming `recordStore.blobs.mode`. Every reply's `error`/`code` pair follows the
  claim path's rule: `UNAVAILABLE` for an unanswering Postgres, `INTERNAL` for
  every other fault, through the shared `classifyDBError` (`ledger.go`). One
  code is added: `NOT_FOUND` (`CodeNotFound`) answers an unknown `run_id` on
  `status` and both artifact kinds, and an `artifact_url` path the run did not
  record. A blob-mode refusal and a malformed request stay `INTERNAL`.
  `CrossTenantMetrics` (`crosstenant_metrics.go`) holds
  `recorder_crosstenant_requests_total` (labelled `kind`/`outcome`, the
  outcome one of `allowed`, `denied`, `error`, or `not_found`) and
  `recorder_crosstenant_grant_denied_total` (labelled `reason`, from
  `crossTenantGrantDenialReason`'s match on the veto message's added
  clause, since `GrantResult`'s shape stays fixed to the `FenceResult`
  fields `ledger_fence_placement.go`'s direct type conversion depends
  on); `dispatchCrossTenant` records both on every call.
- **Fleet locks** (`fleet_lock_acquire.go`, `fleet_lock_renew.go`,
  `fleet_lock_release.go`, `fleet_lock_metrics.go`,
  `reader_fleetlocks.go`) — the cross-tenant responder's
  `lock_acquire`/`lock_renew`/`lock_release` kinds over
  `varroa.fleet_locks` (migration `00006`). `reader_fleetlocks.go`
  answers the two reads a consumer sees the table through:
  `Reader.ListFleetLocks` answers every row whose `owner_tenant` equals
  the tenant plus every row its own runs hold or wait on, by equality and
  never by a name prefix, and
  `Reader.ListAllFleetLocks` answers every cluster, because the one-holder
  index covers `name` alone and a holder can sit on another cluster than
  its waiters. Both order by
  `(name, waiting_since, run_id)`, so a holder sorts before its
  waiters, and neither answers a queue position. `resolveLockTarget`
  (`fleet_lock_acquire.go`) resolves one lock call: `name` is the bare
  name, `target_tenant` is the owner, and an empty `target_tenant` is the
  gateway-stamped `caller_tenant`. A name that is empty or holds a `/`
  answers the veto `INVALID_NAME` (`ReasonInvalidName`) and writes no
  row. Nothing splits a lock name: a controller tenant
  (`<namespace>/<controller>`) holds a `/` of its own, so the stored
  `name` is `<owner_tenant>/<bare>` and `owner_tenant` carries the owner.
  An acquire or a renew whose owner is not the caller also calls
  `GrantEvaluator.Check` with `resource_kind` `lock` and the bare name.
  The `lock_acquire` reply carries `tenant`, the resolved owner. `acquireFleetLock` runs the five-step transaction
  (the derived `held`-to-`releasing` move for an `INTERRUPTED` run, the expiry
  delete, the waiting insert, the FIFO promotion by `(waiting_since, run_id)`,
  and the reply) with the one retry `isFleetLockOneHolderViolation` names after
  a unique violation on `fleet_locks_one_holder`; a second violation answers
  `waiting` with no further attempt. A release deletes the caller's row,
  whatever its state, and carries no fence: only acquire and renew do.
  A run id is visible in the lock reads, so acquire, renew, and release
  each also match `tenant = caller_tenant`: the acquire's conflict arm
  refreshes only its own tenant's row (`errFleetLockRunOfOtherTenant`
  answers `CROSS_TENANT`), a renew of another tenant's row answers
  `lost`, and such a release deletes nothing.
  `moveInterruptedHoldersToReleasing` (`fleet_lock_acquire.go`) is the one
  shared implementation of the derived move, behind the `fleetLockExecer`
  interface, which declares `Query` beside `Exec`: the acquire
  transaction's own step calls it with one lock name, and the maintenance
  round's `sweepFleetLocks` (`retention_fleet_locks.go`) calls it with an
  empty name to move every held row of an interrupted run fleet-wide,
  before its own bounded-batch expiry delete, which skips a `releasing`
  row exactly as the acquire's own expiry delete does. It answers every
  moved row's own identity beside the count, because each moved row earns
  its own notification. A `releasing` row leaves the table on exactly two
  confirmations: an ingested terminal `run.completed` for the holder
  (`ingest_completed.go`'s own `applyRunCompleted`, gated on the owner
  test, in the same transaction as the `runs` write, so a crash after
  commit never strands the lock), or `ReleaseConfirmedFleetLocks`
  (`fleet_lock_release.go`), which the tenant-sync responder and the
  backstop round both run, and which reuses `cell_liveness.go`'s
  `deadInstanceGone`/`currentInstanceGone` readings with no anchor test.
  Beside the two confirmations, the holder's own `lock_release` and the
  operator force release delete a `releasing` row.
  **The force release** (`fleet_lock_force_release.go`,
  `RegisterFleetLockReleaseResponder` on `bus.FleetLockReleaseSubject`,
  queue group `recorder`) is the BFF's only path to a lock write. One
  transaction reads the row `FOR UPDATE` and answers `not_found`,
  `not_releasing` (a held or waiting row), `holder_live` (the stored
  placement names the row's `cell_instance` as a member, through
  `memberForInstance`), or deletes the row and answers `released`. The
  placement row is read without a lock, so it cannot deadlock with the
  announce. A request with no name, run id, actor, or reason answers an
  error and writes nothing. A failed commit answers an error reply marked
  `unknown`, because Postgres can have committed the delete. A release publishes one `lock` notification
  after the commit; the BFF writes the audit row.
  **Every lock write publishes one `lock` notification per changed row,
  after its own transaction commits, through the `RunNotifier` the
  recorder wires (`CrossTenantDeps.LockNotifier`,
  `MaintenanceConfig.LockNotifier`, `Ingester.SetRunNotifier`).** Each
  notification carries the changed row's own cluster, tenant, job path,
  name, and run id, read by `RETURNING` or written under the caller's own
  primary key, and never assumed from the caller's request. A statement
  that affects no row publishes nothing, so a repeat poll, a repeat sweep,
  and a second release all stay silent; a rolled-back attempt publishes
  nothing either. A renew publishes nothing (it changes only
  `expires_at` and `placement_generation`), and a mesh takeover publishes
  nothing (it changes only `cell_instance`), because neither changes a
  state a reader renders. `FleetLockMetrics`
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
  is a natural-key upsert. The kind set holds sixteen values; `run.state` is
  the mesh's own kind and writes no typed column, only `payload.mesh`.
  `run.command_refused` (`ingest_command_refused.go`) takes the early
  route beside `run.resume_failed`. `applyCommandRefused` merges one
  `payload.command_refusal` object (`command`, `actor`, `http_status`,
  `error`, `refused_at` from the envelope's `occurred_at`) in one
  `UPDATE`. The guard names the run's primary key **and** the envelope's
  `cluster`, `tenant`, and `job_path`, so a cell cannot write a notice onto
  another tenant's run. A newer stored `refused_at` wins. A run with no
  row is a no-op, never an error, and a later successful command never
  clears the notice. The command name must be one of `refusedCommands`,
  else the envelope is malformed. Only a stored notice publishes a `run`
  notification. The API's run detail reads the key as `commandRefusal`.
  `runsStatusMergeSQL` (`ingest_common.go`) is the
  one CASE expression every `runs` writer reuses to keep `status` monotonic
  and `DUPLICATE` terminal.
  `applyRunTests` (`ingest_tests.go`) also writes the payload's `cases` and
  `test_report` in the same transaction. A `cases` key replaces the run's
  whole `run_test_cases` set: a delete, then one `unnest` insert of the
  cases `dedupeCases` keeps. An absent `cases` key changes no case row.
  `test_report.blob_key` reaches `runs.test_report_key` only when it is
  not empty, and the size and the case count merge into `runs.payload`
  under `test_report`. The merge wraps `runs.payload` in `COALESCE`,
  because `jsonb || jsonb` answers NULL on a NULL operand.
  `applyRunHeartbeat` (`ingest_heartbeat.go`) raises the two liveness
  columns only for the run's own owner. The owner is the intent's
  `cell_instance`, else the stored `accepted_instance`, which is the
  expression the sweep reads. The target a dispatched `mesh.resume`
  names also raises them, because that target's first heartbeat proves
  the resume. A heartbeat from any other instance changes no column, and
  the recorder logs it at debug level. A heartbeat whose payload carries a
  `summary` value also raises `runs.summary` and the payload's
  `summary_seq` key, under the guard `summaryHeartbeatGuardSQL` states;
  one whose payload carries no `summary` value changes neither.
  One whose payload carries an `inputs` key also raises `runs.inputs` and
  the payload's `inputs_seq` key, under the guard
  `inputsHeartbeatGuardSQL` states, only when its set differs from the
  stored one, and never on a completed row; one whose payload carries no
  `inputs` key changes neither.
  Two cases hold the unconditional
  raise, because the test needs a name on each side: an envelope that
  names no sender, and a run that no source names an owner for. Without
  the owner rule a dead owner's
  in-flight heartbeat raises the stamp above `mesh.resume.requested_at`.
  The run then stays `RUNNING` for good, because nothing lowers the
  column again. `ingest_duplicate.go` holds the duplicate
  backstop `run.started` runs in the same transaction, publishing a
  `cancel_run` ledger action (`ledgeraction.go`) through
  `PublishLedgerAction` on `bus.LedgerActionSubject`. Every action's
  `Cluster` names the target cell's cluster, because only that cluster's
  operator subscribes; `PublishLedgerAction` refuses an empty `Cluster`.
  `RunIngestConsumer`
  (`ingest_consumer.go`) is the durable pull consumer named `recorder`
  that applies D4's disposition table; `EnsureBuildsStream`
  (`ingest_stream.go`) creates the `varroa_builds` stream, and
  `cmd/recorder` is the only caller. `IngestMetrics`
  (`ingest_metrics.go`) holds the `recorder_ingest_*` instruments on
  `otel.Meter("varroa-recorder")`. The `job.schedules` kind rides the same
  `varroa_builds` stream and the same `recorder` consumer as every
  `run.*` kind; it carries an empty `run_id`, so `Ingester.Apply` skips
  the intent lookup for it. The `run.trigger_failed` kind takes the same
  early route, before `uuid.Parse` and before `loadIntent`: a failed
  trigger writes the only record of its run, so no intent can exist
  before it. `applyTriggerFailed` (`ingest_trigger_failed.go`) inserts
  one `runs` row stamped with the UUIDv7 time of its own `run_id`, under
  `ON CONFLICT (created_at, run_id) DO NOTHING`. That pinned
  `created_at` gives a real row and a synthetic row one primary key, so
  the conflict clause alone decides which write wins. A real event
  supersedes a stored `TRIGGER_FAILED` row in place:
  `supersedeTriggerFailedSQL` (`ingest_common.go`) is the predicate, and
  `runsStatusMergeSQL` takes its second half as a parameter (`TRUE` in
  `applyRunStarted`, the owner flag in `applyRunCompleted`, so a foreign
  completion supersedes nothing). The recorder runs no separate delete,
  because two statements cannot see each other's uncommitted work.
  `ingest_job_schedules.go` upserts every row
  the envelope carries, row-locks the cell's `job_schedule_cells`
  watermark inside the same transaction, rejects an envelope whose
  `observed_at` is not newer than the stored value, and deletes every row
  of the cell older than the accepted envelope, but only when the
  envelope is not truncated. The delete matches the stored cell. A home
  write moves a job's rows to the new home at once, so the old home's
  next envelope skips the job and deletes none of its rows. The gate
  stores a row only when the envelope's namespace **and** cell equal the
  job's home member, because two namespaces can hold a cell of one name.
  The ingest
  reads each tenant's home `FOR SHARE` (`jobScheduleHomeCell`), so a home
  write waits for an envelope that already read the old home. The upsert keeps passed occurrences in
  `next_fire_times`: with an unchanged spec, time zone, and disabled
  state, it merges each
  stored occurrence at or before the envelope time and newer than
  `passedOccurrenceWindow(cronGrace)` (`max(3 × cronGrace, 2 × cronGrace +
  backstopAnswerWait) + 2m`, so a lost fire still finds its occurrence
  when its suppression ends), at most
  `maxPassedOccurrences` (16), with the carried list, deduplicated and
  sorted. The backstop needs that: the plugin exports future times only,
  so a plain replace removes a miss before the round can judge it.
  `cronGrace` comes from `readTenantSchedulePolicy` on the envelope
  transaction, cached per tenant; that function takes any
  `schedulePolicyQuerier`, a pool or a transaction. `IngestMetrics` also holds
  `recorder_schedules_truncated_total` and `recorder_schedules_stale_total`.
  `applyRunCompleted` (`ingest_completed.go`) reports `owner`, whether its
  write was the run's real terminal write, not a foreign instance's
  payload-only merge; on `owner`, in the same transaction as the `runs`
  write, it deletes this run's `held`/`releasing` fleet_locks rows and
  reports each deleted row's own identity, the first of the two release
  confirmations `fleet_lock_release.go`'s own bullet names. `Apply`
  carries those rows onto `ApplyResult.ReleasedFleetLocks`, and
  `Ingester.notify` publishes one `lock` notification per row beside the
  run notification, whether or not the kind earns one. `Ingester.SetUpstreamFanout` (`ingest_apply.go`) is an
  optional setter, matching `Ledger.SetConcurrencyCancel`'s own
  convention: an `Ingester` no caller wires runs no fan-out. When wired,
  `Apply` calls `RunUpstreamFanout` (`upstream_fanout.go`) after the
  transaction that recorded an owner `run.completed` write has committed,
  never inside it, because the round reads `GrantEvaluator` over several
  queries and publishes on the bus. **The round is the one part of the
  terminal path that is not idempotent**, because it mints a fresh run
  identifier for each subscriber, so `dispatch` arms it only behind
  `claimFanoutRound`: an at-most-once `payload.fanout_at` marker written
  in that same transaction. The build stream is at least once, and a
  repeat delivery would otherwise publish a second `trigger_run` under
  one intent key, which the target's claim answers `duplicate`.
  **The round selects two candidate sets.**
  Set A holds a `varroaUpstream` subscription: rows matched by the producer's
  relative job name (stripped of the producer's own tenant prefix, matching
  `upstream_job`'s own storage convention), through the
  `(upstream_tenant, upstream_job)` index. Set B holds a native Jenkins
  `upstream` trigger inside the producer's own cluster and tenant, through the
  `(tenant, trigger_type)` index; Go then matches each row's `spec`, the
  comma-separated list of resolved upstream job paths the plugin exports,
  against the producer's **tenant-qualified** path. Set B must not reuse set
  A's relative form: a full path never equals it, and every native edge would
  miss. A listed path whose first segment is another tenant is dropped, and a
  subscriber that holds at least one such path and matched none of its own
  counts one `cross_tenant` skip. Each set applies the Jenkins result threshold
  inside its own selection. The joined list then takes the placement test
  (`resolveFanoutTargetCluster`'s own mirror of
  `GrantEvaluator.resolveTargetPolicy`'s cluster resolution) and the disabled
  test, is de-duplicated by `(cluster, tenant, job_path)` **after** those two
  tests, orders by `(cluster, tenant, job_path)`, and caps at 32. Filtering
  before the de-duplication is what lets a job that declares both kinds build
  when either declaration passes. The cap covers the de-duplicated list, never
  one set. The round then runs the `fanout`-path
  `GrantEvaluator.Check` per row and publishes one `LedgerActionTriggerRun`
  per allowed row, actor `varroa-upstream/<producer-tenant>`. The action's
  namespace and controller come from `resolveJobHome` for the subscriber
  job, never from the row. A home slot with no cell publishes nothing and
  counts `unplaced` with a warning; the round cannot retry it, because
  `fanout_at` is already written. Only a tenant with no placement row keeps
  the row's own coordinate. A set B candidate
  always passes, because its caller tenant equals its target tenant. The
  intent-key seq is `t<digest>`, the first twelve hex characters of the
  SHA-256 of the subscriber's own `<cluster>/<tenant>/<job_path>`
  (`upstreamFanoutSeq`), stable across a redelivery because it never
  carries a position. `UpstreamFanoutMetrics`
  (`upstream_fanout_metrics.go`) holds `recorder_upstream_fanout_total`
  and `recorder_upstream_fanout_skipped_total` (labelled `reason`, over
  `over_cap`/`denied`/`unplaced`/`disabled`/`publish_failed`/`cross_tenant`).
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
  (`Delete` plus `DeletePrefix`), a narrow interface over `blob.Store`, and
  call `blob.LogKey`/`blob.LogIndexKey` for the two fixed log keys.
  `deleteExpiredChildRows` (`retention_childrows.go`) deletes `run_stages`,
  `run_tests`, and `run_test_cases` rows past the `retention.runs` cutoff,
  in bounded batches.
  `deleteExpiredRunState` is the mesh's hourly backstop: it removes the
  run-state objects of every run terminal for more than an hour, except
  the kept build.xml, over its own bounded query, because the log step
  reads a different cutoff. The prompt delete runs after the terminal
  commit instead, from `Apply` and from each `markInterrupted` caller.
  `deleteRetainedRunScripts` deletes the whole run-state prefix of every
  run past the `retention.runs` cutoff, before the `runs` partition drop. `finalizeStaleLogs` (`retention_finalize.go`) is step
  10, the log-finalization sweep C3's `build-log-externalization`
  capability defines; it never lowers a stored `finalized` true. The
  exported `Liveness` interface answers whether a run's owning cell is
  gone; `HeartbeatLiveness` is C1's heartbeat-only implementation
  (15-minute bound, no production call site), and `PlacementLiveness`
  (`retention_liveness.go`) is the placement-aware one `cmd/recorder`
  actually builds, reading `tenant_placements` through the same
  `cellLivenessGone` reading `interrupted.go`'s sweep uses. Both read
  `RunLiveness.HeartbeatReceivedAt`, never the pod's own stamp. `sweepFleetLocks`
  (`retention_fleet_locks.go`) runs after `finalizeStaleLogs`: it moves
  every `held` fleet_locks row of an `INTERRUPTED` run to `releasing`
  fleet-wide, then deletes every expired row that is not `releasing`, in
  `deleteBatched`'s own bounded batches; see the Fleet locks bullet for
  the shared move it calls. This sweep does **not** use `deleteBatched`:
  it calls `deleteExpiredFleetLocksNotifying`, which issues the same
  bounded delete through `Query` with `RETURNING` and publishes one `lock`
  notification per deleted row, and drains each batch's rows before the
  next statement runs on the round's one held connection.
  `MaintenanceConfig.FleetLockMetrics` counts
  the round's own expired-row deletes, and `MaintenanceConfig.LockNotifier`
  publishes the moved and deleted rows' own notifications. Step 5a
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
  the confirmed test, then the moved test, then the silent test, to one
  `QUEUED` or `RUNNING` candidate; `cellLivenessGone` is the one shared
  cell-liveness reading both this package's sweep and
  `retention_finalize.go`'s log-finalization sweep call, through
  `PlacementLiveness`. It answers a `livenessReading`
  (`livenessAlive`/`ConfirmedGone`/`Moved`/`Silent`), not a bool, because
  the reason depends on which test fired; `retention_liveness.go`
  compares against `livenessAlive` alone, so that seam is unchanged.
  **The confirmed test is the one signal that rests on a pod read.** It
  holds when the owning instance's `dead_instances` entry carries a
  `confirmed_gone_at` older than `confirmedGoneGrace`
  (`DefaultConfirmedGoneGrace`, 30s; chart
  `recordStore.cellLoss.confirmedGoneGrace`, env
  `VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE`), and it applies **no anchor
  and no window**: a run's heartbeat age proves nothing once the operator
  has proved the pod gone. It names `ReasonCellLost` (`cell-lost`), which
  `afterMark` deliberately excludes from the `cancel_run` publish — the
  pod is gone and holds no process to stop — and which
  `MarkMovedRunsForTenant` deliberately does **not** skip, unlike
  `no-heartbeat`. An entry with no `confirmed_gone_at` keeps the
  five-minute and fifteen-minute windows exactly. The grace is threaded as
  an argument through four seams (the sweep, `PlacementLiveness`,
  `ReleaseConfirmedFleetLocks`, `BackstopConfig.ConfirmedGoneGrace`),
  never a package variable, so no test mutates global state.
  `mergeDeadInstances` keeps the **earlier** `confirmed_gone_at` on a
  collision while it keeps the newer `last_seen_at`: the operator repeats
  the entry on every tick, and a later-wins rule would hold the timestamp
  young forever. `candidatesQuery` selects both
  statuses in one query: a `QUEUED` candidate's owning instance is the
  ledger's `cell_instance` when an intent exists, else its
  `payload->>'accepted_instance'`, and its liveness baseline is
  `runs.heartbeat_received_at`, else that `accepted_at` payload value,
  else `created_at` (`livenessBaseline`). A candidate with no owning instance from either
  source is skipped and counted in `recorder_sweep_skipped_total` with
  reason `no-owning-instance`, in both round loops. `markInterrupted`
  conditions its update on the status the round observed, `QUEUED` or
  `RUNNING`, never a literal, and leaves a `QUEUED` row's accepted keys in
  place. `MarkMovedRunsForTenant` is the fast path a `tenant.sync` announce
  triggers when `cell` or `cell_instance` changes; `SweepInterruptedRuns`
  is the periodic path, gated on the recorder's own `varroa_builds`
  consumer through `sweepConsumerSkip`. **That gate tests a backlog,
  never quiet.** A pending count at or above `VARROA_SWEEP_MAX_PENDING`
  (default 1000) skips the round as `pending`. The `AckFloor.Last` age
  is applied **only while `NumPending > 0`**: a consumer with nothing
  pending has processed everything the stream holds, so an old or nil
  floor means no build event arrived lately, not lag. Skipping a quiet
  fleet was a live defect — with no other build running, nothing ever
  refreshes the floor, so a lost cell's runs waited for a build that
  never came.
  Neither inserts an intent or touches `fleet_locks`; `afterMark` returns
  before either publish for a marked `QUEUED` run — no build ever started
  to cancel, and the store holds no parameters to retry with — and
  otherwise publishes the `cancel_run` and automatic `retry_run` actions a
  mark triggers. The cancel names the run's own cell, which holds the
  process. The retry names the job's home member through `retryAddress`
  and `jobHomeOf`, never `runs.cell`: a home slot with no cell publishes
  nothing and counts `unplaced` in
  `recorder_retry_skipped_total`. A tenant with no placement row keeps
  the run's own coordinate. `recorder_interrupted_marked_total` carries a `status`
  attribute (`queued` or `running`) beside `path`, and
  `recorder_cell_loss_confirmed_total` counts each `cell-lost` mark by
  the same `path`. `liveCellInstances`
  (`tenant_sync.go`) is the dead-instances trim's keep-set: the union of
  every `QUEUED`/`RUNNING` run's own owning instance and every `GRANTED`
  intent with a null `refired_at`. **The first arm resolves that instance
  with `candidatesQuery`'s own expression, over the same left join**
  (`NULLIF(ri.cell_instance, '')`, else `payload->>'accepted_instance'`),
  so the keep-set and the sweep read one owning-instance source. A
  narrower keep-set trims away the only evidence the sweep can mark such
  a run with, and the run then stays `RUNNING` for good. The second arm
  exists because the first joins `runs`: an intent whose build never
  started holds no `runs` row, so a busy pool would otherwise trim away
  the evidence its re-fire needs.
- **Unstarted-intent re-fire** (`refire.go`) — `RefireLostIntents` is the
  one path that recovers a build the sweep can never see: a `GRANTED`
  `run_intents` row whose build never started writes no `runs` row at
  all. It publishes one `LedgerActionTriggerRun` carrying the **same**
  `run_id` and `intent_key`, so the repeat claim from the new cell takes
  `reanswerSameRun`'s existing ownership move and stale-build-number
  refresh; this package adds no action kind and changes no claim rule.
  Four rules gate it, in order: the owning `cell_instance` reads
  `livenessConfirmedGone` (a stream loss alone is never enough);
  `params_digest` is null, empty, or `emptyParamsDigest` (the store holds
  no parameters for a build that never started, so a re-fire would run
  each declared default — the refusal counts `parameterized` and a person
  reruns the job); `refirePlacement.ready()`, which **defers** a re-fire
  rather than dropping one, twice over — the operator clears its stored
  instance at the moment of the confirmation (`no-placement`), and a
  replacement pod reports its UID well before its mite holds a Jenkins
  token (`not-rehydrated`). **`rehydrated_instance == cell_instance` is
  the one placement read that proves the cell can accept a trigger**: the
  rehydration reload itself needs a token, so a rehydrated instance holds
  one. Without that test the trigger lands on a tokenless mite, the
  security realm answers 401, and the gateway reports
  `TRIGGER_REJECTED` (a live defect on `core`). Last, the conditional
  `UPDATE … SET refired_at WHERE refired_at IS NULL AND state = 'GRANTED'`
  changes one row, which is the whole idempotency mechanism and the only
  thing that authorizes the publish. The publish is not transactional
  with that marker: a lost action produces no re-fire and the recorder
  never republishes, matching the `retry_run` rule. **A publish that
  returns an error rolls the marker back** (`clearIntentRefired`,
  counted `publish-failed`), because the candidate query, the round's
  own tenant list, and the trim keep-set all filter on
  `refired_at IS NULL`: a marker left behind by a failed publish would
  lose the queued build forever and stop protecting its evidence. The
  marker still goes first, since that ordering is what makes two
  replicas publish one action. **A rejected trigger heals the marker
  too**: `applyTriggerFailed` calls `healRefiredIntent` inside its own
  transaction, so the marker and the failed run's record commit
  together. `TRIGGER_REJECTED` and `TRIGGER_UNREACHABLE` are transient
  here (`isTransientTriggerReason`: the cell refused the call, or it
  reached no Jenkins), so each clears the marker and counts
  `cleared`/`rejected-transient`; `JOB_NOT_FOUND` and
  `JOB_NOT_BUILDABLE` are permanent, so the marker stays and the round
  counts `skipped`/`rejected-permanent`. `run_intents.refire_attempts`
  bounds that loop at `maxRefireAttempts` (5) and **never decreases**, so
  a clear cannot restart it from zero; a retried run keeps its own run
  identifier, so `applyTriggerFailed`'s `ON CONFLICT DO NOTHING` leaves
  the `runs` table with one row for every attempt and no way to count
  them. A spent intent leaves the candidate set, the round's tenant
  list, and the trim keep-set, so its retry loop ends and its dead
  instance stops being pinned. `Ingester.SetRefireMetrics` wires the
  counter, following `SetUpstreamFanout`'s optional-setter convention.
  The action names the
  tenant's **current** cell and namespace, read from `tenant_placements`
  by `refire.go`'s own query, because `tenantPlacementLivenessRow`
  carries no namespace and the stored cell is the dead one. It runs from
  the same two places the marking does: `backstop_round.go` for every
  tenant (enumerating its own `(cluster, tenant)` pairs from
  `run_intents`, never `listJobSchedulePairs`, which reads
  `job_schedules` and would miss a tenant holding a granted intent and no
  cron schedule), and `tenant_sync.go`'s announce hook for one tenant.
  `recorder_intent_refired_total` counts every outcome by `outcome` and
  `reason`. A mark and a re-fire are independent: `markInterrupted`
  writes no `run_intents` row, so the intent survives the mark and the
  re-fired build claims the same run.
- **Claim-expiry sweep** (`claim_expiry.go`) — `ExpireUnclaimedRuns`
  marks a `QUEUED` run with a null `build_number`, an
  `accepted_at` at or before the round time minus `ClaimExpiryAge` (a
  fixed 75 minutes: the plugin's one-hour claim budget plus a margin),
  no mesh resume object, and **no `run_intents` row** as `COMPLETED`/
  `TRIGGER_FAILED` with `trigger_reason_code` `ReasonClaimExpired`
  (`CLAIM_EXPIRED`). Such a run has no intent, so neither the interrupted
  sweep (a live cell) nor the re-fire (no intent) can end it. The
  conditional update repeats `claimExpiryGuardSQL` in its `WHERE` clause,
  so a claim or a real event that commits between the read and the write
  keeps the run. `claimExpiryGuardSQL` is the age test plus
  `unclaimedQueuedRunSQL`, which the veto mark shares. A run the veto
  mark ended is no longer `QUEUED`, so the sweep skips it. The write keeps the accepted keys and writes no
  `trigger_http_status`; a late `run.started` supersedes it through the
  ordinary `TRIGGER_FAILED` arm. The same transaction writes one
  `build.claim_expired` activity row; after the commit the sweep sends one
  run notification through `BackstopConfig.RunNotifier` and counts
  `recorder_claim_expired_total`. It writes no intent and publishes no
  ledger action. `RunBackstopRound` runs it right after
  `SweepInterruptedRuns`. One round marks at most `claimExpiryBatch`
  (500) runs, oldest `accepted_at` first, because each mark commits under
  the backstop lock. The age does not prove a claim attempt: an item that
  waits in the Jenkins queue past the limit before its first claim (behind
  a long previous build) is marked too, and its later `run.started`
  supersedes the row; the activity row and the count stay. A second class,
  `claimAbandonedGuardSQL`, ends a `QUEUED` run that still holds its accepted keys
  and a `GRANTED` intent with no start mark, at the same age, with `CLAIM_ABANDONED` and a
  `build.claim_abandoned` activity row. `recorder_claim_expired_total` carries the
  attribute `reason` for both classes. The two classes share one 500-attempt batch
  per round, the abandoned class first, so an unclaimed backlog never starves it.
- **The end of a marked lost run** (`interrupted_marked.go`) —
  `EndMarkedLostIntents` ends a `GRANTED` intent that holds a start mark,
  whose run holds no `runs` row that records a build and no non-`QUEUED`
  row (`markedLostIntentGuardSQL`), and whose `cell_instance` is
  confirmed gone past the grace (`confirmedGoneInstance`). An unconfirmed
  dead entry waits. In one transaction it locks the intent, re-reads the
  guard, writes the run `RUNNING` from the intent's columns (an insert, or
  an update of a `QUEUED` accepted row that strips the accepted keys),
  and calls `markInterruptedTx` with `cell-rolled`. The intent stays
  `GRANTED`. After the commit it sends one run notification and calls
  `afterMark`; an automatic retry of a parameterized intent is counted in
  `recorder_retry_skipped_total` with reason `parameterized` and not
  published. It applies `sweepConsumerSkip` itself, and `RunBackstopRound`
  runs it before `SweepInterruptedRuns`, so the sweep never marks the
  accepted row as a queued run.
- **Backstop cron scheduler and the interrupted sweep's round**
  (`backstop_round.go`, `backstop_policy.go`, `backstop_candidates.go`,
  `backstop_fire.go`, `backstop_metrics.go`, `action_metrics.go`,
  `concurrency_metrics.go`, `ledger_concurrency.go`) — design D10 and
  D11. `RunBackstopRound` holds one round on `backstopLockKey`, the third
  fixed advisory-lock key after `migrateLockKey` and `maintenanceLockKey`,
  and runs `EndMarkedLostIntents`, `SweepInterruptedRuns`, `ExpireUnclaimedRuns`,
  `ReleaseConfirmedFleetLocksRound`, and `runBackstopSchedule` inside the
  same hold, so one ticker and one lock cover them all. The round computes no
  fire minute of its own: `isBackstopCandidate` admits only an occurrence
  Jenkins itself exported (`JobSchedule.NextFireTimes`) that is past the
  tenant's grace window, not older than 24 hours, holds no
  `cron:<job-path>:<occurrence-minute>` ledger intent, and holds no
  recent `build.scheduled.backstop` activity row (keyed on that row's
  `reason` field, which holds the occurrence minute).
  `dropSupersededCandidates` then removes every candidate before the
  newest `build.scheduled.backstop` occurrence of the job in the last 24
  hours (the fired occurrence itself stays, so a lost fire can re-fire): a row keeps passed occurrences, so without it each round fires
  the next older miss. `selectFire` applies
  `Team.spec.pipelines.cronCatchUp`; `selectReadyFires` applies
  the `sha256(cluster|tenant|job_path) mod grace_seconds` spreading delay
  and the 100-fire-per-round bound. `readTenantSchedulePolicy` answers
  the backstop-scheduler capability's defaults (backstop on, catch-up
  `one`, grace `5m`) for a tenant with no `tenant_placements` row.
  `runBackstopSchedule` resolves each row's home through `resolveJobHome`
  before it selects a candidate, and addresses each fire's `wake_cell` and
  `retry_run` to that home. A home slot with no cell skips the row for this
  round and writes no activity row, so a later round fires it. A row whose
  namespace or cell is not the home's is skipped, because every home write
  moves the job's rows. The round reads no placement generation: a
  generation raise leaves an unmoved row below it until its cell exports
  again. A tenant with no placement row keeps the row's own coordinate.
  **A failed fire belongs to the re-fire pass** (`backstop_retry.go`).
  `isBackstopCandidate` refuses an occurrence whose intent key holds any
  `TRIGGER_FAILED` run, so a permanent code never fires again, and its
  activity-row suppression lasts the longer of the grace window and
  `backstopAnswerWait`, so a waiting first fire gets no second fire.
  `selectBackstopRetries` reads the failed `cron:` runs of the last 24
  hours, not `job_schedules`, so a re-fire outlives the passed-occurrence
  window. It re-fires a key only when every failed run carries
  `TRIGGER_UNREACHABLE` and at most `maxBackstopRefires` (3) failed runs
  and fires exist (an unanswered fire spends its attempt too), no
  intent holds the key, the failed runs number at least the
  occurrence's `build.scheduled.backstop` rows (every fire answered: a
  count, never a clock, because a run identifier holds millisecond time,
  or `backstopAnswerWait` (10 minutes) since the newest fire),
  the newest fire is past the grace window, and no newer occurrence of
  the job fired. `mergeBackstopRetries` drops a re-fire that a newer first
  fire of the same job in the round supersedes. A re-fire counts
  `recorder_cron_refired_total` beside `recorder_cron_fired_total`.
  `applyTriggerFailed` writes `payload.trigger_attempt` (one more than the
  key's other failed runs of the same cluster and tenant, and at least
  the occurrence's fire count for a `cron:` key) on every failed trigger, and appends
  `backstopRefireSpentNote` to `trigger_error` for the `cron:` failure
  that spends the last re-fire.
  `Ledger.SetConcurrencyCancel` wires the sibling concurrency-groups
  capability: on a granted claim for a `cancelInProgress` tenant, `Claim`
  starts `runConcurrencyCancel` in its own goroutine so it answers the
  caller before the cancel's own database read and publish. Every action
  this bullet's code publishes counts in `recorder_actions_published_total`,
  labelled by kind, alongside the interrupted-run marking's own publishes.
- **A row that records no build is not a run record** — a `TRIGGER_FAILED`
  row records that no build exists, and a row that still holds the
  payload key `accepted_at` records an accepted trigger no real event
  has yet followed, including one the interruption sweep has since
  marked interrupted. Every query that asks about a **real run** excludes
  both through `recordsBuildSQL` (or `recordsBuildRunSQL` for an aliased
  join), both in `ingest_common.go` and both the negation of
  `noBuildRecordedSQL`. The `TRIGGER_FAILED` half says `IS DISTINCT FROM`,
  never `<>`: a queued or running row holds a null `result`. The
  `accepted_at` half is `COALESCE`-guarded against a null `payload`, so a
  heartbeat-first or tests-first placeholder is never excluded by
  mistake. The excluding readers are the four job aggregates, the two
  windowed cell-usage aggregates (`reader_cellusage.go`), both
  `rehydration.go` queries, `runsRowExists` (`ledger.go`), `lookupOwner`
  (`ledger_owner.go`), and the orphan-intent `NOT EXISTS`
  (`retention_intents.go`). Each has its own failure mode: a seed built
  from such a row renders build zero and the mite fails the whole bundle;
  a counted row keeps the old owner on a repeat claim and makes the real
  completion read as foreign; a joined `COMPLETED` status arms a
  log-finalization deadline that already passed; an orphan intent never
  leaves the table.
  **A reader that anchors on `run_intents` and joins `runs` needs the
  predicate too.** A repeated client token reuses the run identifier, so
  an intent row and a synthetic row can name one run with no real event
  yet. `lookupOwner` is exactly that shape.
  `selectFinalizeCandidates` (`retention_finalize.go`) and
  `readCrossTenantRunRecord` (`crosstenant_runrecord.go`) keep the
  narrower `notTriggerFailedSQL`/`notTriggerFailedRunSQL` on purpose: an
  accepted-only row's finalization anchor is always null, so the sweep
  never selects it, and a cross-tenant poll reads such a row as queued
  while it waits and as interrupted once the sweep marks it, through the
  ordinary status mapping. The cross-tenant read reads a
  `TRIGGER_FAILED` row only when no intent row names the run. The two `status = 'RUNNING'`
  cell-usage counts (`reader_cellusage.go`) also keep it: a running row
  can hold neither a `TRIGGER_FAILED` result nor an `accepted_at` key, so
  the narrower predicate already excludes everything the wider one would.
  `ListRuns`, `SearchRecords`, and `GetRun` keep the row on purpose. A
  query that already names a `status` the row does not hold (the
  interruption sweep, the duplicate backstop, the concurrency cancel,
  the fleet-lock move, the running-instance read) needs no predicate.
  Every real write removes the payload keys `accepted_at` and
  `accepted_instance` before its own merge, so a row that holds a build
  event never carries them; the interruption mark leaves them in place.
  The complete reader survey is the change's design S14.
- **Reader** (`reader*.go`) — `Reader` is the BFF's only read path onto
  the record tables; `NewReader` wraps a `*pgxpool.Pool`. Every method
  calls `checkSchema` first and returns `ErrSchemaTooOld` when the
  applied `varroa.goose_db_version` is below `minGooseSchemaVersion`, or
  unreadable. `ListRuns` pages by keyset on `(created_at DESC, run_id
  DESC)`, never `OFFSET`; its `cause` filter joins `run_intents` instead
  of matching `runs.cause`, because `runs.cause` holds the free-form
  Jenkins `Cause` object and `run_intents.cause` holds the plain-text
  classification a claim request carries. `ListArtifacts` omits every
  internal key, a path under `InternalArtifactPrefix` (`.varroa/`, where the
  plugin archives `.varroa/tests.json`), so the BFF list and both
  cross-tenant artifact kinds never show one. `GetArtifact` reads one row
  by its exact path, internal keys included; the BFF download route reads
  through it, so the test report still downloads. `GetRun` resolves `created_at`
  from `run_intents` first, so its `runs` query prunes to one partition.
  A run that holds no intent row, which is what a failed trigger writes,
  falls back to a bounded `runs` query over `runWindow`
  (`runwindow.go`, 24 hours before the `run_id` time to 7 days after
  it), then to an unbounded one. The bound is a fast path alone, and it
  never decides whether a row is readable. Every job aggregate
  (`JobWindowStats`, `addJobRecentResults`, `addJobLastRuns`,
  `addJobDurationTrends`) carries
  `AND result IS DISTINCT FROM 'TRIGGER_FAILED'`, never `<>`, because a
  running row holds a null `result`. `ListRuns` and `SearchRecords`
  carry no such predicate.
  `JobSummary` is record-derived only: no job definition, no trigger
  state, no job that has never run — the pipelines API supplies those
  from the catalog. `ListActivity` pages `activity` by keyset on
  `(ts, event_key)`, backward by default and forward for an explicit
  time range; its `ActivityQuery.Scope` is the D5 authorization
  superset pushed into SQL, and the per-event predicate still runs on
  every returned row. `ListJobSchedules` returns one cell or tenant's
  `job_schedules` rows, carrying `NextFireTimes`, `Namespace`, `Cell`, and
  `PlacementGeneration`. The backstop cron scheduler calls
  `ListJobSchedules` directly from this package, not only from the BFF.
  It compares `Namespace` and `Cell` with the job's home and never reads
  `PlacementGeneration`, which fences the tenant purge. `SearchRecords`
  answers the `job`, `run`, and `activity` kinds of the `search` tool
  and `GET /search`. `ActivityRetentionDays` reports the parsed
  `recordStore.retention.activity` value the BFF passed to `NewReader`;
  a nil `Backfill` (no DSN configured) never reaches this method.
  `CellUsage`, `CellUsageMany`, and `CellTenantUsage`
  (`reader_cellusage.go`) report one cell's load. The first two answer
  tenant by tenant; `CellUsage` calls `CellUsageMany` with one cell.
  `CellTenantUsage` answers one row lower — **one row for each of one
  tenant's jobs** — because the team cell read must sum only the jobs the
  caller's `jobs` globs admit, and a tenant-wide number would leak the
  rest. Its row set is the union of the tenant's `job_schedules` paths and
  every job path the two `runs` aggregates name, and its `jobs` value is
  one or zero per row, so summing a row set reproduces the cell-wide
  aggregate for those jobs. The cell-wide
  statement holds four subqueries: the `tenant_placements` rows, a
  windowed `runs` aggregate, a status-only `runs` aggregate, and a
  `job_schedules` aggregate. The two `runs` aggregates stay separate —
  one scan carries one predicate, and the windowed one keeps the
  `created_at` term that prunes the partitions while `runningNow` carries
  no time bound at all. Neither `runs` aggregate carries a `namespace`
  term: the ledger moves an intent's `cell` on an ownership transfer and
  not its `namespace`, so the term would drop a run the cell did execute.
  The tenant set always comes from `tenant_placements`, never from a scan
  of `runs`, which carries no index on `cell`; a tenant that left the cell
  therefore contributes no count to it. `jobs` counts
  `DISTINCT job_path`, because `job_schedules` holds one row for each
  trigger type. The window is a fixed 24 hours and no caller chooses
  another. Each tenant row also carries the placement row's `generation`
  and `agent_namespace`, which the Cell tab shows for a member cell.
  `ListTestCases` and `ListTestCaseHistory` (`reader_testcases.go`) read
  `run_test_cases`. The run read answers every row that is not `SKIPPED`
  first, then each group by suite, class name, and test name. The history
  read joins `runs` on its own primary key, answers one class and name of
  one job newest first, and computes no rate. The caller bounds its limit.

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
  go test -race -count=1 -timeout 30m ./internal/recordstore/...
go test -race -count=1 ./internal/recordstore/...   # DSN unset: tests skip
```

The DSN must be a URL and its role must hold `CREATEDB`: the `blob` test
binary creates and uses `<db>_blob`, so the two packages run at the same
time without `-p 1`.

The package runs about 7 minutes with a live DSN. Under runner contention it can
run longer than Go's default 10-minute test timeout. The `Record store` job in
`.github/workflows/pr.yaml` sets `-timeout 30m` for this reason. Keep the
flag on every command that runs `./internal/recordstore/...` with a live DSN.

## Child DOX Index

- [blob](blob/AGENTS.md) — the `Store` interface, `pgblob`, and the
  `gocloud.dev` adapter for build logs and build artifacts.
