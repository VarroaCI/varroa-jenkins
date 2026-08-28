# Call Jenkins controller tools

Varroa provides two MCP tools for managed Jenkins controllers.

| Tool | Task |
|---|---|
| `list_jenkins_controllers` | List visible controllers and their reachability |
| `call_jenkins_tool` | Forward JSON-RPC to one controller's MCP plugin |

The controller plugin defines its own tool names, arguments, and effects.
Varroa therefore marks `call_jenkins_tool` as open-world and potentially
destructive.

## Discover tools

Send `tools/list` to the target controller:

```json
{
  "namespace": "teams-platform",
  "name": "demo",
  "method": "tools/list"
}
```

## Call a tool

Place the controller tool name and its arguments inside JSON-RPC `params`:

```json
{
  "namespace": "teams-platform",
  "name": "demo",
  "method": "tools/call",
  "params": {
    "name": "build_job",
    "arguments": {"job": "smoke-test"}
  }
}
```

| Argument | Required | Value |
|---|---|---|
| `namespace` | Yes | Controller namespace |
| `name` | Yes | Controller name |
| `method` | Yes | JSON-RPC method such as `tools/list` or `tools/call` |
| `params` | No | JSON-RPC parameters |

## Resolve failures

| Failure | Check |
|---|---|
| Controller is absent | Varroa `controllers:read` scope |
| Controller is unreachable | Connection and BFF cluster locality |
| Jenkins returns `403` | The caller's Jenkins role and item scope |
| Tool is unknown | Run `tools/list` again |

The bridge reaches only controllers in the BFF's local cluster. It does not
proxy to member-cluster controllers.

See [Identity and auditing](identity.md) for the two authorization checks.
