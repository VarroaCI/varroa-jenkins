# Build Records

The build record store keeps build history outside each Jenkins controller. It records one run
per build: stages, tests, artifacts, and build-number ledger.
It is off by default.

Logs and artifacts live in a blob store, and the record store holds only their index. See [Build logs and
artifacts](build-logs-and-artifacts.md).

## Enable the store

```yaml
recordStore:
  enabled: true
  postgres:
    mode: cnpg
    cnpg:
      instances: 1
      storage:
        size: 20Gi
  blobs:
    mode: postgres
  retention:
    runs: 365d
    logs: 90d
    artifacts: 30d

recorder:
  replicas: 1
```

`recordStore.postgres.mode` selects the database. `cnpg` renders a CloudNativePG `Cluster` in the
chart; `external` skips the `Cluster` and reads your Secret instead:

```yaml
recordStore:
  postgres:
    mode: external
    external:
      existingSecret: my-postgres-dsn
      dsnKey: dsn
```

`recordStore.blobs.mode` selects where log and artifact bytes live. `postgres` stores them as
rows in the same database; `s3` stores them in an S3-compatible bucket:

```yaml
recordStore:
  blobs:
    mode: s3
    s3:
      url: "s3://my-bucket?region=us-east-1"
      existingSecret: my-s3-credentials
```

The bucket must support conditional PUTs (`If-None-Match: *` and `If-Match`). AWS S3 and MinIO do.

`recorder.*` configures the `varroa-recorder` component as `bff.*` configures the BFF.

## Install CloudNativePG first

`recordStore.postgres.mode: cnpg` needs the [CloudNativePG](https://cloudnative-pg.io/) operator
in the cluster. The chart does not install it:

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-1.30.0.yaml
```

Pin the operator version and upgrade it on your schedule.

## Postgres roles

| Role | Created by | Used by |
|---|---|---|
| `varroa_recorder` | CloudNativePG, as the database owner | the recorder; runs migrations and writes every table |
| `varroa_client` | CloudNativePG, as a managed role | the BFF; reads records and writes agent tables |
| `varroa_gateway` | CloudNativePG, as a managed role | the gateway, only in `postgres` blob mode; reads every table and writes log chunks |

`varroa_gateway` writes only to `varroa.log_chunks` and its later partitions, and never deletes a
row. The recorder alone writes every other table.

`external` mode skips the three roles and uses one DSN for the recorder, the BFF and the
gateway. Point `existingSecret` at a role that owns schema `varroa`.

## Retention

```yaml
recordStore:
  retention:
    runs: 365d
    logs: 90d
    artifacts: 30d
    restartSource: 7d
```

Each value is a day count from `1d` to `3650d`. `runs` controls how long
a build's row and stages stay queryable. `logs` and `artifacts` control blobs.
`restartSource` keeps a restart copy of a cell run's build. It cannot exceed `runs`.

Store-wide retention rounds up to a month, because the recorder drops whole monthly partitions.
A `runs` value of `365d` keeps a build 365 to 396 days. The build-number watermark is permanent.

## Tenant and job windows

Set a shorter window per tenant or job. See [Tenant and job retention windows](multi-tenancy.md#tenant-and-job-retention-windows).

## A trigger that fails

Jenkins or the recorder can refuse a trigger. The store then holds one `TRIGGER_FAILED` run with its reason.

| Reason code | What it means |
|---|---|
| `JOB_NOT_FOUND` | The job is still missing 90 seconds after the trigger reached the controller. |
| `JOB_NOT_BUILDABLE` | The job exists, but it cannot start a build. |
| `TRIGGER_REJECTED` | The controller refused the trigger for another reason. |
| `TRIGGER_UNREACHABLE` | After five minutes, the trigger found no ready Jenkins with its tenant's items. |
| `CLAIM_EXPIRED`, `CLAIM_ABANDONED` | No claim, or no build after a granted claim, in 75 minutes. See [Build claims](build-claims.md). |
| `FENCED`, `CROSS_TENANT`, `INVALID_KEY`, `THROTTLED` | The recorder refused the build claim. |
| `NOT_HOME` | A replay or restart whose job moved. |

The cron backstop retries only `TRIGGER_UNREACHABLE`; see
[Cell loss](cell-loss.md#a-backstop-fire-during-a-cell-roll).
The run page shows the error text. It holds no build number, log, or stage. `Re-run` starts a fresh run with each parameter's declared default.

A `TRIGGER_FAILED` run counts in no job statistic. It appears in the run list until `runs` expires it.

## A trigger that a cell loses

A cell pod can roll before a build Jenkins accepted onto its queue starts. The store then holds
one run whose status is `QUEUED`, with no build number, and the run page shows `Queued`.
[Cell loss](cell-loss.md) states how the recorder marks and re-fires such a run.

Counters: `varroa.gateway.trigger_accepted`, `recorder_ingest_trigger_accepted_total` (a
`stored` attribute), and `recorder_interrupted_marked_total` (a `status` attribute, `queued` or
`running`). `recorder_ingest_superseded_start_total` counts a dropped late `run.started`; see
[Build claims](build-claims.md).

## Live updates

The recorder publishes one notification per run change onto the NATS subject
`runs.<cluster>.<tenant>`.

The BFF serves one stream per tenant at `GET /pipelines/{tenant}/stream`, under the ticket scope
`pipelines:{tenant}`, with `run` and `stage` events.

A page treats a notification as a hint and refetches the record.

The recorder coalesces per run: a change waits at most 500 milliseconds, and a terminal state
publishes at once.

With a closed stream, the build page reads a live run's record every 3 seconds.

## Activity history

Every activity event publishes onto the JetStream stream `varroa_activity`. That stream holds
at most 72 hours, so the recorder can catch up after a brief outage; it is no retention tier.
The recorder writes each event once into the `activity` table. The activity readers never read the stream.

```yaml
recordStore:
  retention:
    activity: 30d
```

Size Postgres for the retention you configure. An activity row is about 400 bytes, and a busy
fleet of 400 controllers writes about 40 MB a day, so `30d` needs about 1.2 GB and `90d` about
3.6 GB, plus the sizing above.

Enabling the store on an existing fleet imports the 72-hour buffer once. It discards an event
older than `retention.activity`.

## Source fields

A build reports its definition in four kinds of event.

| Event | Fields |
|---|---|
| The flow starts. | The script path. The shared libraries. The branch, for a multibranch child. |
| The first node that resolves a revision. | The branch, the revision, the repository URL, the changes, and the pod templates the run has allocated. |
| Each allocation of a new pod template after that node. | Every source field again, and every pod template the run has allocated. No change list. |
| Completion of a run with no revision. | Its pod templates, and no revision fields. |

The repository URL is the job's own. A library checkout never sets it. The URL carries no credentials, query, or fragment.

A template-defined run reports no script path. The plugin reads each pod template at agent attach, so an early leaver stays recorded. Each entry names a pod template, at most 50. A non-Kubernetes agent,
or an agent gone before the read, reports its node name and no image.

Each `libraries` entry also carries `repositoryUrl` and `repositoryForge`. The URL reads the current configuration.
The forge is `github`, `gitlab`, or `gitea`, or absent when unknown. The revision is empty for a cached library.

`GET /runs/{run}` answers them in one `source` object: `definitionPath`, `libraries[]`,
`podTemplates[]`.

## Test suites

The completion event carries one counter row per junit suite, at most 500. The recorder
replaces the suite rows with that set. `GET /runs/{run}/tests` serves `items` and
`totals`.

## Test cases

When a run completes, the plugin writes every junit case, passed cases included, to
`.varroa/tests.json`. Download it through
`GET /runs/{run}/artifacts/.varroa/tests.json`, which answers `409` when Jenkins stored the file
locally.

The completion event carries the failed and skipped cases, at most 500. The recorder
stores one `run_test_cases` row per case. `GET /runs/{run}/tests`
serves them in `cases`, failed cases first, and the report file in `report`.

`retention.runs` controls the case rows. `retention.artifacts` controls the report. Without
the report, `downloadable` is false.

`GET /pipelines/{tenant}/jobs/{job}/tests/{case}` answers the recent outcomes of one case, newest
first. `{case}` is a `caseKey` value from the tests read, encoded for one path segment.
The read answers 50 rows, and `limit` accepts at most 200. It covers the `retention.runs`
window.

## Coverage and warnings summaries

The completion event carries at most 20 coverage reports and 20 warnings reports. The recorder
stores one `run_coverage` row or one `run_warnings` row per report. The rows hold summary numbers
only, not the report file. A run with no report has no row. A zero count is a real value. The new
and fixed counts are present only when the warnings plugin found a reference build. `retention.runs`
controls these rows, and a tenant retention window deletes them with the run.

`GET /runs/{run}` and the `get_run` tool carry `quality`. It lists the run's coverage reports and
warnings reports in report id order. The BFF computes `percent` as covered count times 100 over the
total, rounded to one decimal. `percent` is absent when the total is zero.

Each report carries `delta` against the newest earlier run of the same job with the same report id.
`linePoints` and `branchPoints` are percentage-point differences. A report with no earlier run has
no `delta`. The Varroa delta can differ from the plugin delta, because the plugin compares with a
Jenkins reference build. `new` and `fixed` come from the plugin only.

The job detail and `get_job_health` carry `qualityTrend` over the same runs as `trend`. A run with no
row for a report has no point in that series.

## Job schedules

The `job_schedules` table holds the fleet's cron inventory: one row per job trigger, plus one
`none` row for a job with no trigger. A cell's set is its whole job inventory. After a cell restart, a Jenkinsfile trigger has no row until its next build.

The plugin computes each cron row's next three occurrences with Jenkins's cron engine, so the times match the Jenkins job page. An `scm-poll`, `upstream`, or `other` row, and every `none`
row, carries no occurrence.

The mite sends no set until its Jenkins holds an applied desired state, because a new cell pod's
empty set would delete the cell's rows. The first apply opens this gate and starts a poll. A
failed poll retries within a minute. The mite polls again 5 seconds after the next cron
occurrence, at most once a minute and at least every ten minutes. It sends the set when the set
changed, on a reconnect, when a sent occurrence has passed, or every thirty minutes. A row names
its job's home cell, its only writer. A home move moves the row at once. The backstop fires only
a row on its job's home. An envelope deletes only rows that name its cell.

A row's `next_fire_times` keeps its passed occurrences from the last
`max(3 × cronGrace, 2 × cronGrace + 10m) + 2m`, at most 16, so the backstop can judge a miss. A changed spec, time zone, or disabled state
replaces the list.

`job_schedule_cells` holds one watermark row per cell: the time the cell took its newest accepted set. A resent set keeps the time the cell took it.
An occurrence that never fired under a watermark at most thirty minutes old is a missed fire. An older watermark means the cell has not reported, so its set is stale.

The recorder refuses a set taken more than five minutes ahead of its own clock and keeps the watermark. It logs the cell and counts `recorder_job_inventory_ahead_total`. A set from a correct clock replaces a watermark that is already past that limit.

A `truncated: true` set means the plugin's walk stopped early, at 5000 rows or 700 KiB, whichever
came first. The recorder deletes no row, because a partial set would erase schedules the plugin missed. The one exception is a row stamped more than five minutes ahead of the recorder clock, which the recorder always removes.

## Multibranch inventory

`job_branch_projects` holds each multibranch job's branch inventory, up to ten minutes old. The same five-minute limit applies to the branch inventory.

## Search

`GET /search` and the MCP `search` tool add three record-backed kinds on top of the four
Kubernetes-sourced kinds: `job`, `run`, and `activity`. Each kind is capped at five results.
Matching is a Postgres full-text prefix match on the last search token, not a substring match:
`serv` matches `tenant-a/service/main`, but no infix in a longer word. Without the record store
these three kinds are absent; the other four still answer.

## The ledger and intent keys

Every build starts with a claim against the ledger, carrying an intent key that identifies the
request that caused it. The recorder uses the key to make a retried claim idempotent. Until the
build starts, the same key and run get the same answer. Once the recorder records the start, a
repeat claim answers `duplicate`. A redelivery before that record can still build twice on
another cell.

| Scheme | Shape | Use |
|---|---|---|
| `scm` | `scm:<job-path>:<revision>` | a build triggered by an SCM change |
| `manual` | `manual:<uuid>` | a build triggered by a person |
| `cron` | `cron:<job-path>:<fire-minute>` | a build triggered by a schedule |
| `retry` | `retry:<run-id>` | a retry of an earlier build |
| `upstream` | `upstream:<caller-run-id>:<seq>` | a build triggered by an upstream pipeline |
| `api` | `api:<uuid>` | a build triggered through the API |

A claim with a key outside this set, or an empty segment, is rejected.

## Backlog metrics

The recorder exports three ingest-backlog metrics:

| Metric | Meaning |
|---|---|
| `recorder_ingest_backlog_msgs` | Build events waiting to be written. |
| `recorder_ingest_backlog_bytes` | Bytes of build events waiting to be written. |
| `recorder_activity_backlog_msgs` | Activity events waiting to be written. |

The build-event metrics measure the `varroa_builds` stream, which holds at most 1 GiB. Warn at 50
percent (512 MiB) and page at 80 percent (819 MiB). A full stream discards new events until the
backlog drains.

The activity metric measures the `varroa_activity` stream, which holds at most 1000000 messages.
Warn at 500000 and page at 800000. A full stream discards its **oldest** message.

## Network policy

```yaml
networkPolicy:
  recordStorePostgresEgress:
    enabled: true
    cidrs: []
    ports: [5432]
  recordStoreBlobEgress:
    enabled: true
    cidrs: ["203.0.113.10/32"]
    ports: [443]
```

`recordStorePostgresEgress` governs egress to Postgres from the recorder, the BFF, and, in
`postgres` blob mode, the gateway. In `cnpg` mode an empty `cidrs` list selects the in-cluster
CloudNativePG pods; in `external` mode it selects nothing, so the render fails and asks you to
name the host.

`recordStoreBlobEgress` governs the same components' egress to S3-compatible storage, and it
renders nothing when `recordStore.blobs.mode` is `postgres`. In `s3` mode, an empty `cidrs` list fails the render.

Both policies apply only when `networkPolicy.enabled` and `recordStore.enabled` are true.

In `cnpg` mode the chart also renders a policy for the Postgres pods, admitting the recorder, the
BFF, and the gateway on port 5432, and the CloudNativePG operator on port 8000 from the namespace
`networkPolicy.recordStoreCNPGOperatorNamespaceSelector` names (default `cnpg-system`).

## When the database is unreachable

A connect to the record store is bounded. Each BFF read is bounded too.

- A connect attempt stops after 5 seconds. A connection string that sets its own `connect_timeout` keeps that value.
- Each read in the BFF stops after 15 seconds. Each assistant store call stops after 8 seconds. The gateway, the recorder, and the Tenant agent runner do not bound their calls after connect.
- The BFF starts and stays ready while the database is down. Its startup schema check stops after 10 seconds.
- The assistant stays on while the database is down. Its calls answer 503 with the code `record_store_unreachable` until the database returns. A BFF restart is not necessary.
- The BFF keeps each schema check that passes for its life. A database restored to an older schema needs a BFF restart.
- Pipelines reads answer 503 with the code `record_store_unreachable`. The page shows "Pipeline data is unreachable" with a Retry button. A 503 without that code means the store is off or its schema is too old. A read answers after one connect timeout. The page does not retry the read on its own.
- The Cells page and the Cell tab mark each record count as not available and show Retry. A store that is off shows no Retry; enable `recordStore` instead.
- The tenant home Locks card shows the same Retry state.
- The recorder fails at startup and restarts until the database returns. A restart is the correct response.

Build history returns when the database answers again. No operator action is needed for the BFF.

## Remove the database

Disabling `recordStore.enabled` does not delete the database. The chart marks the `Cluster` and
every role Secret `helm.sh/resource-policy: keep`, so a disabled or uninstalled release leaves
the data in place. To delete the data, remove the `Cluster` and every role Secret together:

```bash
kubectl delete clusters.postgresql.cnpg.io varroa-recordstore -n <release-namespace>
kubectl delete secret varroa-recordstore-recorder varroa-recordstore-client varroa-recordstore-gateway -n <release-namespace>
```

Deleting only the `Cluster` and re-enabling the store regenerates the Secrets with new passwords,
which locks the recorder out of any surviving volume. Delete both together, or not at all.
