# Understand MCP tool behavior

Inspect each tool's schema and annotations before calling it. Tool discovery is
the authoritative reference for available names and arguments.

## Interpret annotations

| Annotation | Meaning |
|---|---|
| `readOnlyHint` | The tool does not change state |
| `destructiveHint` | The tool may create, replace, or delete state |
| `idempotentHint` | Repeating the same call does not add another effect |
| `openWorldHint` | The effect depends on a system outside Varroa's resource model |

Varroa create and update tools are declarative. The create tools for controllers, groups, roles,
role bindings, Jenkins roles, Jenkins role bindings, catalog sources, composed bundles, and users refuse a name that
already exists. An identical create for an existing controller returns that controller. Use the update tool to change an existing object. Updates for most
resources replace omitted fields. Two tools are sparse instead.
`update_controller` is a sparse patch with separate ownership rules. Read
[Writing through MCP](writing.md) before using it. `update_cell_pool` writes
only the dials the call names, which is why it alone among the write tools
reports `destructiveHint: false`.

`call_jenkins_tool` is open-world because the target Jenkins plugin defines the
remote tool's effect. Review its discovered schema for every controller.

## Read results

Collection tools return an object:

```json
{"items": [], "count": 0}
```

`list_controllers` and `list_cell_pools` read every cluster of the brood. Their
result also carries `clusters`, one read status for each cluster: `name`, `ok`,
and `error`. A cluster with `ok: false` did not answer, and its rows are absent.
When `cluster` names one member that does not answer, the result has no items
and one `ok: false` row. When a brood is configured, an unknown `cluster` gives a tool error.

Single-resource tools return the resource directly. List tools usually return
compact summaries. Pass `verbose: true` where supported, or call the matching
`get_*` tool for full detail.

`list_activity` returns complete events and accepts `limit`. The default is 50
and the maximum is 200. It returns an empty list when the record store is not
wired.

`search` returns a kind-tagged envelope across seven result kinds:
controller, namespace, group, catalogitem, job, run, and activity. Each kind
is capped at five results, and unused kind slots do not go to another kind.
Authorization runs before a result enters the list, so a hidden controller,
job, or run never appears. The last three kinds need the record store; when
it is not wired, `search` still returns the first four.
The result also carries `clusters`, one read status for each cluster with `name`, `ok`, and `error`.
A cluster with `ok: false` returned incomplete results.

The MCP server also exposes fifteen pipeline tools: `run_pipeline`, `list_runs`,
`get_run`, `get_run_log`, `list_job_branches`, `get_job_health`, `list_fleet_locks`, `release_fleet_lock`,
`proceed_run_input`, `abort_run_input`, `replay_run`, `restart_run`, `scan_multibranch`, `get_tenant_cell`, and `lint_jenkinsfile`. They operate on the
build record store. The `lint_jenkinsfile` tool takes `tenant` and `jenkinsfile` and lints the Jenkinsfile on the tenant's cell. It returns `valid` and the `errors` list, and it writes nothing. The propose-pipeline session calls it before it opens a pull request. It contacts the tenant's cell and does not read the build record store. The `scan_multibranch` tool starts a scan of a multibranch job or an organization folder and returns
a `scanId` and the state `requested`. A job that is neither answers an error. Read the result with `list_job_branches`, in `lastScan`. The `run_pipeline` tool starts a build and returns a
`runId`, an `intentKey`, and the state `requested`. A refused trigger ends the run with status `COMPLETED`
and result `TRIGGER_FAILED`; `get_run` shows the `triggerReasonCode` and `triggerError`.
The `list_runs` tool takes `limit` with the same default of 50 and the same
maximum of 200 as `list_activity`. The `get_run` tool returns one run's
detail, with the run's `upstream` link and the first page of its `downstream` links. It also carries
`quality`, the run's coverage and warnings summaries. The `get_run_log` tool returns a bounded tail of the run's console
log, never a stream; `tailLines` defaults to 100 and clamps between 1 and
500. An agent that needs live output calls `get_run_log` again. The
`list_fleet_locks` tool takes `tenant` and answers the locks that tenant's
tenants own, hold, or wait on, with the same rows
`GET /pipelines/{tenant}/locks` answers.

The `proceed_run_input` tool answers a paused input step of a run. It takes
`run`, `input`, and an optional `parameters` object of name to string value.
The `abort_run_input` tool rejects that step, and takes `run` and `input`. An
agent reads the run's `inputs` array with `get_run` first. Both tools answer
that the request was sent, so the agent reads the run again for the outcome.

The `get_job_health` tool takes `tenant` and `job`. It returns the job's `health`, which is the
same object the job page shows, and its `trend`, the job's finished runs newest first. Health counts
the last ten finished runs: SUCCESS passes, and FAILURE, UNSTABLE, and ABORTED fail. INTERRUPTED and
NOT_BUILT do not count. The `trend` carries each run's duration and suite counters when it recorded
them. The tool gives the same answer as the job detail. Its `qualityTrend` lists one coverage or
warnings series per report, over the same runs as `trend`.

The `get_tenant_cell` tool takes `tenant` and returns the cell that hosts the tenant's tenant. It carries the Tenant's tenant warning when one holds. It gives the same answer as `GET /api/v1/pipelines/{tenant}/cell`.

The server also exposes one cell read tool, `get_cell_usage`. It takes
`namespace`, `name`, and an optional `cluster` that defaults to the local
cluster. It returns the same value the REST path
`GET /api/v1/cells/{namespace}/{name}/usage` returns: each tenant's four
load counts, the totals, the tenant cap, the free slots, and the Jenkins
queue length. The tool checks the `controllers:read` grant on the cell. A
name that is not a cell, and a cell the caller may not read, both give a
not-found error. Without the record store the tool gives an error that names
it. See [Cell usage](../operations/cell-usage.md) for the count rules.

The server exposes three cell-pool tools. A
[cell pool](../operations/cell-pools.md) sizes the cells of one version
profile.

| Tool | Arguments | Verb |
|---|---|---|
| `list_cell_pools` | `cluster`, `verbose` (both optional) | `cellpools:read` |
| `get_cell_pool` | `name`, `cluster` (optional) | `cellpools:read` |
| `update_cell_pool` | `name`, `cluster` (optional), and at least one of `warm`, `maxCells`, `maxTenantsPerCell`, `unreadyGracePeriod` | `cellpools:update` |

Each cluster of the brood holds its own pools. `list_cell_pools` lists every
cluster, or the one `cluster` names. It returns a compact summary of each pool,
with the local cluster first, then by cluster and name. Each summary names its
`cluster` and carries every spec dial and every status count. The result also
carries `clusters`, one read status for each cluster. A cluster with
`ok: false` did not answer, and its pools are absent. Pass `verbose: true`
for the full resources, which add the cell template and the condition list.
`get_cell_pool` returns one pool's full resource with a `cluster` key. Without
`cluster`, `get_cell_pool` and `update_cell_pool` act on the core cluster. An
unknown cluster, or a member that does not answer, gives a tool error.

`update_cell_pool` scales a pool. It writes only the dials the call names and
removes nothing, so it is annotated `destructiveHint: false` and
`idempotentHint: true`. `warm`, `maxCells`, and `maxTenantsPerCell` are whole
numbers. A fractional value and a value outside the 32-bit signed range are
both refused before the write. `unreadyGracePeriod` is a duration such as
`5m`. The tool refuses a call that names no dial.

`profileRef` and `cellTemplate` are not tool arguments. Edit those with
`kubectl` or through GitOps. There is no tool to create a pool and none to
delete one.

`CellPool` is cluster-scoped, so both verbs read the caller's cluster-wide
bindings only. A namespace-scoped binding satisfies neither. The built-in
`varroa:operator` role holds both verbs, and `varroa:admin` holds them through
its wildcard. `varroa:developer` and `varroa:viewer` hold neither, because a
pool's status lists every tenant on it.

Each tool applies the same authorization as its REST counterpart. Triggering
a run checks the write grant on the resolved tenant and job. Listing runs
checks the read grant on the same pair. Reading a run or its log checks the
read grant on the run's own tenant and job path. Answering a run input checks
the write grant on the run's own tenant and job path. A caller who may not
read a run gets a not-found error, not a forbidden error.

### MCP Apps views

Four read tools also advertise a view for a host that supports MCP Apps. The host renders a read-only card from the result. The text result does not change. The result `_meta` carries an "Open in dashboard" link, and it is absent when the server has no dashboard address. The run card shows one quality
line when the run carries `quality`.

| Tool | Card | Link opens |
|---|---|---|
| `get_run` | Run summary | Build page |
| `get_cell_usage` | Cell load | Controller page, Cell tab |
| `get_controller` | Controller status | Controller page |
| `list_fleet_locks` | Fleet locks | Tenant home |

## Open a pull request

`open_pull_request` creates one pull request from a new branch. It
always needs approval.

| Argument | Rule |
|---|---|
| `tenant` | A pipelines tenant that the caller may write. |
| `repo` | An `https` repository URL. In a session, the session repository. |
| `branch` | A new branch name, at most 200 characters. |
| `title` | One line, 1 to 200 characters. Also the commit message. |
| `body` | At most 8192 bytes. The BFF appends a line that names the actor. |
| `files` | 1 to 10 files with `path` and `content`. At most 64 KiB each, 256 KiB in total. |

The operator tries each tenant credential that may push. A forge 403 deletes the new branch. When the delete succeeds, the operator moves to the next credential. When the delete fails, the call stops and names the branch for manual removal. When every credential answers 403, the call answers `forge_write_denied` and names each credential. A forge rate limit is not a 403. The call deletes the branch, stops, and answers `forge_unreachable`, with the retry time when the forge states one.

A session call adds a `Varroa-Session` trailer to the commit. A failed call starts its error text with a code. On `proposal_base_moved`, start over. On `request_expired`, call again. No pull request opened. On `pull_request_busy`, another pull request for the Tenant runs. Call again after it ends. No pull request opened. On `operator_unavailable`, no operator received the call. No pull request opened. Call again later. On `operator_timeout`, the branch or the pull request can exist. Look in the repository before you call again. Any other relay error answers `forge_unreachable`. A success logs a
`pipeline.proposal.opened` event.

## Account for redaction

MCP and REST results omit credentials and noisy Kubernetes metadata. Removed
fields include:

- controller wake tokens
- password hashes and write-only user passwords
- `metadata.managedFields`
- `resourceVersion`, `uid`, and `generation`

Do not build workflows that expect these fields from MCP.

## Handle errors

| Error | Response |
|---|---|
| Authentication failure | Follow [the connection procedure](connecting.md) |
| Authorization failure | Check Varroa and Jenkins roles |
| Field conflict | Follow [field ownership](writing.md#manage-field-ownership) |
| Oversized list | Use filters or `get_*` |
| Jenkins tool failure | Re-discover the controller tools |

## Tenant agent tools

A Tenant agent calls four tools. A person can call them too, with the same grants
that the tool checks. See [Tenant agents](tenant-agents.md).

| Tool | Kind | Arguments | Effect |
|---|---|---|---|
| `read_repo_file` | read | `tenant`, `path`, optional `repo` | Reads a file from the instructions repository at the tracked revision. With `repo`, it reads that repository's default branch. A file is at most 256 KiB. A directory answers its entry names. |
| `post_note` | create | `run` or `tenant` with `job`, and `body` | Writes one note on a run or a job. A session writes one note only. |
| `open_issue` | create | `tenant`, `title`, `body`, `labels` | Opens one issue in the repository of `spec.outputs.issues`. Each label must be in `spec.outputs.issues.labels`. The operator chooses the credential. |
| `send_webhook` | action | `tenant`, `body` | Sends one signed message to the Tenant's completion webhook. |

An output tool answers `disabled` when the object does not enable its output.

`open_issue` answers `request_expired` when too little time is left. No issue opened. Call again.
It answers `operator_unavailable` when no operator receives the request. No issue opened. Call again. It answers `operator_timeout` when the operator does not answer in time. The issue can exist. Look in the repository before you call again. Any other relay error answers `forge_unreachable`.
On a member-placed Tenant, the instructions read and `open_issue` answer
`no_matching_credential` on the core cluster.

## Related pages

- [Connect an MCP client](connecting.md)
- [Writing through MCP](writing.md)
- [Jenkins controller tools](jenkins-tools.md)
