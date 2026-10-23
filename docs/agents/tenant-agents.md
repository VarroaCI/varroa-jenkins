# Tenant agents

A Tenant agent is an agent that a pipelines Tenant runs. It starts when a
build fails, when a schedule misses a run, or when a person asks for it. It
reads the Tenant's instructions, reads the repository, and can post a note, open
an issue, or send the Tenant's webhook.

A Tenant agent acts as its own principal. Its subject starts with `agent:`. It
does not act as a person, and it does not use an administrator token. Every
tool call is checked against the grants of its principal and of its Tenant.

## Fields

A Tenant agent is a `TenantAgent` object in a namespace that the Tenant owns or in the
release namespace. The acceptance rule selects one object for each Tenant. A
second object for the same Tenant answers `Duplicate` and does not run.

| Field | Meaning |
|---|---|
| `spec.tenantRef.name` | The pipelines Tenant that the agent serves. |
| `spec.enabled` | `false` stops new sessions. The object stays accepted. |
| `spec.provider`, `spec.model` | The model provider and model. The chart defines the providers. |
| `spec.instructions` | The `CatalogSource` that holds the instructions file, and its path. |
| `spec.triggers` | The events that start a session. See the trigger table. |
| `spec.unattendedWrites` | Write tools that may run without an approval, after a grant. |
| `spec.outputs` | Where the agent may write: notes, an issue repository, and the webhook. |
| `spec.budgets` | The token limits of one session and of one UTC day. |
| `spec.dataSharing` | The most data the provider may receive: `none`, `metadata`, or `logs`. |

## Example

```yaml
apiVersion: varroa.dev/v1alpha1
kind: TenantAgent
metadata:
  name: payments-triage
  namespace: varroa-system
spec:
  tenantRef:
    name: payments
  enabled: true
  provider: smoke
  model: m1
  instructions:
    source: payments-repo
    path: .varroa/tenant-agent.md
  triggers:
    - type: run.failed
      debounce: 10m
    - type: manual
  unattendedWrites:
    - restart_run
  outputs:
    notes: true
    issues:
      repo: https://git.example.org/payments/platform
      forge: gitea
      labels: [ci]
    webhook: false
  budgets:
    session: 50000
    day: 200000
  dataSharing: logs
```

A Tenant agent reads the [operator guide](guide.md) in its system prompt and the Tenant instructions file in its first turn. The system prompt names the Tenant agent and its Tenant.

## The instructions file

The instructions file is a text file in the repository of the `CatalogSource`.
The default path is `.varroa/tenant-agent.md`. The file must follow these rules:

- The file is at most 32 KiB.
- The file is valid UTF-8 text with no NUL byte.
- The source repository uses its default branch. A pinned revision is refused.
- The source has been synced once. Until then the object reports `NotSynced`.

The agent reads the file at the revision that the source reports. Every read in
one session uses the same revision. A change to the file takes effect at the
next session.

The runtime does not read keys, links, or includes from the file.

## Forge credential

The instructions read, the repository reads, and `open_issue` use a Tenant
credential. The operator selects the credential from the Secrets in the
namespace of `spec.pipelines.agentNamespace`. A Secret is a candidate only
when it has all of these:

- The label `varroa.dev/tenant-credential: "true"`.
- The annotation `varroa.dev/credential-id`.
- The annotation `varroa.dev/credential-kind`. Use `usernamePassword` with the
  token in the key `password`, or `secretText` with the token in the key `secret`.
- The annotation `varroa.dev/allowed-hosts`. Its value is a comma-separated
  list of host names, for example `git.example.org`. The list must include the
  host of the repository.

A Secret without the `allowed-hosts` annotation never matches. When no
credential matches, the condition `InstructionsResolved` reports `FetchFailed`.

The token needs these permissions on Gitea or Forgejo:

| Use | Token scope | Repository access |
|---|---|---|
| Instructions and repository reads | `read:repository` | Read |
| `open_issue` | `read:repository` and `write:issue` | Read |

`open_issue` needs no push access. The operator tries each credential that
reads the repository. It tries the credentials that may push first. A forge 403
moves to the next credential. When every credential answers 403, `open_issue`
answers `forge_write_denied`. A forge rate limit stops the attempt and answers
`forge_unreachable`, with the retry time when the forge states one. On GitHub, give the token read access to contents
and write access to issues.

`open_issue` answers `request_expired` when too little time is left. No issue opened. Call again.
It answers `operator_unavailable` when no operator receives the request. No issue opened. Call again. It answers `operator_timeout` when the operator does not answer in time. The issue can exist. Look in the repository before you call again. Any other relay error answers `forge_unreachable`.

The instructions source must follow the default branch. Set no
`spec.revision` on the `CatalogSource`. A source that pins `revision: main`
reports `NotDefaultBranch`. Create a second `CatalogSource` for the agent when
the Tenant source pins a revision. Create it in the namespace of the `TenantAgent`:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CatalogSource
metadata:
  name: payments-agent
  namespace: varroa-system
spec:
  repoURL: https://git.example.org/payments/platform
```

## Triggers

Each trigger starts a session, and each session records the activity that
started it.

| Trigger | Activity type | Produced by |
|---|---|---|
| `run.failed` | `build.failed` | The recorder, at the owner's completion event. |
| `run.interrupted` | `build.interrupted` | The recorder, at the synthetic end of a run. |
| `cron.missed` | `build.scheduled.backstop` | The recorder, when it fires a missed cron run. |
| `signature.refused` | `cell.signature_refused` | The operator, for a refused script signature. |
| `scan.failed` | `pipeline.branch_scan_failed` | The recorder, for a failed multibranch scan. |
| `lock.held` | `fleetlock.wait_exceeded` | The recorder, after a fleet lock wait of 15 minutes. |
| `placement.moved` | `tenant.placement_moved` | The operator, when the Tenant moves to another cell. |
| `manual` | `tenantagent.run_requested` | The BFF, on a request from a person. |

A trigger is best effort. A producer can miss an event during an outage. The
runner ignores an event older than one hour.

## Outputs

An output is a write that the agent may make without a person.

- `notes: true` lets the agent write one note for each session, on a run or a
  job. A second note in the same session returns the first one.
- `issues` lets the agent open issues in one repository. Each label must be in
  the list `spec.outputs.issues.labels`. A label that the repository does not have
  refuses the call.
- `webhook: true` lets the agent send one signed message to the Tenant's
  completion webhook, with the event `tenant_agent.message`.

An enabled output adds its tool to the unattended group of the capabilities.
The tool still needs a grant before it runs without an approval. See
[Safety](safety.md).

## Allowlist and grant

`spec.unattendedWrites` is the allowlist. It names the write tools that the
agent may run without an approval. Only the tools in
`run_pipeline`, `restart_run`, and `scan_multibranch` can be granted.

A person with the `tenantagents:unattended` permission grants the allowlist on the
`/agents` page. The grant copies the current list and pins it to the object
UID. A later spec edit does not extend the grant. A grant of an empty list
answers `nothing_to_grant`.

## Budgets

Two limits apply to every session:

- The session limit is the lower of the chart value `assistant.budgets.session`
  and `spec.budgets.session`.
- The day limit is the lower of the chart value `assistant.budgets.tenantDay` and
  `spec.budgets.day`. The day total sums every Tenant agent session of the Tenant
  for the current UTC day.

When the day total reaches the day limit, new sessions end as `skipped` with the
reason `budget_day`. The step limit is the chart value `assistant.budgets.steps`.

## Pages

- `/pipelines/<tenant>/agent` shows the Tenant agent of the Tenant. Its tabs are
  Sessions, Approvals, Capabilities, and Settings. A session opens the full
  transcript.
- The Tenant home shows a Tenant agent card. The card is hidden when the Tenant has no
  accepted agent.
- `/agents` lists every Tenant agent of the brood. It needs the `agentsessions:read`
  permission. The grant toggle needs `tenantagents:unattended`.

A tenant reader sees the sessions and the capabilities. A tenant writer also
decides approvals and cancels sessions. The page never shows a forge token or
a credential.

A tenant reader sees each tool result of a session. An admin who is not a tenant
reader sees the tool calls, but not their results. The API marks each such
result as withheld.

## Member clusters

A Tenant can run on a member cluster. The runner runs in the core release. A manual
run request names the placement cluster of the Tenant, so the activity event
carries that cluster.

On the core cluster, the instructions read and the `open_issue` tool answer
`no_matching_credential` for a member-placed Tenant. The core operator holds no
Tenant object for that Tenant, so it cannot choose a credential. Sessions still start.

## Troubleshooting by condition

Read the conditions in the Settings tab or on `/agents`.

| Condition | Reason | Action |
|---|---|---|
| `InstructionsResolved` | `NotConfigured` | Set `spec.instructions`. |
| `InstructionsResolved` | `NotSynced` | Wait for the catalog source to sync. |
| `InstructionsResolved` | `NotDefaultBranch` | Remove the pinned revision from the source. |
| `InstructionsResolved` | `NotFound` | Add the file at the path, on the default branch. |
| `InstructionsResolved` | `TooLarge`, `NotText` | Shorten the file, and keep it UTF-8 text. |
| `InstructionsResolved` | `HostNotAllowed` | Use an `https` repository URL on port 443. |
| `InstructionsResolved` | `FetchFailed` | If no credential matches, add one that lists the host. See [Forge credential](#forge-credential). Otherwise check the operator logs. The ticker retries after ten minutes. |
| `ProviderReady` | `UnknownProvider`, `UnknownModel` | Use a provider and model from the chart. |
| `ProviderReady` | `KeyMissing` | Add the provider key to the chart secret. |
| `ProviderReady` | `AssistantDisabled` | Turn on the assistant in the chart. |
| `UnattendedAllowed` | `NotGranted` | Grant the allowlist on `/agents`. |
| `Paused` | `Disabled` | Set `spec.enabled` to `true`. |
| `Paused` | `KillSwitch` | An administrator turns the kill switch off. |
| `Paused` | `BudgetDay` | Wait for the next UTC day, or raise the day limit. |
| `Paused` | `TenantNotFound` | Create the Tenant, or fix `spec.tenantRef.name`. |
| `Paused` | `NamespaceNotOwned` | Move the object to a namespace that the Tenant owns. |
| `Paused` | `Duplicate` | Delete the other object for the Tenant. |

## Related pages

- [Safety](safety.md)
- [Tool reference](tools.md)
- [Assistant](assistant.md)
