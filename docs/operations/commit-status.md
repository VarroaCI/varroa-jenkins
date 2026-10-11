# Commit status

A Jenkins job reports its commit status to the forge. Varroa controllers support two methods.

## Multibranch and organization-folder jobs

A multibranch job or an organization-folder job on a cell reports a status by itself. The
`github-branch-source` plugin sends the status for each build. You do not add a step.

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

## Network

The cell must reach the forge API on port 443. Allow the egress in
`networkPolicy.cellIsolation.extraEgress`.

## Not supported

Varroa does not report commit status to GitLab or Bitbucket.
