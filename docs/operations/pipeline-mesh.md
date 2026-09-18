# Pipeline Mesh

A cell backs its Jenkins home with ephemeral storage, so a cell that dies takes every running
Pipeline build with it. The mesh replicates each build's state files off the cell, and the tenant's
next cell continues the build.

The mesh needs the [build record store](build-records.md) and the [cell loss](cell-loss.md)
signal, and it resumes a Pipeline build only.

## What survives, and what repeats

The plugin pushes the build's state files every two seconds while it moves. A long step gets
one push every 30 seconds. A resumed build starts again from its last push, so **the work since
that push repeats**.

A step whose side effect is not safe to repeat should hold a fleet lock, whose token stays readable
across the resume:

```groovy
varroa.lock('deploy/production') {
  sh './deploy.sh'
}
```

## The durability requirement

Jenkins writes a build's flow program at a frequency its durability setting decides. A lower
setting writes at a checkpoint, so a copied set can be stale by whole stages.

Varroa therefore sets `MAX_SURVIVABILITY` in every cell, and the plugin pushes nothing below that
level. A job that sets its own durability property has no mesh.

## The two windows

Each wait is a constant. The **arm window** is 15 minutes from the moment Varroa claims the run, and
bounds the wait for a replacement cell. The **resume deadline** is 6 minutes from the dispatch, and
bounds the resume itself. Past either window the run reads `INTERRUPTED` with the reason
`resume-timeout`.

**The arm window delays the mark, and that delay buys the resume.** Without the mesh, a
confirmed-lost run is marked 30 seconds after the confirmation. With the mesh it waits up to 15
minutes. Two costs come with that wait.

| Cost | Detail |
|---|---|
| the run reads as running for longer | the pipelines page shows a live run whose cell is gone |
| the run's fleet lock stays held | a second run waiting on that name waits with it |

That cost is why the window is not longer.

**A resume that fails is tried again on a new pod.** A pod that cannot fetch the state files, or that
rolls first, reports the failure. Varroa sends the resume again inside the same arm window, to a
replacement pod of the same cell or to another cell. It never picks a pod that already failed, and
once a pod loads the build no other may take it.

## The size bound

A run's replicated state stays at or below 64 MiB. Over that bound Varroa deletes it, and the
run takes the ordinary path. A gateway restart lets one run overshoot, because that counter lives in
memory. The recorder reclaims it on the next push.

**A run's state is deleted when the run ends.** An hourly round removes the state of any run
terminal for more than an hour, so a stopped recorder leaves nothing.
