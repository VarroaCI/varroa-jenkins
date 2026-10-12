# Commit status

A Jenkins job reports its commit status to the forge. Varroa controllers support two methods.

## Multibranch and organization-folder jobs

A multibranch job or an organization-folder job on a cell reports a status by itself. The
`github-branch-source` plugin sends the status for a GitHub source. The `gitea` plugin sends
the status for a Gitea or Forgejo source. You do not add a step.

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

## Forgejo and Gitea

Forgejo and Gitea sources use the `gitea` plugin. Declare each forge server in the cell
bundle. The plugin reads the server list from the `unclassified.giteaServers` section of
the Jenkins configuration:

```yaml
unclassified:
  giteaServers:
    servers:
      - displayName: forge
        serverUrl: https://git.example.org
        manageHooks: false
        aliasUrl: ""
```

Set `manageHooks: false`. Varroa registers the webhook, so the plugin must not register one.

Create a username and password credential in the job's folder. Put an access token in the
password field. The token needs the `write:repository` and `read:organization` scopes. The
notifier uses the folder credential on the source.

Add a webhook to the repository. Use these values:

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

## Status links

The status link of a build points to the Varroa build page. A plain job without a `targetUrl`
gets this link. The link has this form:

```text
https://<dashboard>/headless/<cluster>/<namespace>/<cell>/job/<job path>/<n>/display/redirect
```

The door opens `/pipelines/<team>/<job>/<runId>`. A multibranch build link has the same form,
for example `…/job/<team>/job/<repo>/job/PR-<n>/<b>/display/redirect`. See
[Headless controllers](../security/headless-controllers.md) for the door.
