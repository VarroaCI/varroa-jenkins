# Read and Run Pipelines

Open a team's pipelines, read a build's log, download an artifact, and start a build with
parameters.

## Prerequisite

The pipelines pages need the [build record store](../operations/build-records.md). Set
`recordStore.enabled: true` in your Helm values before you start. A cluster with the store off
still shows the **Pipelines** row, yet every page explains the store is off instead of listing
builds.

## Open a team

Sign in and select **Pipelines** in the sidebar. Access follows the team: you see the pipelines
of every [Team](../security/varroa-rbac.md#grant-pipelines-access) that grants you a `pipelines`
verb. `/pipelines` lists those teams and opens your only team automatically. With more than one
team, pick one from the list.

## Your cell card

A team on a [cell](../operations/cells.md) shows a placement pill under the page title. The pill
names the state and the cell. The **Your cell** card sits behind the pill and starts closed.

Select the pill to open the card. The card names the cell, its pool, its version profile, and its
Jenkins version. It also reports the free tenant slots on the cell, the length of the cell's build
queue, and your own running builds.

One line counts the other teams on the cell, and names none of them. It shows no other team's
job, build, or count.

**Cell details** opens the cell's controller page, when your access covers that page. Without it
you still read every field.

**Updated** states when the browser last read the record, which it reads again every 30 seconds.

The card reports one of five states.

| State | What the card shows |
|---|---|
| Placed | the whole card |
| Moving | the whole card, plus a note that Varroa is moving the team to another cell |
| Rehydrating | the whole card, plus a note that the cell is rebuilding the team's folders |
| Not placed | the badge and the reason only. The team waits for its first cell |
| No card | the team runs on its own controller, so no cell hosts it |

**Reason** carries the operator's own text for the placement, such as `TenantPlaced` or
`CellNotReady`. Ask an operator about a reason you do not recognize.

A cell in another cluster reports no queue length. The **Queue** figure then reads `not reported`,
which is not an empty queue.

**Free slots** counts the free tenant slots of this cell alone. The cell's pool can hold a lower
cap, which stops a new team from landing even while the cell reports a free slot. An operator
reads both numbers on the cell page.

## Read the pipelines tree

Four cards follow the page head: **Running now**, **Pipelines**, **Locks**, and **Runners**.

**Running now** holds one tile per build in progress: the job name, the build number, the
current stage, the branch, and a progress bar against the job's usual duration. The stage line
names the run's top-level stage in progress. A running build with no stage in progress reads
`Starting`, and a build that has not started reads `Queued`.

**Pipelines** holds the team's folder tree. A job row carries its health, its last build, its
branch, its last eight build durations, and when it last ran. **Queue wait** shows the job's
median wait between its queued and started moments, over the last 30 days. Health is a weather
glyph: clear when the last five runs passed, cloudy when one or two failed, and rain when three
or more failed. A job that has never run shows no glyph and an empty last build.

A folder row carries a fold control, the folder name, and two counts: the jobs below it, and how
many of those build now. A folder reports no health value.

A multibranch item is a folder row that counts branches instead of jobs. It offers no **Run**
control, because it does not build. Its branches sit below it.

Row order is fixed: at each level the folders come first, then the jobs, each group in
alphabetical order.

A first load unfolds the top two levels and folds every deeper level.

The fold state lives in the page URL, as one `open` value per unfolded folder:

```text
/pipelines/payments?open=release&open=release/eu
```

Copy that URL to share the same view. A reload keeps the folds. `?open=` with no value folds
everything.

**Runners** lists the [external runners](../operations/runners.md) bound to the team. Each row
shows the launcher, the labels, the cell, the connection state, and the last contact time.

A caller whose grant carries `jobs` globs reads only the matching jobs and running builds. A
folder holding no readable job does not appear at all. See
[Varroa RBAC](../security/varroa-rbac.md#scope-a-binding-to-a-pipelines-tenant) for how an
administrator writes those globs.

Select a job's name to open its pipeline page.

## Open a build

The pipeline page carries a breadcrumb trail under the job name. It names the team, then each
folder, then the job. Every crumb except the last is a link. Select a folder crumb to open the
team page with that folder, and every folder above it, already unfolded.

The **Defined by** card states where the job's script comes from. A job that reads a Jenkinsfile
from source control shows the repository and the last built commit. A job whose bundle item
carries the script inline shows a **Script** row that says so. The card also names the catalog
template, when the job has one, and the version profile of the controller.

The page lists the job's builds, with filters for branch, result, actor, and cause. Each filter
stays in the page URL, so a filtered view is shareable and survives a reload.

Select a build number to open its build page, which shows the status, the stage graph, and four
tabs: **Log**, **Artifacts**, **Tests**, and **Changes**.

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
permalink: append `#L<line number>` to the build page URL to open it scrolled to that line,
highlighted.

The **Log** tab shows a banner naming the reason when the run's log did not close complete, for
example after a lost chunk. A log with that banner is missing bytes; do not treat it as the full
record.

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
on its local disk; see [Build logs and artifacts](../operations/build-logs-and-artifacts.md)
for the storage modes that make an artifact downloadable.

The tab omits the files Varroa archives for its own use under `.varroa/`. Download the test
report from the **Tests** tab.

## Run a job with parameters

On the pipeline page, select **Run**. This control is present only for a caller who holds the
tenant's `pipelines` write grant.

The drawer renders the job's declared parameters: a text field, a checkbox, a choice selection,
a text area, or a password field. Fill them in and submit. A repeated submit of the same drawer
starts one build, not two.

Submitting navigates to the new build's page, which shows `Requested` until the build starts. A
build that has not started within one minute shows `Not started`; the page keeps polling, so a
late-starting build still resolves.

To start a new run of a build you are viewing, use **Re-run**. It is disabled once a retry of
that build exists, because Varroa allows one retry per build.

## Concurrent runs of the same job

A job can start a new build before an older one finishes. The
`Team.spec.pipelines.concurrency` setting controls this: `allow` (the default) runs both, and
`cancelInProgress` cancels the older run fleet-wide once the new run claims a
build number.

The setting is per tenant, and the group it cancels within is the job path itself, not the
build. A multibranch child job's path includes its branch, so a new build on one branch never
cancels another branch's build.

## Next steps

[Operate a run](run-operations.md) covers fleet locks, the failure digest, the summary line, and
calls to a job on another tenant.
