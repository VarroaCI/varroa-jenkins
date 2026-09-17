# Pipeline Mesh

A cell backs its Jenkins home with ephemeral storage, so a cell that dies takes every running
Pipeline build with it. The mesh replicates each running build's state files off the cell, and the
tenant's next cell continues the build.

The mesh needs the [build record store](build-records.md) and the [cell loss](cell-loss.md)
signal. It resumes a Pipeline build only. Every other run takes the ordinary path.

## What survives, and what repeats

The plugin pushes the build's state files every two seconds while the build moves. A long step gets
one push every 30 seconds. A resumed build starts again from its last push, so **the work since
that push repeats**, as it does after a Jenkins restart.

A step whose side effect is not safe to repeat should hold a fleet lock, whose token stays readable
across the resume:

```groovy
varroa.lock('deploy/production') {
  sh './deploy.sh'
}
```

## The durability requirement

Jenkins writes a build's flow program at a frequency its durability setting decides. A lower
setting writes at a checkpoint, not at each step. A copied set can then be stale by whole stages,
and a resume would repeat finished work under the same build number.

Varroa therefore sets `MAX_SURVIVABILITY` in every cell's configuration, and the plugin pushes
nothing for a build below that level. A job that sets its own durability property overrides that
value and has no mesh.

## The two windows

A resume waits twice, and each wait is a fixed constant. The **arm window** is 15 minutes from the
moment Varroa claims the run, and bounds the wait for a replacement cell. The **resume deadline**
is 6 minutes from the dispatch, and bounds the resume itself. Past either window the run reads
`INTERRUPTED` with the reason `resume-timeout`, and a late command restores nothing.

**The arm window delays the mark, and that delay buys the resume.** Without the mesh, a
confirmed-lost run is marked 30 seconds after the confirmation. With the mesh, a claimed run waits
up to 15 minutes. Two costs come with that wait.

| Cost | Detail |
|---|---|
| the run reads as running for longer | the pipelines page shows a live run whose cell is gone |
| the run's fleet lock stays held | a second run waiting on that name waits with it |

The second cost is the reason the window is not longer.

## The size bound

A run's replicated state stays at or below 64 MiB. Over that bound Varroa deletes the run's state,
and the run takes the ordinary path. A gateway restart lets one run overshoot the bound, because that
counter lives in memory. The recorder reclaims the overshoot on the run's next push.

**A run's state is deleted when the run ends.** An hourly round then removes the state of any run
terminal for more than an hour. A recorder that stopped before its own delete therefore leaves
nothing behind.
