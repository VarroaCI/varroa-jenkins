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

## Read the pipelines table

The team page shows two cards.

**Running now** holds one tile per build in progress: the job name, the build number, the
current stage, the branch, the commit, and a progress bar against the job's usual duration.

**Pipelines** holds one row per job: its health, its last build, its branch, its last eight
build durations, and when it last ran. Health is a weather glyph: clear when the last five runs
passed, cloudy when one or two failed, and rain when three or more failed. A job that has never
run shows no glyph and an empty last build.

Select a job's name to open its pipeline page.

## Open a build

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
