# Read and Run Pipelines

Open a team's pipelines, read a build's log, download an artifact, and start a build with
parameters.

## Prerequisite

The pipelines pages need the [build record store](../operations/build-records.md). Set
`recordStore.enabled: true` in your Helm values before you start. With the store off, the
**Pipelines** row still shows, and every page says so instead of listing builds.

## Open a team

Sign in and select **Pipelines** in the sidebar. You see the pipelines of every [Team](../security/varroa-rbac.md#grant-pipelines-access) that grants you a `pipelines`
verb. `/pipelines` lists only the teams that hold pipelines you can read, including member-cluster
teams. A team with no pipelines and no controllers in its namespaces does not show. It opens your
only team automatically. With more than one, pick one.

A team on a member cluster shows its definition, parameters, declared jobs, source input, and version profile. Varroa reads them from the member at each request.

## Your cell card

A team on a [cell](../operations/cells.md) shows a placement pill under the page title. The pill
names the state and the cell. The **Your cell** card sits behind the pill and starts closed.

Select the pill to open the card. The card names the cell, its pool, its version profile, and its
Jenkins version. It also reports the free tenant slots on the cell, the length of the cell's build
queue, and your own running builds.

One line counts the other teams on the cell without naming them, and shows none of their jobs,
builds, or counts.

**Cell details** opens the cell's controller page, if your access covers it. Otherwise you still
read every field.

The card reports one of five states.

| State | What the card shows |
|---|---|
| Placed | the whole card |
| Moving | the whole card, plus a note that Varroa is moving the team |
| Rehydrating | the whole card, plus a note that the cell is rebuilding the team's folders |
| Not placed | the badge and the reason only. The team waits for its first cell |
| No card | the team runs on its own controller, so no cell hosts it |

Callers who can read a team with no pipelines see No pipelines. Other callers see Not found.

**Reason** carries the operator's text, such as `CellNotReady`.

A cell in another cluster reports no queue length. The **Queue** figure then reads `not reported`,
which is not an empty queue.

**Free slots** counts the free tenant slots of this cell alone. The cell's pool can hold a lower
cap, which stops a new team from landing even while the cell reports a free slot.

## Read the pipelines tree

Four cards follow the page head: **Running now**, **Pipelines**, **Locks**, and **Runners**.

**Running now** holds one tile per build in progress: the job name, the build number, the
current stage, the branch, and a progress bar against the job's usual duration. The stage line
names the run's top-level stage in progress. A running build with no stage in progress reads
`Starting`, and a build that has not started reads `Queued`.

**Pipelines** holds the team's folder tree. A job row carries its health, its last build, its
branch, its last eight build durations, and when it last ran. **Queue wait** shows the job's
median wait from queue entry to start over the last 30 days. Health is a weather glyph: clear when the last five runs passed, cloudy when one or two failed, and rain when three
or more failed. A job that never ran shows no glyph and an empty last build.

A folder that its cell holds shows no declared job; run history shows. See [Cells and Pipelines Tenants](../operations/cells.md).

A job or multibranch row can show a **Pending** badge: the bundle declares the job, but no cell has created it. A cell creates it at its next apply. The job page shows the note; **Run** stays available. The badge appears only when every cell reports a full job list.

A folder row carries a fold control, its name, and two counts: the jobs below it, and how
many build now. A folder reports no health value.

A multibranch item is a folder row that counts branches. It offers no **Run** control, because
it does not build.

Each level lists folders, then jobs, alphabetically.

A first load unfolds the top two levels and folds deeper levels.

The fold state lives in the page URL, as one `open` value per unfolded folder:

```text
/pipelines/payments?open=release&open=release/eu
```

Copy that URL to share the view. A reload keeps the folds. `?open=` with no value folds
everything.

**Runners** lists the [external runners](../operations/runners.md) bound to the team. Each row
shows the launcher, the labels, the cell, the connection state, and the last contact time.

A caller whose grant carries `jobs` globs reads only the matching jobs and running builds. A
folder holding no readable job does not appear at all. See
[Varroa RBAC](../security/varroa-rbac.md#scope-a-binding-to-a-pipelines-tenant) for how an
administrator writes those globs.

## Open a build

Breadcrumbs name the team, folders, and job. A folder crumb opens the team page with that folder unfolded. On build and test pages, the job crumb links to the pipeline page.

A pull-request run shows a PR chip beside its branch and a **Pull request** row.

A queued build held by a cap shows a live Waiting notice.

The **Defined by** card states where the job's script comes from. A job that reads a Jenkinsfile
from source control shows the repository and the last built commit. A job with an inline script shows a **Script** row. The card names any catalog template and the version profile.

A shared library in the **Ran with** row links to its repository: the commit, or on GitHub and GitLab the version. An unknown host shows text.

The **Jenkinsfile** and **Source input** rows link GitHub, GitLab, Gitea, and Forgejo hosts when the forge is known. Other hosts show text.

The page lists the job's builds, filterable by branch, result, actor, and cause. Each filter
stays in the page URL, so a filtered view is shareable and survives a reload.

The commit opens on GitHub, GitLab, Gitea, or Forgejo. Other hosts get a copy button.
The branch opens the pipeline page filtered to that branch. The **Queue** column shows each build's wait.

The build page shows the parameters that the build ran with. A masked value appears as four dots.
A replayed build names the build that it copied, with a link. A restarted build names the build
and the stage that it restarted from. A queue wait longer than five seconds appears on the meta
line.
Select a build number to open its build page: the status, the stage graph, and four
tabs: **Log**, **Artifacts**, **Tests**, and **Changes**. The **Older** and **Newer** buttons
beside the build number open the adjacent builds. The buttons cover the newest 100 builds
of the job. The `tab` URL parameter keeps the active tab.

Each pipelines page sets the browser tab title.

An interrupted build shows a banner naming the reason, and a link to its retry when one exists.

A wide stage graph scrolls inside its card. A stage name too wide for its node is cut short in its
node; point at the node to read it in full. Nested stages sit under a heading that names their
parent. Select the heading to show or hide them.

## Read a log

Open the **Log** tab. The log viewer supports:

- A search box, bound to `/`.
- A **First error** jump to the first line matching `error`, `failed`, `failure`, or
  `exception`.
- A **Timestamps** toggle.
- A **Follow** toggle that keeps the view at the newest line. It turns off when you scroll up,
  select a stage, or jump to a line with a permalink, a search, or **First error**. It is off
  and disabled when the run has ended and its log is complete.
- A **Raw** link that opens the same log as plain text in a new tab.

Select a stage in the stage graph to scroll the log to that stage's header. A header sits before
the first line of its stage. A parallel branch that has no lines of its own opens at its first
nested stage. A `#stage-<id>` fragment in the URL does the same and also scrolls the page. Every line carries a
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
| `?` | Show or hide the shortcut list |

Select **Shortcuts** to show the same list.

## Download an artifact

Open the **Artifacts** tab. Each row shows the artifact's path and size, with a **Download**
control. The control is disabled, with an explanation, for an artifact Jenkins still stores only
on its local disk; see [Build logs and artifacts](../operations/build-logs-and-artifacts.md)
for the storage modes that make an artifact downloadable.

The tab omits the files Varroa archives for its own use under `.varroa/`. Download the test
report from the **Tests** tab.

The **Tests** tab shows the totals, the failed and skipped cases (at most 500), and a suite table.
The **Class** column shows only when a suite holds more than one class.

## Run a job with parameters

On the pipeline page, select **Run**. This control is present only for a caller who holds the
tenant's `pipelines` write grant.

The drawer renders the job's declared parameters: a text field, a checkbox, a choice selection,
a text area, or a password field. Fill them in and submit. A repeated submit of the same drawer
starts one build, not two.

You can start a pending job, but the run can fail until the cell creates the job.

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
calls to a job on another tenant. See [multibranch jobs](multibranch.md).
