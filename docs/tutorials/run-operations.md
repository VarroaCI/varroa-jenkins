# Operate a Run

Hold a fleet lock across tenants, read why a build failed, and attach a summary line to a run.
Answer a paused input, replay a build with an edited script, and call a job on another tenant.

## Prerequisite

These pages need the [build record store](../operations/build-records.md), the same way the
[Read and run pipelines](pipelines.md) tutorial does. Set `recordStore.enabled: true` in your
Helm values before you start.

## Fleet locks

A job that must not run twice at the same time takes a fleet lock. The `varroa.lock` step holds
the lock, and a second run waits for it. See
[Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the lock name form, the three
lock states, and the timeout budget.

The **Locks** card on the team home lists the locks your tenant holds and the locks its runs wait
on. A row names the lock, the state, the holder, and the time since the wait or the acquisition
started. The card reads the locks again on a `lock` event, at most once every two seconds. It
shows no queue position.

An operator opens `/locks` to list every lock of every cluster and tenant. That page needs
the `fleetlocks:read` verb, which `varroa:operator` and `varroa:admin` hold. The page polls.

A stuck lock frees itself in three ways:

- The holder's block ends.
- The holder's run reaches a terminal state.
- The Varroa operator confirms the holder's pod gone.

Otherwise, force-release a `releasing` lock whose build runs on no cell. Use **Force release** on `/locks`, or:

```bash
varroactl locks release <lock name> --run <run id> --reason "<why>"
```

Both need the `fleetlocks:release` verb. Varroa refuses a held lock or a live pod, and names
its cell. Restart that cell when its Jenkins is down. Each release writes a
`fleetlock.force_released` activity row. A lost reply writes
`fleetlock.force_release_unconfirmed`; read `/locks` before you retry.

## Read a failure digest

A run that fails leads with a **Digest** card, above the **Pipeline** card.

These runs carry a card: `UNSTABLE`, `FAILURE`, `ABORTED`, `NOT_BUILT`, and a started
`INTERRUPTED`. These runs carry no card: a run with no result yet, a successful run, a refused
trigger, and an interrupted run that never started. A started `INTERRUPTED` run carries a card
only when it names a failed stage or failed tests.

The card shows up to five parts. Each part appears on its own.

| Part | Meaning |
|---|---|
| Stage | The innermost failed stage, or a failed branch with no stage |
| Error | That stage's own stored error text |
| Log tail | The last 50 plain-text log lines of that stage and its nested stages. An interrupted run has no log tail |
| Failed tests | The failed, skipped, and total counts of the run's tests |
| Failed cases | At most ten failed cases by class and test name. Each links to that case's history |

The card uses the whole log for the log tail when no stage reports `FAILURE`. It uses the whole
log too when the stored log index names no stage. An aborted run and a run with no stages take this
fallback.

The card carries no log tail when the server cannot read the log. The **Log** tab then shows what
the store holds.

A run with none of the parts carries no card.

## Attach a summary line

A pipeline attaches one line of text to its own run. Call `varroa.summary` with the text:

```groovy
varroa.summary('deployed canary to 10%, holding for 1h')
```

The build page shows the line in its header. The line shows the latest call's text, so a second
call replaces the first. A run that never calls `varroa.summary` shows no line. An empty text
attaches nothing: the run keeps the last text that was not empty.

The call cuts the text at 512 bytes of UTF-8. The cut stops at the largest whole character at or
under that limit, so the stored text stays valid UTF-8. Text under the limit stays unchanged.

The call needs no network round trip. Call it early to show the line during the run.

## Answer a paused input

A pipeline that calls `input` stops and waits for a person. The run reports a running status
while the step waits.

```groovy
input message: 'Deploy to production?',
      ok: 'Deploy',
      submitter: 'varroa-payments',
      parameters: [choice(name: 'ENV', choices: ['staging', 'prod'])]
```

The card appears a few seconds after the step pauses. It holds the message, the `ok` label, a
form built from the step's parameters, and an abort control. A parameter with a choices list
becomes a select. A boolean parameter becomes a checkbox. A password parameter becomes a password
field. Every other parameter becomes a text field. The card names the `submitter` when the step
declares one. A caller without the tenant write grant sees the card and no control.

Answer the step from the card, or from an MCP client.

| Tool | Arguments |
|---|---|
| `proceed_run_input` | `run`, `input`, and an optional `parameters` map |
| `abort_run_input` | `run` and `input` |

Both tools apply the tenant write check on the run's job path.

Fill the form and select **Deploy**. The page reports that the request was sent. The card leaves
when the run record no longer carries the input. A refused answer or cancel shows a notice on the
build page with the sender, the HTTP status, and the error.

Jenkins writes the approving user to the build log, which the **Log** tab shows. Varroa keeps no separate record of the approval.

A step's `submitter` accepts a user name or a Varroa group name. A `submitter` that names a
Varroa group admits any caller who holds that group. A Jenkins administrator on the controller can answer
every input, whatever the `submitter` names.

## Replay a build with an edited script

The **Replay** control on the build page starts a new build of the job with a script you edit. The
control needs the tenant's `pipelines` write grant.

Select **Replay**. The editor opens with the build's own script. Edit the script, then submit it.
The editor also shows the library scripts that the build loaded. These scripts are read only. The
new build loads them again from their library, so a library change reaches the replay.

A build shows the control only when it meets all five conditions:

| Condition | Meaning |
|---|---|
| It ended | The build reached a terminal state |
| It ran on a cell | The cell kept the build record that holds the script |
| It used the Groovy sandbox | The build ran its script in the sandbox |
| It declared no template | No pipeline template or marker file defined the job |
| It used no masked parameter | The build took no password or other masked parameter value |

The new build takes its own build number from the ledger. The run record names the source run in
`replayOf`. A refused replay shows **Build not started** on the new build's page.

A headless tenant sees the same control, because the dashboard is the only surface of that tenant.

## Restart a build from a stage

The **Restart from stage** control on the build page starts a new build of the same job. The new
build begins at the stage that you choose. Stages before that stage do not run again. The new build
keeps the parameter values of the source build. The control needs the tenant's `pipelines` write grant.

Select **Restart from stage**, choose a stage, and select **Restart**. The new build opens. A
refused restart shows the reason on the drawer.

A build shows the control only when it meets all of these conditions:

| Condition | Meaning |
|---|---|
| It ended | The build reached a terminal state |
| It is declarative | The build used a `pipeline { }` definition. Jenkins does not restart a scripted pipeline from a stage. |
| It has a restartable stage | At least one stage of the build can restart. A stage skipped for a failure in an earlier stage cannot restart. |
| It is not template defined | No pipeline template or marker file defined the job |
| It ran on the job's current cell, or keeps a restart copy | The cell that ran the build is still the job's home cell, or the run has a complete restart copy |

Varroa keeps a restart copy of a declarative cell run. The copy holds the build's `build.xml`, its
`workflow-completed` files, and its `libs` files. Varroa writes the copy to the blob store after the
run ends. The copy stays for 7 days by default, set by `recordStore.retention.restartSource`. It
never lasts longer than the run record. After the copy expires, a restart needs the source build on
the job's current cell.

A restart of a build whose job moved, or whose cell pod rolled, restores the copy before it runs.
A restart after a pod roll replaces the rehydrated build record with the kept copy.
When no complete copy exists, and the job moved, the restart is refused with `home-moved`.
The plugin counts a copy in `varroa_plugin_restart_source_kept` only after the gateway stores every file.
A refused copy counts in `varroa_plugin_restart_source_dropped`.

A restarted stage can use `unstash` only when the job keeps its stashes. A preserved stash survives
a pod roll of the same cell, but not a move of the job to another cell. Add this option to the
job:

```groovy
options {
    preserveStashes(buildCount: 5)
}
```

A job that sets `disableRestartFromStage()` in its `options` block cannot restart from a stage.

The new run names the source run in `restartOf`, and the stage in `restartStage`. The build page
shows the link, and the run detail carries the same two fields. The restart answer is in the `restart`
object of the run detail. It lists the stages that a restart can use.

An MCP client uses the `restart_run` tool with `run` and `stage`. The `get_run` tool returns the
`restart` object, which names the stages. The tool applies the tenant write check on the run's job path.

## Call a job on another tenant

Two pipelines tenants, a producer and a caller, connect through a grant and a trigger.

On the producer tenant's `Team`, export the job to the caller:

```yaml
spec:
  pipelines:
    profile: jenkins-lts
    agentNamespace: team-platform
    exports:
      - job: "release"
        callers: ["payments"]
```

Apply the `Team` and wait about one reconcile tick for the grant to reach the record store.

On the caller tenant's job item, declare a `varroaUpstream` trigger naming the producer:

```yaml
triggers:
  - varroaUpstream:
      tenant: platform
      job: release
      result: SUCCESS
```

Wait up to ten minutes for the caller's cell to export its schedules. A producer run that
completes before that export starts nothing. Then run the producer's job to success. The trigger
starts the caller's job.

Open either job's page and check the **Triggers** card. The caller's job shows an **Upstream**
row naming the producer job; the producer's job shows a **Downstream** row naming the caller.
Neither row carries **grant missing**, because the export covers the call. Select an edge to
open that job's page.

A pipeline can also call another tenant's job directly, without a declared trigger, using
`varroa.build`. A direct call runs once, on demand, and does not add a row to either job's
**Triggers** card. See [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the
full grant syntax, the direct call, the fleet lock, and the timeout budgets.
