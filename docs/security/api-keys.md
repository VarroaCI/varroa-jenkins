# Manage API keys

Varroa API keys authenticate CLI, MCP, CI, and other non-browser clients. A key
acts as its owner and uses that owner's current permissions.

## Create a key

Create a key from the dashboard or API:

```bash
curl -sf -X POST https://app.example.com/api/v1/me/apikeys \
  -H "Authorization: Bearer $CURRENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"deployment-automation","expiresIn":"720h"}'
```

The response contains `vk_<prefix>.<secret>`. Copy it immediately. The full
token is returned once. The prefix is safe to use when listing or revoking the
key.

Verify it:

```bash
curl -sf https://app.example.com/api/v1/me \
  -H "Authorization: Bearer $VARROA_API_KEY"
```

## Rotate or revoke

```bash
curl -sf https://app.example.com/api/v1/me/apikeys \
  -H "Authorization: Bearer $VARROA_API_KEY"

curl -sf -X POST \
  https://app.example.com/api/v1/me/apikeys/<prefix>/rotate \
  -H "Authorization: Bearer $VARROA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"expiresIn":"720h"}'

curl -sf -X DELETE \
  https://app.example.com/api/v1/me/apikeys/<prefix> \
  -H "Authorization: Bearer $VARROA_API_KEY"
```

Rotation returns a new token and revokes the old prefix. Administrators can
list or revoke another user's keys under `/api/v1/users/<name>/apikeys`.

Previously accepted credentials can remain valid in a validation cache for up
to about 60 seconds. Remove the owner's role grants as well when access must be
withdrawn across all keys immediately.

## Use a key with Jenkins

Managed controllers accept a Varroa key as a bearer token and apply the owner's
Jenkins roles:

```bash
curl -sf https://jenkins.example.com/api/json \
  -H "Authorization: Bearer $VARROA_API_KEY"
```

Validated bearer requests do not require a Jenkins CSRF crumb.

## Protect keys

- Create one key per client and purpose.
- Set an expiry for unattended automation.
- Store the full token in a secret manager.
- Log only the prefix.
- Rotate instead of sharing.
- Grant the owner only the required Varroa and Jenkins roles.

For MCP setup, use the canonical procedure in
[Connect an MCP client](../agents/connecting.md).

## Troubleshoot

| Symptom | Check |
|---|---|
| `401` | Complete token value, expiry, rotation, and revocation |
| `403` | Owner's Varroa or Jenkins role |
| Group grant is absent | Owner's resolved groups from `/api/v1/me` |
| Revoked key briefly succeeds | Allow for the validation cache interval |
