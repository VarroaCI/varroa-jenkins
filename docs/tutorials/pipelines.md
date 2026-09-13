# Read and Run Pipelines

Open a team's pipelines, read a build's log, download an artifact, and start a build with
parameters.

## Prerequisite

The pipelines pages need the [build record store](../operations/build-records.md). Set
`recordStore.enabled: true` in your Helm values before you start. A cluster with the store off
still shows the **Pipelines** row, but every pipelines page explains that the store is off
instead of listing builds.

## Open a team

Sign in and select **Pipelines** in the sidebar. Access follows the team: you see the pipelines
of every [Team](../security/varroa-rbac.md#grant-pipelines-access) that grants you a `pipelines`
verb. `/pipelines` lists those teams and opens the one team you belong to automatically. When
you belong to more than one team, pick one from the list.

## Your cell card

The team page heads with a **Your cell** card when the team runs on a
[cell](../operations/cells.md). The card names the cell, its pool, its version profile, and its
Jenkins version. It also reports the free tenant slots on the cell, the length of the cell's build
queue, and your own running builds.

One line counts the other teams on the cell. The card names none of them. It shows no other team's
job, build, or count.

**Cell details** opens the cell's controller page. The link appears only when your access covers
that page. Without it you still read every field of the card.

**Updated** states when the browser last read the record. The browser reads it again every 30
seconds.

The card reports one of five states.

| State | What the card shows |
|---|---|
| Placed | the whole card |
| Moving | the whole card, plus a note that Varroa is moving the team to another cell |
| Rehydrating | the whole card, plus a note that the cell is rebuilding the team's folders |
| Not placed | the badge and the reason only. The team waits for its first cell |
| No card | the team runs on its own controller, so no cell hosts it |

**Reason** carries the operator's own text for the placement, such as `TenantPlaced` or
`CellNotReady`. Ask an operator what to do about a reason you do not recognize.

A cell in another cluster reports no queue length. The **Queue** figure then reads `not reported`,
which is not the same as an empty queue.

**Free slots** counts the free tenant slots of this cell alone. The cell's pool can hold a lower
cap, which stops a new team from landing on the cell even while the cell reports a free slot. An
operator reads both numbers on the cell page.

## Read the pipelines tree

Below the cell card the team page shows two more cards.

**Running now** holds one tile per build in progress: the job name, the build number, the
current stage, the branch, the commit, and a progress bar against the job's usual duration.

**Pipelines** holds the team's folder tree. A job row carries its health, its last build, its
branch, its last eight build durations, and when it last ran. Health is a weather glyph: clear
when the last five runs passed, cloudy when one or two failed, and rain when three or more
failed. A job that has never run shows no glyph and an empty last build.

A folder row carries a fold control, the folder name, and two counts: the number of jobs below
it, and the number of those jobs building now. A folder reports no health value of its own.

A multibranch item is a folder row that counts branches instead of jobs. It offers no **Run**
control, because a multibranch item does not build. Its branches sit below it.

Row order is fixed. At each level the folders come first, then the jobs, each group in
alphabetical order.

A first load unfolds the top two levels and folds every deeper level.

The fold state lives in the page URL, as one `open` value per unfolded folder:

```text
/pipelines/payments?open=release&open=release/eu
```

Copy that URL to share the same view. A reload keeps the folds. `?open=` with no value folds
everything.

A caller whose grant carries `jobs` globs reads only the matching jobs and only the matching
running builds. A folder holding no readable job does not appear at all. See
[Varroa RBAC](../security/varroa-rbac.md#scope-a-binding-to-a-pipelines-tenant) for how an
administrator writes those globs.

Select a job's name to open its pipeline page.

## Open a build

The pipeline page carries a breadcrumb trail under the job name. The trail names the team, then
each folder, then the job. Every crumb except the last is a link. Select a folder crumb to open
the team page with that folder, and every folder above it, already unfolded.

The **Defined by** card states where the job's script comes from. A job that reads a Jenkinsfile
from source control shows the repository and the last built commit. A job whose bundle item
carries the script inline shows a **Script** row that says so. The card also names the catalog
template, when the job has one, and the version profile of the controller.

The pipeline page lists the job's builds in a table, with filters for branch, result, actor, and
cause. Each filter you set stays in the page URL, so a filtered view is shareable and survives a
reload.

Select a build number to open its build page. The build page shows the build's status, its stage
graph, and four tabs: **Log**, **Artifacts**, **Tests**, and **Changes**.

An interrupted build shows a banner naming the reason, and a link to its retry when one exists.

## Read a log

Open the **Log** tab. The log viewer supports:

- A search box, bound to `/`.
- A **First error** jump to the first line matching `error`, `failed`, `failure`, or
  `exception`.
- A **Timestamps** toggle.
- A **Follow** toggle that keeps the view at the newest line, and turns off when you scroll up.
- A **Raw** link that opens the same log as plain text in a new tab.

Select a stage in the stage graph to scroll the log to that stage's lines. Every line carries a
permalink: append `#L<line number>` to the build page URL to open it scrolled to that line and
highlighted.

The **Log** tab shows a banner naming the reason when the run's log did not close complete, for
example after a lost chunk. A log carrying that banner is missing bytes; do not treat it as the
full record.

These keys work on the build page, outside the search box.

| Key | Action |
|---|---|
| `j` | Open the next older build |
| `k` | Open the next newer build |
| `l` | Move focus to the log viewer |
| `/` | Focus the log search box |
| `f` | Toggle follow |
| `t` | Toggle timestamps |

## Download an artifact

Open the **Artifacts** tab. Each row shows the artifact's path and size, with a **Download**
control. The control is disabled, with an explanation, for an artifact Jenkins still stores only
on its own local disk; see [Build logs and artifacts](../operations/build-logs-and-artifacts.md)
for the storage modes that make an artifact downloadable.

## Run a job with parameters

On the pipeline page, select **Run**. This control is present only for a caller who holds the
tenant's `pipelines` write grant.

The drawer renders the job's declared parameters: a text field, a checkbox, a selection for a
choice parameter, a text area, or a password field. Fill them in and submit. A repeated submit
of the same open drawer starts one build, not two.

Submitting navigates to the new build's page, which shows `Requested` until the build starts. A
build that has not started within one minute shows `Not started`; the page keeps polling, so a
late-starting build still resolves.

To start a new run of a build you are already viewing, use **Re-run** on the build page. It is
disabled once a retry of that build already exists, because Varroa allows one retry per build.

## Concurrent runs of the same job

A job can start a new build while an older one of the same job is still running.
`Team.spec.pipelines.concurrency` decides what happens next. `allow`, the default, runs both
side by side. `cancelInProgress` cancels the older run fleet-wide as soon as the new one is
claimed.

The setting is per tenant, and the group it cancels within is the job path itself, not the
build. A multibranch child job's path includes its branch, so a new build on one branch never
cancels a build on another branch.

## Call a job on another tenant

Two pipelines tenants, a producer and a caller, connect through a grant and a trigger.

On the producer tenant's `Team`, export the job to the caller:

```yaml
spec:
  pipelines:
    exports:
      - job: "release"
        callers: ["payments"]
```

Apply the `Team` and wait about one reconcile tick for the grant to reach the record store.

On the caller tenant's job, declare a `varroaUpstream` trigger naming the producer:

```groovy
triggers {
    varroaUpstream(tenant: 'platform', job: 'release', result: 'SUCCESS')
}
```

Run the producer's job to success. The trigger starts the caller's job.

Open either job's page and check the **Triggers** card. The caller's job shows an **Upstream**
row naming the producer job; the producer's job shows a **Downstream** row naming the caller.
Neither row carries **grant missing**, since the export now covers the call.

A pipeline can also call another tenant's job directly, without a declared trigger, using
`varroa.build`. A direct call runs once, on demand, and does not add a row to either job's
**Triggers** card. See [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the
full grant syntax, the direct call, the fleet lock, and the timeout budgets.
