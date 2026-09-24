# Build Records

The build record store keeps build history outside each Jenkins controller. It records one run
per build: its stages, its test summary, its artifact index, and a durable build-number ledger.
It is optional and off by default.

The store holds no job configuration, console log text, or artifact bytes. Logs and artifacts
live in a separate blob store, and the record store holds only their index. See [Build logs and
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

`recorder.image`, `recorder.replicas`, and `recorder.resources` configure the `varroa-recorder`
component the same way `bff.*` configures the BFF.

## Install CloudNativePG first

`recordStore.postgres.mode: cnpg` needs the [CloudNativePG](https://cloudnative-pg.io/) operator
in the cluster, which the chart does not install. Install it before you enable the store:

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-1.30.0.yaml
```

Pin the operator version and upgrade it on your schedule. `helm template` renders the
`Cluster` without the operator, so a render needs no live cluster.

## Postgres roles

| Role | Created by | Used by |
|---|---|---|
| `varroa_recorder` | CloudNativePG, as the database owner | the recorder; runs migrations and writes every table |
| `varroa_client` | CloudNativePG, as a managed role | the BFF; reads runs, stages, tests, and artifacts |
| `varroa_gateway` | CloudNativePG, as a managed role | the gateway, only in `postgres` blob mode; reads every table and inserts log chunks |

`varroa_gateway` holds no `UPDATE` or `DELETE` on any table, and no default `INSERT` on future
tables: it inserts only into `varroa.log_chunks`, and that grant reaches every later partition.
The recorder alone writes every other table.

`external` mode skips the three roles and uses one DSN for the recorder, the BFF and the
gateway. Point `existingSecret` at a role that owns schema `varroa`: it creates the schema, runs
migrations, and reads and writes every table.

## Retention

```yaml
recordStore:
  retention:
    runs: 365d
    logs: 90d
    artifacts: 30d
```

Each value is a day count with a `d` suffix, from `1d` through `3650d`. `runs` controls how long
a build's row, stages, and test summary stay queryable; `logs` and `artifacts` control their blob
content independently.

Retention rounds up to a month boundary, because the recorder drops a whole monthly partition,
not single rows. A `runs` value of `365d` keeps a build for 365 to 396 days.

The build-number watermark is permanent, and no build number is reused, even after every run of a
job expires.

## A trigger that fails

Jenkins can refuse a build trigger. The store then holds one run whose result is
`TRIGGER_FAILED`, and the run page names the reason.

| Reason code | What it means |
|---|---|
| `JOB_NOT_FOUND` | The job does not exist on the controller. |
| `JOB_NOT_BUILDABLE` | The job exists, but it cannot start a build. |
| `TRIGGER_REJECTED` | The controller refused the trigger for another reason. |

The run page also shows the error text the controller reported. Such a run holds no build number,
no log, and no stage. `Re-run` starts a fresh run with each parameter's declared default.

A `TRIGGER_FAILED` run counts in no job statistic: no pass rate, no result count, no median
duration, and no last-build tile. It still appears in the run list, and retention bounds it under
`runs` above.

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

The BFF serves one stream per team at `GET /pipelines/{team}/stream`, under the ticket scope
`pipelines:{team}`, with `run` and `stage` events.

A page treats a notification as a hint: it refetches and renders the record.

The recorder coalesces per run: a change waits at most 500 milliseconds, and a terminal state
publishes at once.

Each page refetches when its stream opens, because the stream sends no first frame. While the
stream is closed and the run is not terminal, the build page reads the record every 3 seconds.

## Activity history

Activity history rides two stores: a fixed transport buffer in NATS and a durable table in
Postgres. Every activity event publishes onto the JetStream stream `varroa_activity`, which
covers lifecycle changes, user actions, and mite-observed changes. That stream holds at most 72
hours, in code and not in chart values: it lets the recorder catch up after a brief outage, and
it is no retention tier. The recorder drains the stream and writes each event once into the
`activity` table. `GET /activity` and the MCP activity readers read that table, never the stream.

```yaml
recordStore:
  retention:
    activity: 30d
```

Size Postgres for the retention you configure. An activity row is roughly 400 bytes, and a busy
fleet of 400 controllers writes about 40 MB a day, so `30d` needs about 1.2 GB and `90d` about
3.6 GB, plus the sizing above.

Enabling the store against a fleet that ran without one imports history once, from the oldest
message the 72-hour buffer still holds. An event older than `retention.activity` when read is
acknowledged and discarded, so a stale backlog creates no row bound for deletion.

## Source fields

A build reports its definition in three events.

| Event | Fields |
|---|---|
| The flow starts. | The script path. The shared libraries. The branch, for a multibranch child. |
| The first node that resolves a revision. | The branch, the revision, the repository URL, the changes, and the pod templates the run has allocated. |
| The first allocation, when the first node that resolves a revision reported no pod template. | Every source field again, and the pod template of the allocated agent. It carries no change list. |

A template-defined run reports no script path. A run that resolves a revision reports the pod
template once it allocates its first agent. A run that resolves no revision reports none. A later
agent reports no entry.

`GET /runs/{run}` answers them in one `source` object: `definitionPath`, `libraries[]`,
`podTemplates[]`.

## Test cases

When a run completes, the plugin writes every junit case, passed cases included, to
`.varroa/tests.json`. It archives that file as an ordinary run artifact. Download it through
`GET /runs/{run}/artifacts/.varroa/tests.json`, which answers `409` when Jenkins stored the file
locally.

The completion event carries the run's failed and skipped cases, at most 500. The recorder
writes them into the `run_test_cases` table, one row for each case. `GET /runs/{run}/tests`
serves them in `cases`, failed cases first, and the report file in `report`.

`retention.runs` controls the case rows. `retention.artifacts` controls the report bytes. When
the report leaves, the tests read answers `downloadable` false.

`GET /pipelines/{team}/jobs/{job}/tests/{case}` answers the recent outcomes of one case, newest
first. `{case}` is a `caseKey` value from the tests read, encoded again for one path segment.
The read answers 50 rows, and `limit` accepts at most 200. It covers the `retention.runs`
window.

## Job schedules

The `job_schedules` table holds the fleet's cron inventory: one row per job trigger, plus one
`none` row for a job with no trigger. A cell's set is that cell's whole job inventory, so a
branch job with no trigger and no run still holds a row, because no other producer reports it.

The plugin computes each cron row's next three occurrences with Jenkins's cron engine, so the
times match the Jenkins job page. An `scm-poll`, `upstream`, or `other` row, and every `none`
row, carries no occurrence.

Each mite polls its Jenkins every ten minutes and after every reconnect. It sends the set when
it changed, on a reconnect, or every thirty minutes. The gateway publishes one cell's set in
one envelope, and the recorder replaces the cell's rows with it or rejects a row whose tenant
is unresolved.

`job_schedule_cells` holds one watermark row per cell: the time of the newest accepted envelope.
Read it before you judge a missing occurrence. An occurrence that never fired under a watermark
no more than thirty minutes old is a missed fire. An old watermark means the cell has not
reported, likely a disconnected mite or a hibernated controller, and the whole set is stale.

A `truncated: true` set means the plugin's walk stopped early, at 5000 rows or 700 KiB, whichever
came first. Its rows still hold accurate data; only the count is low. The recorder deletes no
row, because a partial set would erase schedules the plugin never reached.

## Search

`GET /search` and the MCP `search` tool add three record-backed kinds on top of the four
Kubernetes-sourced kinds: `job`, `run`, and `activity`. Each kind is capped at five results.
Matching is a Postgres full-text prefix match on the last search token, not a substring match:
`serv` matches `team-a/service/main`, but no infix in a longer word. Without the record store
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
Warn at 500000 and page at 800000. A full stream discards its **oldest** message, so a sustained
backlog loses old history rather than rejecting new events.

## Network policy

```yaml
networkPolicy:
  recordStorePostgresEgress:
    enabled: true
    cidrs: []
    ports: [5432]
  recordStoreBlobEgress:
    enabled: true
    cidrs: ["0.0.0.0/0"]
    ports: [443]
```

`recordStorePostgresEgress` governs egress to Postgres from the recorder, the BFF, and, in
`postgres` blob mode, the gateway. In `cnpg` mode an empty `cidrs` list selects the in-cluster
CloudNativePG pods; in `external` mode it selects nothing, so the render fails and asks you to
name the host.

`recordStoreBlobEgress` governs the same components' egress to S3-compatible storage, and it
renders nothing when `recordStore.blobs.mode` is `postgres`: that mode needs no external egress.

Both policies apply only when `networkPolicy.enabled` and `recordStore.enabled` are true.

In `cnpg` mode the chart also renders a policy for the Postgres pods, admitting the recorder, the
BFF, and the gateway on port 5432, and the CloudNativePG operator on port 8000 from the namespace
`networkPolicy.recordStoreCNPGOperatorNamespaceSelector` names (default `cnpg-system`). It lets
the pods reach the API server that the instance manager watches.

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
