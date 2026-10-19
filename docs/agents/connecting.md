# Connect an MCP client

Varroa serves stateless Streamable HTTP MCP at
`https://<dashboard-host>/api/v1/mcp`. Authenticate with a Varroa API key that
belongs to the person or service account the client represents.

## Create a credential

Create a key as described in [API keys](../security/api-keys.md). The key uses
its owner's current Varroa and Jenkins permissions. Do not create it from an
administrator account unless the client requires administrator access.

## Configure a client

Each block below reads the key from the environment or from a client prompt.
No block holds a key. Replace `<dashboard-host>` with your dashboard address.

### Cursor

Cursor reads this shape from `.cursor/mcp.json`. It reads `VARROA_API_KEY`
from the environment.

```json
{
  "mcpServers": {
    "varroa": {
      "url": "https://<dashboard-host>/api/v1/mcp",
      "headers": {
        "Authorization": "Bearer ${env:VARROA_API_KEY}"
      }
    }
  }
}
```

### GitHub Copilot

GitHub Copilot in VS Code reads this shape from `.vscode/mcp.json`. VS Code
asks for the key when the server first starts.

```json
{
  "inputs": [
    {
      "type": "promptString",
      "id": "varroa-api-key",
      "description": "Varroa API key",
      "password": true
    }
  ],
  "servers": {
    "varroa": {
      "type": "http",
      "url": "https://<dashboard-host>/api/v1/mcp",
      "headers": {
        "Authorization": "Bearer ${input:varroa-api-key}"
      }
    }
  }
}
```

### Stdio bridge

Any MCP client that starts a local command can use this shape. The
`varroactl` context holds the key, so the client file holds no credential.

```json
{
  "mcpServers": {
    "varroa": {
      "command": "varroactl",
      "args": ["mcp"]
    }
  }
}
```

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

## From the dashboard

The dashboard shows a card named "Connect an MCP client" to a user who has no
API key and has not turned on the assistant. The card shows the MCP address
with a copy button and a link to the API keys page. It shows the three blocks
above, with your dashboard address in place of `<dashboard-host>`. The card
never shows or holds a key.

## Related pages

- [API keys](../security/api-keys.md)
- [Identity and auditing](identity.md)
- [Tool behavior](tools.md)
- [varroactl CLI](../varroactl.md)
