# Commit status

A Jenkins job reports its commit status to the forge. Varroa controllers support two methods.

## Multibranch and organization-folder jobs

A multibranch job or an organization-folder job on a cell reports a status by itself. The
`github-branch-source` plugin sends the status for a GitHub source. The `gitea` plugin sends
the status for a Gitea or Forgejo source. You do not add a step.

To stop the status on a GitHub source of a `multibranch` item, declare the `githubSkipNotifications` trait. See [GitHub sources](../config/items.md#github-sources). An organization folder does not support the trait.

## GitHub Checks

A multibranch build on a cell publishes a GitHub check when its source credential is a
`githubApp` credential. The check is named `Jenkins`. The build also publishes a commit status.
To stop the commit status, declare the `githubSkipNotifications` trait on the source. The check still publishes.

The check links to the Varroa build page. The link is the same as the status link.

The GitHub App needs "Checks: write" on the repository. A classic or fine-grained personal
access token cannot publish a check. Store the App credential as a `githubApp` tenant
credential. See [Projected credentials](cells.md#what-composes-a-tenant).

### Re-run a check

A user selects **Re-run** on a check that Varroa reported. GitHub sends the re-run request only
to the GitHub App that created the check. Point that App at the tenant door:

- Webhook URL: `https://<host>/hibernation/<token>/tenants/<tenant>/queue/github-webhook/`.
  Read `<token>` from `status.pipelines.webhookToken`.
- Webhook secret: the value of `status.pipelines.webhookSecret`.
- Events: subscribe the App to **Check run** and **Check suite**.

One App serves one tenant. A second tenant needs its own App.

A re-run starts a build of the branch's current head. It does not rebuild the original commit.
A re-run keeps the fork trust decision of the branch source. **Re-run all checks** on a pull
request from a fork starts no build. The tenant rules are in
[Re-run from GitHub](webhooks.md#re-run-from-github).

A re-run builds even when an earlier run of the same revision is still live. The rule in
[A duplicate run](build-records.md#a-duplicate-run) does not mark a re-run.
[Two runs of one commit](#two-runs-of-one-commit) states which result the forge then shows.

## Plain jobs

A plain pipeline job has no SCM source that reports a status. Call the `githubNotify` step
in the job:

```groovy
githubNotify(
    credentialsId: '<folder credential>',
    account: '<owner>',
    repo: '<repository>',
    sha: '<commit sha>',
    context: 'ci/build',
    status: 'SUCCESS'
)
```

The credential must be a username and password credential in the job's folder. A GitHub App
credential also works.

## Credential scope

| Credential | Required scope |
|---|---|
| Classic personal access token | `repo:status`. Use `repo` for a private repository. |
| Fine-grained personal access token | "Commit statuses: write" on the repository. |
| GitHub App | "Commit statuses: write" on the repository. |
| GitHub App, for GitHub Checks | "Checks: write" on the repository. |

## Forgejo and Gitea

Forgejo and Gitea sources use the `gitea` plugin. The plugin needs a server entry for each
forge. Declare the entry in the way that fits the controller.

### Pool cell

Set `spec.giteaServers` on the `CellPool`. Each entry names one forge:

```bash
kubectl patch cellpool <pool> --type merge -p '{"spec":{"giteaServers":[{"serverUrl":"https://git.example.org"}]}}'
```

The operator renders each entry as `unclassified.giteaServers` in the cell configuration.
The operator sets `manageHooks: false` and writes no credential. The operator does not
register the webhook on the forge. You add it by hand. See
[Set up a Forgejo or Gitea webhook](webhooks.md#set-up-a-forgejo-or-gitea-webhook).

The controller reports the result in the `PoolGiteaServers` condition. The condition uses
these reasons:

- `Rendered`: every server rendered.
- `PluginMissing`: the pinned plugin set has no `gitea` plugin. No server renders.
- `ServerInvalid`: a server carries a `${` token. The operator skips that server.

The cell must reach the forge API on port 443. The operator does not add this egress. Allow
it in `networkPolicy.cellIsolation.extraEgress`.

### Classic controller

Declare each forge server in the controller's own bundle. The plugin reads the server list
from the `unclassified.giteaServers` section of the Jenkins configuration:

```yaml
unclassified:
  giteaServers:
    servers:
      - displayName: forge
        serverUrl: https://git.example.org
        manageHooks: false
        aliasUrl: ""
```

Set `manageHooks: false`. The operator does not register the webhook, so you add it by hand.

### Credentials and webhook

Create a username and password credential in the job's folder. Put an access token in the
password field. The token needs the `write:repository` and `read:organization` scopes. The
notifier uses the folder credential on the source.

Add a webhook to the repository. Use these values. For an organization folder, add the
hook to the organization instead. See [Tenant webhooks](webhooks.md).

| Field | Value |
|---|---|
| Target URL | `https://<host>/hibernation/<token>/tenants/<tenant>/queue/gitea-webhook/post` |
| Content type | `application/json` |
| Secret | The value of `status.pipelines.webhookSecret` |
| Events | Push and pull request |

The `<token>` and the `<tenant>` come from the tenant. The webhook must reach the dashboard
host on port 443.

## Network

The cell must reach the forge API on port 443. Allow the egress in
`networkPolicy.cellIsolation.extraEgress`.

## Not supported

Varroa does not report commit status to GitLab or Bitbucket.

## Interrupted builds

Jenkins sets a commit status when a build completes. The recorder can end a build that Jenkins
never completes. The status then stays `pending`. The operator fixes this in three steps:

1. The recorder ends the build with a synthetic end reason.
2. The operator reads the statuses of the build's commit.
3. The operator sets each `pending` status of that build to `error`.

The operator writes the status with the tenant credential. The credential must allow a push to the
repository. The operator tries each pushing credential in turn. A forge 403 moves to the next
credential. A forge rate limit stops the attempt. The operator logs it, and the status stays pending. The new status keeps the context and the link of the old status. Its description reads
`Varroa ended the build: <reason>`.

The operator changes only a status whose link is the build link of the build:

- The link starts with the Jenkins location URL of the build's cell. Each scheme, host, and path
  segment before `/job/` must match. A cell's location URL is its dashboard door.
- One `job/<name>` pair for each segment of the job path follows, then the build number.
- Nothing, a slash, or `display/redirect` ends the link.

A status of another Jenkins or another cell with the same job path and build number stays
`pending`. A status that has an end state is never changed. A retry of the build has its own build
number, so the retry keeps its own pending status.

These builds keep their `pending` status:

- A provisional end: `heartbeat-lost` and `start-lost`. The build can still finish. The build's own
  completion then writes its status.
- A build with no recorded commit or no recorded repository.
- A build whose cell has no location URL that the operator can name. A deleted cell has none. The
  operator logs the build at `Warn`.
- A build that a resume moved to another cell. Its status link names the first cell.
- A build that started before a change of the dashboard URL.
- A tenant whose only credential is a `githubApp` credential. The operator does not end a status
  with a GitHub App credential.

The forge reports the status on the pull-request revision of the run. See
[Pull-request revision](#pull-request-revision). The start event carries that revision too, so a build
that stops before its checkout still names it.

A failed write leaves the status `pending`. The operator logs the failure and does not retry.

An interrupted build never changes a status that links to the job, because an automatic retry owns that status.

The operator runs at most four of these writes at once on each replica. When all four are busy, the
operator drops the write and logs it at `Info`. The status then stays `pending`.

The write can race the retry. If the retry's own `pending` status lands first, the `error` write can
land after it. The context then shows `error` until the retry build completes. The retry build then
writes its own end state, which replaces the `error`.

## Dropped branch jobs

A scan can mark a branch job dead while the branch job builds. The default Gitea branch discovery
drops a branch that a pull request also files. The branch-source plugin then finds no source for
the job. It writes no status, so the context keeps its `pending` status.

At completion, the operator sets that `pending` status from the Jenkins result. The state is the
state that the branch-source plugin of the forge writes:

| Result | GitHub | Gitea and Forgejo |
|---|---|---|
| `SUCCESS` | `success` | `success` |
| `UNSTABLE` | `failure` | `failure` |
| `FAILURE` | `error` | `failure` |
| Any other result | `error` | `error` |

For `NOT_BUILT`, the `gitea` plugin writes `skipped` or `warning`. The operator writes `error`.

The description reads `Varroa reported the build result: <result>`. The operator also ends the
queued status that links to the job, `Build queued...`, because a dead branch job queues no other build.
That link must also start with the location URL of the build's cell.

The credential, link, rate-limit, and four-write rules of [Interrupted builds](#interrupted-builds) apply.

## Pull-request revision

A pull-request build records one revision. The rule has two parts:

- The revision action hash, when the branch source provides one. The GitHub, Gitea, Forgejo,
  and Bitbucket branch sources provide it. It is the head commit of the pull request.
- The pre-merge commit of the checkout, when the branch source provides no hash.

The recorder never records the local merge commit of the checkout, on any forge.

## Two runs of one commit

Two runs of one job can build one commit at the same time. A re-run, the **Run** button, a retry,
a replay, a restart, and a `varroa.build` call each start such a run. A pull request also builds
again when its target branch moves, and both builds share the head commit.

Both runs report the same status context on the commit. The plugin writes the context when each
run completes, so an older run that completes last writes its result over the newer one.

The newest run of the job and the commit owns the context. The newest run has the newest run id.
A run id is a UUIDv7, so the order is the order in which Varroa created the runs. A `DUPLICATE`
run, an `INTERRUPTED` run, and a failed trigger never own the context.

When the newest run has completed, the operator puts its result back:

1. The recorder sees a completion of a run of the job and the commit.
2. The operator waits 30 seconds, so the plugin's own write can land.
3. The operator reads the latest status of each context on the commit.
4. The operator writes the result of the newest run over each context that names an older run.

The operator writes only over a context that meets all of these rules:

- The context is a default context of the branch-source plugin. On GitHub, it is
  `continuous-integration/jenkins/branch`, `continuous-integration/jenkins/pr-merge`, or
  `continuous-integration/jenkins/pr-head`. On Gitea and Forgejo, it is the folder of the job,
  then `/pipeline/`, then `head`, `tag`, `release`, or `pr-<target branch>`.
- Its link is the build link of an older run of the same job.
- Its link starts with the Jenkins location URL of the cell that ran the newest build. Each scheme,
  host, and path segment before `/job/` must match. A cell's location URL is its dashboard door.

The operator never writes over a context that a pipeline step writes, such as `githubNotify`. It
never writes over a status of another Jenkins with the same job path and build number. It writes
only for a headless controller, such as a cell. After a cell move or a change of the dashboard
URL, the old link does not match, so the operator writes nothing. A status that a person sets by
hand with a default context name and the build link of an older run is written over too.

The new status links to the newest build. Its description reads
`Varroa kept the result of the newest build #<n>: <result>`. The state follows the table in
[Dropped branch jobs](#dropped-branch-jobs). The operator compares only the runs that were live
while the newest run built. While the newest run builds, the operator writes nothing, because
that run writes its own status last.

Until the operator writes, the context shows the older result. The credential, rate-limit, and
four-write rules of [Interrupted builds](#interrupted-builds) apply. A failed or dropped write
leaves the older result. An operator restart during the 30-second wait loses the write. A tenant
whose only credential is a `githubApp` credential keeps the older result.

To stop the older build of a pull request, set `Tenant.spec.pipelines.concurrency` to
`cancelInProgress`. The newer build then cancels the older build when it claims its build number.
Until the newer build completes, the context can show the cancelled build's state.

Varroa does not set the context name. The plugin sets it. The `github-branch-source` plugin uses
`continuous-integration/jenkins/pr-merge` by default. Read the context name on the forge before
you set a required status check.

## Status links

The status link of a build points to the Varroa build page. A plain job without a `targetUrl`
gets this link. The link has this form:

```text
https://<dashboard>/headless/<cluster>/<namespace>/<cell>/job/<job path>/<n>/display/redirect
```

The door opens `/pipelines/<tenant>/<job>/<runId>`. A multibranch build link has the same form,
for example `…/job/<tenant>/job/<repo>/job/PR-<n>/<b>/display/redirect`. See
[Headless controllers](../security/headless-controllers.md) for the door.
