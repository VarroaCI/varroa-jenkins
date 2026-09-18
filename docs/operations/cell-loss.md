# Cell Loss

A cell that dies takes its running builds with it. Varroa detects the loss, marks the lost builds,
and re-fires a build that was granted but never started.

Cell loss detection needs the [build record store](build-records.md). An install with the store
disabled records no run and marks nothing. A tenant that runs the
[pipeline mesh](pipeline-mesh.md) resumes a running Pipeline build instead, after that page's own
windows.

## Two signals make one fact

A stream loss alone is never proof: a network partition leaves a live Jenkins process that still
runs builds, and marking those builds lost would strand them on two owners. A dead stream only
makes the operator reconcile sooner.

A pod read is proof. The operator writes the pod's unique identifier into
`Controller.status.cell.instance`. A pod that is absent, `Failed`, or `Succeeded` counts as no
pod; a `Running` or `Pending` pod does not. A failed read clears nothing, so an API outage never
reports a live cell as gone. The operator names each confirmed instance, with the time its read
proved it, in the placement announcement.

A confirmed loss reaches a mark in about 70 to 130 seconds. Without a confirmation the recorder
falls back to two fixed windows: five minutes for a run whose owning instance changed, and fifteen
minutes for a silent run on the current instance.

The recorder measures each window on its own clock, and stamps the time each heartbeat arrives.
A heartbeat from an instance that no longer owns the run moves no window. A drifting cell clock
therefore moves none.

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
activity feed. The reason names the signal that decided.

| Reason | Meaning |
|---|---|
| `cell-lost` | the operator read the cell pod and found none |
| `cell-rolled` | the cell's pod changed and the old instance stopped reporting |
| `tenant-moved` | the tenant moved and the old instance stopped reporting |
| `no-heartbeat` | the run was silent for fifteen minutes on a cell reported not alive |
| `resume-timeout` | the mesh claimed the run and the run did not come back |

A `cell-lost` mark sends no cancel command, because the pod is gone and holds no process to stop. A
fleet lock the lost run held is released on the same schedule. A pod read that proved its owner
gone ends the five-minute wait.

## The re-fire

A build can be granted and never start: the trigger reaches the ledger, the ledger assigns a build
number, and the cell dies first. Such a build writes no run record, so no sweep can see it.

The recorder re-fires it on the tenant's current cell, with the same run identifier and intent key,
so the rebuilt run keeps its identity and its build number. Four rules gate it.

| Rule | Behavior when it fails |
|---|---|
| The owning cell instance is confirmed gone | no re-fire; a stream loss alone is never enough |
| The build carries no parameters | no re-fire; a person runs the build again |
| The tenant holds a live cell and cell instance | the re-fire retries every 60 seconds |
| The recorder has not re-fired this build | no second re-fire, ever |

The third rule holds a re-fire until a replacement pod reports, which takes minutes. The second
rule is permanent: Varroa stores no parameters for a build that never started, so a re-fire would
run the job with each declared default.

A marked run and a re-fired build do not conflict. The mark leaves the ledger entry alone, so the
re-fired build claims the same run identifier.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `recorder_cell_loss_confirmed_total` | `path` | runs marked from a confirmed loss |
| `recorder_intent_refired_total` | `outcome`, `reason` | re-fire outcomes |
| `recorder_interrupted_marked_total` | `path`, `status` | every interrupted mark |
| `recorder_run_resume_total` | `outcome`, `reason` | mesh resume outcomes |
| `recorder_run_resume_timeout_total` | none | runs marked on a resume window |
