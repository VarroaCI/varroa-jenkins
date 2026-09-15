# Build Records

The build record store keeps build history outside each Jenkins controller. It records one run
per build: its stages, its test summary, its artifact index, and a durable build-number ledger.
It is optional and off by default.

The store does not hold job configuration, console log text, or artifact bytes. Logs and
artifacts live in a separate blob store. The build record store holds only the index that points
to them. See [Build logs and artifacts](build-logs-and-artifacts.md) for the log path, the live
tail, and artifact storage.

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

`recordStore.postgres.mode` selects the database. `cnpg` renders a CloudNativePG `Cluster` inside
the chart. `external` skips the `Cluster` and reads a Secret you provide instead:

```yaml
recordStore:
  postgres:
    mode: external
    external:
      existingSecret: my-postgres-dsn
      dsnKey: dsn
```

`recordStore.blobs.mode` selects where log and artifact bytes live. `postgres` stores them as
rows in the same database. `s3` stores them in an S3-compatible bucket:

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
in the cluster. The chart does not install it. Install the operator before you enable the record
store:

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.30/releases/cnpg-1.30.0.yaml
```

Pin the operator version you install and upgrade it on your own schedule. `helm template` renders
the `Cluster` resource without the operator present, so a render never needs a live cluster.

## Postgres roles

| Role | Created by | Used by |
|---|---|---|
| `varroa_recorder` | CloudNativePG, as the database owner | the recorder; runs migrations and writes every table |
| `varroa_client` | CloudNativePG, as a managed role | the BFF; reads runs, stages, tests, and artifacts |
| `varroa_gateway` | CloudNativePG, as a managed role | the gateway, only in `postgres` blob mode; reads every table and inserts log chunks |

`varroa_gateway` holds no `UPDATE` or `DELETE` on any table, and no default `INSERT` privilege on
future tables: it can insert only into `varroa.log_chunks`, the parent of the monthly log-chunk
partitions, and that grant reaches every partition created after it automatically. The recorder
stays the only writer of every other table.

`external` mode skips all three roles. It uses one DSN for the recorder, the BFF, and the
gateway. Point `existingSecret` at a role that owns schema `varroa`: it must create the schema,
run migrations, and read and write every table. The read-only privileges of `varroa_client` or
`varroa_gateway` are not enough, because the recorder shares this same role.

## Retention

```yaml
recordStore:
  retention:
    runs: 365d
    logs: 90d
    artifacts: 30d
```

Each value is a number of days followed by `d`, from `1d` through `3650d`. `runs` controls how
long a build's row, stages, and test summary stay queryable. `logs` and `artifacts` control how
long their blob content stays queryable, independent of `runs`.

Retention rounds up to a month boundary, because the recorder drops a whole monthly partition
instead of deleting rows one at a time. A `runs` value of `365d` keeps a build for 365 to 396
days, depending on where in the month the build ran. `logs` and `artifacts` round the same way.

The build-number watermark is permanent. Retention never deletes it, even after every run of a
job expires. A build number is never reused.

## A trigger that fails

Jenkins can refuse a build trigger. The record store then holds one run whose result is
`TRIGGER_FAILED`, and the run page names the reason.

| Reason code | What it means |
|---|---|
| `JOB_NOT_FOUND` | The job does not exist on the controller. |
| `JOB_NOT_BUILDABLE` | The job exists, but it cannot start a build. |
| `TRIGGER_REJECTED` | The controller refused the trigger for another reason. |

The run page also shows the error text the controller reported.

Such a run holds no build number, no log, and no stage. No build started, so there is nothing to
show. `Re-run` starts a fresh run, with each parameter's declared default.

A `TRIGGER_FAILED` run counts in no job statistic. It changes no pass rate, no result count, no
median duration, and no last-build tile. It still appears in the job's run list, and a run list
filtered on the result `TRIGGER_FAILED` returns it.

Retention treats the run like every other run. The `runs` value above bounds it.

## A trigger that a cell loses

A cell pod can roll before a build Jenkins accepted onto its queue starts. The record store then
holds one run whose status is `QUEUED`, with no build number. The run page shows `Queued` as soon
as the record exists.

The recorder marks the run interrupted once its accepting cell instance is confirmed gone for
five minutes, with reason `cell-rolled`. The run page names the reason and states that the build
never started on the cell.

The recorder publishes no automatic retry for a queued run, because the record holds no build
parameters. `Re-run` starts the build with each parameter's declared default.

Counters: `varroa.gateway.trigger_accepted`, `recorder_ingest_trigger_accepted_total` (a
`stored` attribute), and `recorder_interrupted_marked_total` (a `status` attribute, `queued` or
`running`). `recorder_ingest_superseded_start_total` counts a dropped late `run.started`; see
[Build claims](build-claims.md).

Retention treats the run like every other run. The `runs` value above bounds it.

## Activity history

Activity history rides two stores: a fixed transport buffer in NATS and a durable table in
Postgres. Every activity event publishes onto the NATS JetStream stream `varroa_activity`.
Events include lifecycle changes, user actions, and mite-observed changes. That stream holds at
most 72 hours, in code, not in chart values: it exists so the recorder can catch up after a
brief outage, not as a retention tier. The recorder drains the stream and writes each event once
into the `activity` table. `GET /activity` and every MCP tool that reads activity read that
table, never the stream.

```yaml
recordStore:
  retention:
    activity: 30d
```

Size Postgres for the retention you configure. An activity row is roughly 400 bytes with its
payload. A busy fleet of 400 controllers produces on the order of 100000 rows a day, about 40 MB
a day. A `30d` retention therefore needs about 1.2 GB; a `90d` retention needs about 3.6 GB. Add
that figure to the `runs`/`logs`/`artifacts` sizing above when you plan total volume.

Enabling the record store against a fleet that already ran with no record store imports history
once. The recorder's stream consumer starts at the oldest stored message. Every event the
72-hour buffer still holds at that moment lands in Postgres on the first drain. An event already
older than `retention.activity` when it is read is acknowledged and discarded, not stored, so a
stale backlog never creates rows headed for immediate deletion.

## Job schedules

The `job_schedules` table holds the fleet's cron inventory: one row per job trigger, plus one
`none` row for a job with no trigger. The set for a cell is that cell's whole job inventory. A
branch job a scan discovered yesterday, with no trigger and no run yet, still has a row, because
no other producer reports it.

The Jenkins plugin computes each cron row's next three occurrences with Jenkins's own cron
engine, so the times you read back match the Jenkins job page exactly. A row for an `scm-poll`,
`upstream`, or `other` trigger, and every `none` row, carries no occurrence.

Each mite polls its Jenkins for the schedule set every ten minutes, and once immediately after
every stream reconnect. It sends the set to the gateway only when the set changed, the stream
reconnected, or thirty minutes passed since the last send. The gateway publishes one cell's whole
set in one envelope; the recorder replaces the cell's rows with it, or rejects the whole envelope
when it cannot resolve a tenant for every row in it.

`job_schedule_cells` holds one watermark row per cell: the time of the newest envelope the
recorder accepted. Read that watermark before you judge a missing occurrence. An occurrence that
never fired while the watermark is fresh, no more than about thirty minutes old, is a missed
fire. An occurrence that never fired while the watermark is old is different: the cell itself
has not reported in a while, likely a disconnected mite or a hibernated controller. There the
whole set is stale, not the one occurrence.

A `truncated: true` set means the plugin's walk stopped early, at 5000 rows or 700 KiB of
serialized rows, whichever it reached first. A truncated set's rows still hold accurate data;
only the row count for that cell is undercounted. The recorder deletes no row on a truncated set,
because a partial set would otherwise erase schedules the plugin had simply not reached yet.

## Search

`GET /search` and the MCP `search` tool add three record-backed kinds on top of the four
Kubernetes-sourced kinds: `job` (from `job_schedules`), `run` (from `runs`), and `activity` (from
`activity`). Each kind is capped at five results. Matching uses Postgres full-text search: a
prefix match on the last search token, not substring matching, so a query for `serv` matches a
job path token like `team-a/service/main` but not an arbitrary infix inside a longer word. When
the record store is not wired, these three kinds are absent and the other four still answer.

## The ledger and intent keys

Every build starts with a claim against the ledger. The claim carries an intent key, a string
that identifies the request that caused the build. The recorder uses the key to make a retried
claim idempotent: the same key with the same run always gets the same answer.

| Scheme | Shape | Use |
|---|---|---|
| `scm` | `scm:<job-path>:<revision>` | a build triggered by an SCM change |
| `manual` | `manual:<uuid>` | a build triggered by a person |
| `cron` | `cron:<job-path>:<fire-minute>` | a build triggered by a schedule |
| `retry` | `retry:<run-id>` | a retry of an earlier build |
| `upstream` | `upstream:<caller-run-id>:<seq>` | a build triggered by an upstream pipeline |
| `api` | `api:<uuid>` | a build triggered through the API |

A claim with a key outside this set, or with an empty segment, is rejected.

## Backlog metrics

The recorder exports three metrics for the ingest backlog:

| Metric | Meaning |
|---|---|
| `recorder_ingest_backlog_msgs` | Build events waiting to be written. |
| `recorder_ingest_backlog_bytes` | Bytes of build events waiting to be written. |
| `recorder_activity_backlog_msgs` | Activity events waiting to be written. |

The build-event metrics measure the `varroa_builds` stream, which holds at most 1 GiB. Alert at
two thresholds against that limit: warn at 50 percent (512 MiB) and page at 80 percent (819 MiB).
That stream discards a new build event once full until the backlog drains.

The activity metric measures the `varroa_activity` stream, which holds at most 1000000 messages.
Alert the same way: warn at 50 percent (500000) and page at 80 percent (800000). That stream
discards its **oldest** message once full, not the newest, so a sustained backlog above the page
threshold silently loses old activity history rather than rejecting new events.

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

`recordStorePostgresEgress` governs egress from the recorder, the BFF, and, in `postgres` blob
mode, the gateway, to Postgres. An empty `cidrs` list selects the in-cluster CloudNativePG pods
automatically in `cnpg` mode. In `external` mode an empty list selects nothing, so the render
fails and asks you to name the external host.

`recordStoreBlobEgress` governs the same three components' egress to S3-compatible storage. It
renders nothing when `recordStore.blobs.mode` is `postgres`, because that mode needs no external
egress.

Both policies apply only when `networkPolicy.enabled` and `recordStore.enabled` are both true.

In `cnpg` mode the chart also renders a policy for the Postgres pods themselves. It admits the
recorder, the BFF, and the gateway on port 5432, and the CloudNativePG operator on port 8000
from the namespace `networkPolicy.recordStoreCNPGOperatorNamespaceSelector` names (default
`cnpg-system`). It lets the pods reach the API server, which the instance manager watches.

## Remove the database

Disabling `recordStore.enabled` does not delete the database. The chart marks the CloudNativePG
`Cluster` and every role Secret `helm.sh/resource-policy: keep`, so a disabled or uninstalled
release leaves the data in place. To delete the data, remove the `Cluster` and every role Secret
together:

```bash
kubectl delete clusters.postgresql.cnpg.io varroa-recordstore -n <release-namespace>
kubectl delete secret varroa-recordstore-recorder varroa-recordstore-client varroa-recordstore-gateway -n <release-namespace>
```

Deleting only the `Cluster` and re-enabling the store regenerates the Secrets with new passwords,
which locks the recorder out of any volume that survives. Delete both together, or not at all.
