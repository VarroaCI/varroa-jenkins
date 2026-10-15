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

Varroa create and update tools are declarative. Many create tools apply a full
desired object, so an existing object may be replaced. Updates for most
resources also replace omitted fields. Two tools are sparse instead.
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

The MCP server also exposes twelve pipeline tools: `run_pipeline`, `list_runs`,
`get_run`, `get_run_log`, `list_job_branches`, `list_fleet_locks`, `release_fleet_lock`,
`proceed_run_input`, `abort_run_input`, `replay_run`, `restart_run`, and `scan_multibranch`. They operate on the
build record store. The `scan_multibranch` tool starts a scan of a multibranch job and returns
a `scanId` and the state `requested`. Read the result with `list_job_branches`, in `lastScan`. The `run_pipeline` tool starts a build and returns a
`runId`, an `intentKey`, and the state `requested`. A refused trigger ends the run with status `COMPLETED`
and result `TRIGGER_FAILED`; `get_run` shows the `triggerReasonCode` and `triggerError`.
The `list_runs` tool takes `limit` with the same default of 50 and the same
maximum of 200 as `list_activity`. The `get_run` tool returns one run's
detail, with the run's `upstream` link and the first page of its `downstream` links. The `get_run_log` tool returns a bounded tail of the run's console
log, never a stream; `tailLines` defaults to 100 and clamps between 1 and
500. An agent that needs live output calls `get_run_log` again. The
`list_fleet_locks` tool takes `team` and answers the locks that team's
tenants own, hold, or wait on, with the same rows
`GET /pipelines/{team}/locks` answers.

The `proceed_run_input` tool answers a paused input step of a run. It takes
`run`, `input`, and an optional `parameters` object of name to string value.
The `abort_run_input` tool rejects that step, and takes `run` and `input`. An
agent reads the run's `inputs` array with `get_run` first. Both tools answer
that the request was sent, so the agent reads the run again for the outcome.

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
a run checks the write grant on the resolved team and job. Listing runs
checks the read grant on the same pair. Reading a run or its log checks the
read grant on the run's own tenant and job path. Answering a run input checks
the write grant on the run's own tenant and job path. A caller who may not
read a run gets a not-found error, not a forbidden error.

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

## Related pages

- [Connect an MCP client](connecting.md)
- [Writing through MCP](writing.md)
- [Jenkins controller tools](jenkins-tools.md)
