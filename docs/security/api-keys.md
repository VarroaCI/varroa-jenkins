# API Keys

<!-- sources: internal/api/handlers_apikeys.go, internal/api/auth_handlers.go, internal/api/router.go, internal/apikey/ -->

Long-lived credentials for programmatic access to the Varroa REST API — CI pipelines, CLI tooling, scripts — without a browser login session.

## Concepts

- Token format: `Authorization: Bearer vk_<prefix>.<secret>`. The `vk_` prefix marks a Varroa key; `<prefix>` (~8 bytes base32) is the **public** accessor used for listing/revocation and may appear in logs; `<secret>` (32 random bytes) is hashed with SHA-256 server-side, never stored in plaintext, and shown **exactly once** at creation.
- A key acts as its **owner**: authorization is the owner's live [Varroa RBAC](varroa-rbac.md) at request time — revoking a role narrows every key the user holds, immediately.
- Keys work in every auth mode. Group memberships for API-key requests are resolved from the owning `User` CR (`status.observedGroups` for IdP-managed users, or `Group.spec.members` for local users); if the user has never logged in (no observedGroups yet), groups may be empty.

## How to create a key

Dashboard: open the profile menu in the top bar, select **API Keys**, then select
**Create API key**. The dedicated `/api-keys` page shows the key's name, public
prefix, creation and usage dates, expiry, and active/expired status. Copy a newly
created or rotated token immediately; the full token is disclosed only once.

API (authenticate with your session or an existing key):

```bash
curl -sf -X POST https://app.example.com/api/v1/me/apikeys \
  -H "Authorization: Bearer <your-token>" \
  -H "Content-Type: application/json" \
  -d '{"expiresIn": "720h"}'          # optional expiry; omit for non-expiring
# {"token":"vk_<prefix>.<secret>","warning":"this token will not be shown again"}
```

**Verify:** the new key authenticates and acts as you:

```bash
curl -sf https://app.example.com/api/v1/me -H "Authorization: Bearer vk_<prefix>.<secret>" | jq .username
```

## How to list, rotate, and revoke

```bash
# List your keys (prefixes + metadata, never secrets)
curl -sf https://app.example.com/api/v1/me/apikeys -H "Authorization: Bearer vk_…"

# Rotate: returns a fresh token, revokes the old one server-side
curl -sf -X POST https://app.example.com/api/v1/me/apikeys/<prefix>/rotate -H "Authorization: Bearer vk_…"

# Revoke (the dashboard requires a permanent-action confirmation)
curl -sf -X DELETE https://app.example.com/api/v1/me/apikeys/<prefix> -H "Authorization: Bearer vk_…"
```

Admins can manage anyone's keys:

```bash
curl -sf https://app.example.com/api/v1/users/<username>/apikeys -H "Authorization: Bearer <admin-token>"
curl -sf -X DELETE https://app.example.com/api/v1/users/<username>/apikeys/<prefix> -H "Authorization: Bearer <admin-token>"
```

**Verify:** a revoked key gets `401` on its next request; it disappears from the list.

## Concepts: operational notes

- Prefer per-purpose keys with expiries (`expiresIn`) over one immortal key; rotation is one call and the old token dies with it.
- Treat the full token like a password; the prefix alone is safe to reference in tickets/logs.
- Key verification is served by the control plane (the gateway exposes a dedicated verification port for in-cluster consumers, which the chart's [network policies](../install/network-policies.md) account for).

## Troubleshooting

- `401` with a fresh key → the whole token (`vk_<prefix>.<secret>`) must be sent, not just the secret.
- Key works for you but a scoped API call 403s → keys don't add power; the owner's [roles](varroa-rbac.md) are the ceiling.
- Group-gated action fails only via key (OIDC mode) → the OIDC-groups caveat above; add `Group` CR memberships.

## Related pages

- [Varroa RBAC](varroa-rbac.md) — what a key is allowed to do
- [Authentication](authentication.md) — session-based access for humans
