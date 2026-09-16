# Cell Loss

A cell that dies takes its running builds with it. Varroa detects the loss, marks the lost builds,
and re-fires a build that was granted but never started.

Cell loss detection needs the [build record store](build-records.md). An install with the store
disabled records no run and marks nothing.

## Two signals make one fact

Two signals reach Varroa at different speeds. Neither decides on its own.

| Signal | Source | Speed | Meaning |
|---|---|---|---|
| The mite stream died | the gateway | about 40 seconds | the connection is gone; not proof |
| The cell pod is absent | the operator's own pod read | one reconcile tick | proof |

A stream loss alone is never proof. A network partition leaves a live Jenkins process that still
runs builds. Marking those builds lost would strand them on two owners.

A pod read is proof. The operator reads the cell pod on every tick and writes the pod's unique
identifier into `Controller.status.cell.instance`. A pod that is absent, in phase `Failed`, or in
phase `Succeeded` counts as no pod. A pod in phase `Running` or `Pending` does not. A failed pod
read clears nothing, so an API outage never reports a live cell as gone.

The stream loss only makes the pod read happen sooner. The operator subscribes to the gateway's
disconnect event and reconciles that controller at once, instead of waiting for its 30-second
interval. A missed event costs one interval and nothing more.

The operator sends the result to the recorder in the placement announcement. The announcement
lists each cell instance the operator confirms gone, with the time its own pod read proved it.

## Timing

| Step | Time |
|---|---|
| The gateway sees the stream die | about 40 s |
| The operator reads the pod | under 1 s after the disconnect event |
| The operator announces the confirmation | under 1 s after the pod read |
| The confirmed-gone grace elapses | 30 s, configurable |
| The recorder marks the lost runs | 0 s to 60 s |

A confirmed loss therefore reaches a mark in about 70 to 130 seconds.

Without a confirmation the recorder falls back to its heartbeat windows: five minutes for a run
whose owning cell instance changed, and fifteen minutes for a silent run on the current instance.
Those windows are fixed and are not settings.

## The grace

```yaml
recordStore:
  cellLoss:
    confirmedGoneGrace: 30s
```

The grace is how long a confirmation waits before the recorder counts the instance gone. It is a
duration string. An empty, unparsable, or non-positive value falls back to `30s`.

The grace exists because the operator can clear its stored cell instance on the same tick a
replacement pod appears. A zero grace would then interrupt a run the new pod is about to adopt.

## What a user sees

A marked build reads `INTERRUPTED` on the build page and writes one `build.interrupted` row in the
activity feed. The reason names the signal that decided.

| Reason | Meaning |
|---|---|
| `cell-lost` | the operator read the cell pod and found none |
| `cell-rolled` | the cell's pod changed and the old instance stopped reporting for five minutes |
| `tenant-moved` | the tenant moved to another cell and the old instance stopped reporting |
| `no-heartbeat` | the run sent no heartbeat for fifteen minutes on a cell reported not alive |

A `cell-lost` mark sends no cancel command. The pod is gone and holds no process to stop.

A fleet lock the lost run held is released on the same schedule. A lock does not wait out the
five-minute window when a pod read already proved its owner gone.

## The re-fire

A build can be granted and never start. The trigger reaches the ledger, the ledger assigns a build
number, and the cell dies before Jenkins starts the build. Such a build writes no run record at
all, so no sweep can see it.

The recorder re-fires that build on the tenant's current cell. It publishes one trigger carrying
the same run identifier and the same intent key, so the rebuilt run keeps its identity and its
build number.

Four rules gate a re-fire.

| Rule | Behavior when it fails |
|---|---|
| The owning cell instance is confirmed gone | no re-fire; a stream loss alone is never enough |
| The build carries no parameters | no re-fire; the build stays for a person to run again |
| The tenant holds a live cell and a live cell instance | the re-fire waits and retries every 60 seconds |
| The recorder has not re-fired this build before | no second re-fire, ever |

**A re-fire waits for a replacement pod.** The tenant has no live cell instance at the moment of
the confirmation, so the third rule holds the re-fire until a replacement pod reports. That wait is
the cell pod's own recovery, which takes minutes. The re-fire lands on the first round after the
replacement pod reports.

**A parameterized build is never re-fired.** Varroa stores no parameters for a build that never
started. A re-fire would run the job with each declared default instead of what the caller asked
for. The build keeps its `cell-lost` mark, and a person reruns the job from the pipelines page.

A marked run and a re-fired build do not conflict. The mark leaves the build's ledger entry alone,
so the re-fired build claims the same run identifier. The run reads `INTERRUPTED` for a few seconds
and then reads `RUNNING` again.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `recorder_cell_loss_confirmed_total` | `path` | runs marked from a confirmed loss, by `announce` or `sweep` |
| `recorder_intent_refired_total` | `outcome`, `reason` | re-fire outcomes: `published`, or `skipped` with `parameterized` or `no-placement` |
| `recorder_interrupted_marked_total` | `path`, `status` | every interrupted mark, whatever the reason |
