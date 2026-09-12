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
resources also replace omitted fields. `update_controller` is a sparse patch
with separate ownership rules. Read [Writing through MCP](writing.md) before
using it.

`call_jenkins_tool` is open-world because the target Jenkins plugin defines the
remote tool's effect. Review its discovered schema for every controller.

## Read results

Collection tools return an object:

```json
{"items": [], "count": 0}
```

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

The MCP server also exposes four pipeline tools: `run_pipeline`, `list_runs`,
`get_run`, and `get_run_log`. They operate on the build record store. The
`run_pipeline` tool starts a build and returns a `runId` and an `intentKey`.
The `list_runs` tool takes `limit` with the same default of 50 and the same
maximum of 200 as `list_activity`. The `get_run` tool returns one run's
detail. The `get_run_log` tool returns a bounded tail of the run's console
log, never a stream; `tailLines` defaults to 100 and clamps between 1 and
500. An agent that needs live output calls `get_run_log` again.

Each tool applies the same authorization as its REST counterpart. Triggering
a run checks the write grant on the resolved team and job. Listing runs
checks the read grant on the same pair. Reading a run or its log checks the
read grant on the run's own tenant and job path. A caller who may not read a
run gets a not-found error, not a forbidden error.

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
