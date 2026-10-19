# Propose a first pipeline from a repository

The assistant reads a repository. It proposes a first Jenkinsfile in a pull request. You
review the file, approve the proposal, and merge the pull request on the forge.

## Before you start

- The assistant is on with a provider, and your switch is on.
- The team is placed on a cell and has no job yet.
- You hold the tenant write grant for the team.

## Add a forge credential

Create a Secret in the team's agent namespace, `spec.pipelines.agentNamespace`. Set the label
`varroa.dev/tenant-credential: "true"`. Set the annotations `varroa.dev/credential-id`,
`varroa.dev/credential-kind` (`usernamePassword` or `secretText`), and
`varroa.dev/allowed-hosts`, for example `git.example.org`. The token needs write access.
If the cell has a `gitEgress` policy, list the forge address there.

## Propose and review

1. On the team home, select **Propose a pipeline from a repo**.
2. Enter the repository URL, and select
   **Propose**.
3. Read each file in full. Then select **Approve** or **Deny**.

## Merge and add the job

Merge the pull request on the forge. See
[GitHub sources](../config/items.md#github-sources) or
[Gitea and Forgejo sources](../config/items.md#gitea-and-forgejo-sources).

## Troubleshoot

| Code | Status | Fix |
|---|---|---|
| `assistant_disabled` | 503 | Turn on the assistant. |
| `assistant_off` | 409 | Turn on your assistant switch. |
| `repo_url_invalid` | 400 | Use an `https` URL on port 443. |
| `data_sharing_too_low` | 409 | Raise the data level. |
| `no_matching_credential` | 422 | Add a credential for the host. |
| `forge_unsupported` | 422 | Use a supported forge. |
| `forge_auth_failed` | 422 | Replace the token. |
| `forge_write_denied` | 422 | Use a token with write access. |
| `forge_unreachable` | 502 | Add the forge to `gitEgress`. |
| `jenkinsfile_exists` | 409 | Add the repository as a job. |
| `proposal_branch_exists` | 409 | Delete the branch or merge its pull request. |
| `assistant_busy` | 429 | Wait for a session to end. |
| `kill_switch` | 503 | Wait for the assistant to resume. |
| `invalid_request` | 400 | Send a body with a `repo` field only. |
| `internal` | 500 | Retry. Report the code if it repeats. |
| `not_found` | 404 | Check the repository URL and the team. |
| (no code) | 403 or 404 | Check the team name and your write grant. |

