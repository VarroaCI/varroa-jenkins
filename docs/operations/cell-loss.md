# Cell Loss

A cell that dies takes its running builds with it. Varroa detects the loss, marks the lost builds,
and re-fires a build that was granted but never started.

Cell loss detection needs the [build record store](build-records.md). An install with the store
disabled marks nothing. A tenant that runs the
[pipeline mesh](pipeline-mesh.md) resumes a running Pipeline build instead, after that page's own
windows.

## Two signals make one fact

A stream loss alone is never proof: a network partition leaves a live Jenkins process running
builds. A dead stream only makes the operator reconcile sooner.

A pod read is proof. The operator writes the pod's unique identifier into
`Controller.status.cell.instance`. A pod that is absent, `Failed`, or `Succeeded` counts as no
pod; a `Running` or `Pending` pod does not. A failed read clears nothing, so an API outage never
reports a live cell as gone. The placement announcement names each confirmed instance and the
time its read proved it.

A confirmed loss reaches a mark in about 70 to 130 seconds. Without a confirmation the recorder
falls back to two fixed windows: five minutes for a run whose owning instance changed, and fifteen
minutes for a silent run on the current instance.

The operator keeps the replaced instance of each cell in `status.cell.previousInstance`, and
reports it in every announcement. A pod that rolls while the operator restarts, or while the
recorder is down, is still confirmed. The 70 to 130 seconds start at the next
accepted announcement.

The recorder also compares each announcement with the members it stored. A replaced or removed
member pod becomes an unconfirmed dead instance with the five-minute window. This covers two
rolls during one recorder outage, and a cell deleted while the operator restarts. The operator
reads a left cell until its old pod is gone. A pod that a later announcement names again leaves
the list.

The recorder refuses a late announcement from an old operator replica: a lower generation, a
pod it knows replaced, or an object that a new one of the same name replaced.

The recorder measures each window on its own clock, so a drifting cell clock moves none. A
heartbeat from an instance that no longer owns the run moves no window.

## The grace

```yaml
recordStore:
  cellLoss:
    confirmedGoneGrace: 30s
```

The grace is how long a confirmation waits before the recorder counts the instance gone. An empty,
unparsable, or non-positive value falls back to `30s`. The operator can clear its stored cell
instance on the tick a replacement pod appears. A zero grace would then interrupt a run the new
pod is about to adopt.

## What a user sees

A marked build reads `INTERRUPTED` on the build page and writes one `build.interrupted` row in the
activity feed. The reason names the signal that decided. A completion webhook also sends one `synthetic` delivery.

| Reason | Meaning |
|---|---|
| `cell-lost` | the operator read the cell pod and found none |
| `cell-rolled` | the cell's pod changed and the old instance stopped reporting |
| `tenant-moved` | the tenant moved and the old instance stopped reporting |
| `no-heartbeat` | the run was silent for fifteen minutes on a cell reported not alive |
| `resume-timeout` | the mesh claimed the run and the run did not come back |
| `start-lost` | no start event arrived 75 minutes after the start mark |
| `heartbeat-lost` | the running build was silent for 60 minutes |

A `cell-lost` mark sends no cancel command: the pod is gone. A
fleet lock the lost run held is released on the same schedule. A pod read that proved its owner
gone ends the five-minute wait.

A `start-lost` mark sends no cancel and no retry: the build can still start and replace it.
A `heartbeat-lost` mark sends neither, and the build's own completion replaces it. A Jenkins restart
in the same pod sends it at boot. The mark also
ends a run on a dead cell when a stopped operator reports no liveness.

The lost instance's agent pods keep running. A resume adopts a pod that ran a build step, so a
Running pod stays for about 41 minutes, or about 46 when no running cell holds the slot. A pod that is not Running goes within about four minutes. See
[Agent pod lifetime](tenant-pod-templates.md#agent-pod-lifetime).

## The re-fire

A build can be granted and never start, because its cell dies first. Such a build writes no run
record, so no sweep can see it.

The recorder re-fires it on the tenant's current cell, with the same run identifier and intent key,
so the rebuilt run keeps its identity and its build number. Five rules gate it.

| Rule | Behavior when it fails |
|---|---|
| The owning cell instance is confirmed gone | no re-fire; a stream loss alone is never enough |
| The build carries no parameters | no re-fire; a person runs the build again |
| The tenant holds a live cell and cell instance | the re-fire retries every 60 seconds |
| The recorder has not re-fired this build | no second re-fire, ever |
| The build holds no start mark | no re-fire; the run ends `cell-rolled` or `start-lost` |

The second rule is permanent: Varroa stores no parameters for a build that never started, so a
re-fire would run the job with each declared default.

## A backstop fire during a cell roll

A backstop fire can reach a cell pod that still boots. The mite then waits up to five minutes for
its Jenkins before it sends the trigger. A trigger that still reaches no Jenkins fails with
`TRIGGER_UNREACHABLE`, and the backstop fires the occurrence again on a later round.

A re-fire waits until every earlier fire of the occurrence has failed, or until ten minutes have
passed since the last fire. It also waits until `cronGrace` has passed since the last fire. It goes to the job's home slot. The backstop re-fires one occurrence
at most three times. Any other failure code is permanent, so no round fires that occurrence again.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `recorder_cell_loss_confirmed_total` | `path` | runs marked from a confirmed loss |
| `recorder_intent_refired_total` | `outcome`, `reason` | re-fire outcomes |
| `recorder_cron_refired_total` | none | backstop re-fires of an unreachable trigger |
| `recorder_interrupted_marked_total` | `path`, `status` | every interrupted mark |
| `recorder_retry_skipped_total` | `reason` | automatic retries not published |
| `recorder_run_resume_total` | `outcome`, `reason` | mesh resume outcomes |
| `recorder_run_resume_timeout_total` | none | runs marked on a resume window |
