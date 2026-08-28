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

## Authentication

Send an API key as a bearer token:

```http
Authorization: Bearer <api-key>
```

The API returns standard HTTP status codes. `401` means the credential is missing or invalid. `403` means the identity is authenticated but lacks permission. Do not place API keys in URLs, source control, or browser-visible configuration.

## API conventions

The API base path is `/api/v1`. The interactive reference is at `/api/v1/docs`; the machine-readable document is at `/api/v1/openapi.json`. Most controller and configuration resources are addressed within a cluster. Select the intended cluster explicitly when operating across clusters. Collection responses, pagination, streaming endpoints, action routes, and error bodies are specified in OpenAPI.

For exhaustive schemas, examples, and media types, defer to the OpenAPI document rather than duplicating it here.
