# Varroa API Reference

The OpenAPI document is the authoritative reference for every path, request and response schema, authentication requirement, and error response. Use it for generated clients and integration validation.

## Start here

| Task | Reference |
|---|---|
| Browse the complete HTTP contract | `https://<varroa-host>/api/v1/docs` |
| Create and revoke credentials | [API keys](security/api-keys.md) |
| Configure authentication and identity providers | [Authentication](security/authentication.md) |
| Use command-line workflows | [varroactl](varroactl.md) |
| Authorize callers | [Varroa RBAC](security/varroa-rbac.md) |
| Read the cell paths, their gates, and their response codes | [Cell views](operations/cell-views.md) |

## Authentication

Send an API key as a bearer token:

```http
Authorization: Bearer <api-key>
```

The API returns standard HTTP status codes. `401` means the credential is missing or invalid. `403` means the identity is authenticated but lacks permission. Do not place API keys in URLs, source control, or browser-visible configuration.

## API conventions

The API base path is `/api/v1`. The interactive reference is at `/api/v1/docs`; the machine-readable document is at `/api/v1/openapi.json`. Most controller and configuration resources are addressed within a cluster. Select the intended cluster explicitly when operating across clusters. Collection responses, pagination, streaming endpoints, action routes, and error bodies are specified in OpenAPI.

A create request for a name that already exists answers `409`. Use the update route to change the object.

## Update a configuration object

The update routes of composed bundles, catalog sources, Jenkins roles, Jenkins role bindings, version profiles, and provisioning defaults use the version of your read. Each `PUT` body replaces the stored object. Start from your read. The update routes of roles and role bindings follow the same rules.

- Send the `metadata.resourceVersion` of your read. A stale version answers `409` and writes nothing.
- A version of an object that a delete removed after your read also answers `409`.
- A body without `metadata.resourceVersion` only creates the object. It answers `409` when the object exists.
- A federated copy of a Jenkins role or binding answers `409` with the code `federated_copy`. A repeat does not succeed. The response text names the source object.
- A create or update body for a Jenkins role or binding that sets the label `varroa.dev/federated-from` answers `400` with the code `federated_label`. Only federation sets that label.
- A body without `metadata.resourceVersion` needs the create permission as well as the update permission. Without it, the route answers `403`. Provisioning defaults have no create permission, so their route needs the update permission only.

On a `409` without a code, read the object again and repeat the change. If that read answers `404`, send the change without `metadata.resourceVersion`.

## Requests that an operator serves

The BFF sends each write, and each read of a member cluster, to the operator of the target cluster. Two answers mean that the operator did not reply:

- `503` with the code `operator_unavailable`: no operator of the cluster received the request. The operator restarts or has no leader. Nothing changed, so send the request again later.
- `502` with the text `cluster <name> unreachable`: the operator did not answer in time. It can have received the request. Read the object before you send a write again.

A brood operation row of a cluster that no operator answered carries the code `operator_unavailable`. A create or preview that succeeds on no cluster answers `503` when such a row exists and no row names an unreachable cluster.

For exhaustive schemas, examples, and media types, defer to the OpenAPI document rather than duplicating it here.
