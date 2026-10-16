# Pipeline Mesh

A cell backs its Jenkins home with ephemeral storage, so a cell that dies takes every running
Pipeline build with it. The mesh replicates each build's state files off the cell, and the next cell
continues the build.

The mesh needs the [build record store](build-records.md) and the [cell loss](cell-loss.md) signal.
It resumes a Pipeline build only.

## What survives, and what repeats

The plugin pushes the build's state files every two seconds while it moves. A long step gets one
push every 30 seconds. A resumed build starts again from its last push, so **the work since then
repeats**. The resumed build replicates from its own process, so a second loss repeats only the work
since the resume.

A step that is not safe to repeat should hold a fleet lock, whose token survives the resume:

```groovy
varroa.lock(name: 'deploy-production') {
  sh './deploy.sh'
}
```

## The durability requirement

Jenkins writes a build's flow program at a frequency its durability setting decides. A lower setting
writes only at a checkpoint, so a copy can be stale by whole stages. Varroa therefore sets
`MAX_SURVIVABILITY` in every cell, and the plugin pushes nothing below that level. A job with its own
durability property has no mesh.

## The two windows

Each wait is a constant. The **arm window** is 15 minutes from the claim, and bounds the wait for a
replacement cell. The **resume deadline** is 6 minutes from the dispatch, and bounds the resume.
Past either window the run reads `INTERRUPTED` with the reason `resume-timeout`.

**The arm window delays the mark.** Without the mesh, a lost run is marked 30 seconds after the
confirmation. With the mesh it waits up to 15 minutes. That wait has two costs: the pipelines page
shows a run whose cell is gone, and the run's fleet lock stays held.

**A failed resume is tried again on a new pod.** A pod that cannot fetch the files, or that rolls
first, reports it. Varroa sends the resume again inside the same arm window, never to a pod
that already failed. Once a pod loads the build no other may take it.

## The size bound

A run's replicated state stays at or below 64 MiB. Over that bound Varroa deletes it, and the run
takes the ordinary path. A gateway restart lets one run overshoot, because that counter lives in
memory. The next push reclaims it.

**An active resume holds that delete.** Varroa keeps the state until the target reports or its
deadline passes.

**A run's state is deleted when the run ends.** An hourly round removes the state of any run
terminal for more than an hour, so a stopped recorder leaves nothing.

## The stored copy

Varroa stores each state file under its own push: the Jenkins process and its push count. A resume
reads the push the recorder recorded. A late push cannot overwrite a newer one, whichever gateway
pod writes it.

The plugin keeps each file until the gateway stores it and records it. A failed record holds the file, and the plugin sends it again.

When the gateway cannot find the tenant of a run, it answers busy with the reason `tenant-unresolved`, and the plugin sends the file again after the pause. After 60 seconds it refuses the run as `foreign-run`. A mite reconnect to the same gateway replica keeps that replica's clock while the clock is retained. Gateway replicas do not share clocks, so another replica may start a new clock. The run then stops replicating its state, and a later resume takes the ordinary path. The same rule applies to the restart copy.
