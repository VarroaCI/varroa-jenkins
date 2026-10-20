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
| Target URL | `https://<host>/hibernation/<token>/teams/<team>/queue/gitea-webhook/post` |
| Content type | `application/json` |
| Secret | The value of `status.pipelines.webhookSecret` |
| Events | Push and pull request |

The `<token>` and the `<team>` come from the tenant. The webhook must reach the dashboard
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
repository. The new status keeps the context and the link of the old status. Its description reads
`Varroa ended the build: <reason>`.

The operator changes only a status that matches the build by its job path and build number. A
status that has an end state is never changed. A retry of the build has its own build number, so
the retry keeps its own pending status.

These builds keep their `pending` status:

- A provisional end: `heartbeat-lost` and `start-lost`. The build can still finish. The build's own
  completion then writes its status.
- A build with no recorded commit or no recorded repository.
- A tenant whose only credential is a `githubApp` credential. The operator does not end a status
  with a GitHub App credential.

For a pull-request build, the recorder keeps the head commit of the pull request. The forge reports
the status on that commit. The start event carries that head commit too, so a build that stops before
its checkout still names it.

A failed write leaves the status `pending`. The operator logs the failure and does not retry.

The operator runs at most four of these writes at once on each replica. When all four are busy, the
operator drops the write and logs it at `Info`. The status then stays `pending`.

The write can race the retry. If the retry's own `pending` status lands first, the `error` write can
land after it. The context then shows `error` until the retry build completes. The retry build then
writes its own end state, which replaces the `error`.

## Status links

The status link of a build points to the Varroa build page. A plain job without a `targetUrl`
gets this link. The link has this form:

```text
https://<dashboard>/headless/<cluster>/<namespace>/<cell>/job/<job path>/<n>/display/redirect
```

The door opens `/pipelines/<team>/<job>/<runId>`. A multibranch build link has the same form,
for example `…/job/<team>/job/<repo>/job/PR-<n>/<b>/display/redirect`. See
[Headless controllers](../security/headless-controllers.md) for the door.
