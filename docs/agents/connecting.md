# Connect an MCP client

Varroa serves stateless Streamable HTTP MCP at
`https://<dashboard-host>/api/v1/mcp`. Authenticate with a Varroa API key that
belongs to the person or service account the client represents.

## Create a credential

Create a key as described in [API keys](../security/api-keys.md). The key uses
its owner's current Varroa and Jenkins permissions. Do not create it from an
administrator account unless the client requires administrator access.

## Use the stdio bridge

`varroactl mcp` reads MCP messages on standard input and forwards them with the
credential from the active context. This keeps the key out of the MCP client
configuration.

```bash
claude mcp add varroa -- varroactl mcp
```

Select a named context when needed:

```bash
claude mcp add varroa -- varroactl mcp --context production
```

Equivalent JSON configuration:

```json
{
  "mcpServers": {
    "varroa": {
      "command": "varroactl",
      "args": ["mcp", "--context", "production"]
    }
  }
}
```

## Use HTTP

For a local HTTP client, start an authenticated proxy:

```bash
varroactl mcp serve --listen 127.0.0.1:8811 --context production
```

Connect the client to `http://127.0.0.1:8811/`. Keep the listener on loopback
unless you provide separate transport security and access controls.

A client can also connect directly:

```http
POST /api/v1/mcp HTTP/1.1
Host: app.example.com
Authorization: Bearer vk_<prefix>.<secret>
Content-Type: application/json
```

Inject the key from a secret store. Do not save it in a shared client
configuration file.

## Verify access

Call `get_me` to confirm the resolved identity, then call
`get_my_permissions` with an optional namespace and controller name. An empty
permission result means authentication succeeded but the owner has no matching
role grant.

Use [Varroa RBAC](../security/varroa-rbac.md) for control-plane access and
[Jenkins RBAC](../security/jenkins-rbac.md) for controller operations.

## Related pages

- [API keys](../security/api-keys.md)
- [Identity and auditing](identity.md)
- [Tool behavior](tools.md)
- [varroactl CLI](../varroactl.md)
