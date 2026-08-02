# Varroa API Reference

This document describes the Varroa BFF (Backend-For-Frontend) REST API — the primary HTTP interface for the Varroa dashboard, CLI, and third-party integrations.

## Global search

`GET /api/v1/search?q=<query>` searches authorized controllers, namespaces,
groups, and catalog items. Its `items` envelope contains `type`, `name`, `link`,
and applicable `cluster`, `namespace`, and `description` fields. Each type is
ranked and independently capped at five results. Total source failure returns
`503`; partial cluster failures return the available results.

**Audience:** API consumers, CLI developers, and integrators.

## Overview

The Varroa BFF serves a JSON REST API under `/api/v1/`. All endpoints return JSON bodies unless noted otherwise. The API is documented with an OpenAPI 3.0.3 specification and exposed via an interactive documentation page.

| Base URL | Version | Format |
|----------|---------|--------|
| `https://<varroa-host>/api/v1` | v1 | JSON |

## Interactive Docs

The BFF serves an interactive API reference powered by RapiDoc:

- **URL:** `/api/v1/docs`
- **Raw spec:** `/api/v1/openapi.json`

Both endpoints are unauthenticated (GET only). The docs page loads **zero external resources** — the RapiDoc JS bundle and the OpenAPI spec are served directly by the BFF.

## Authentication

The API uses **Bearer token** authentication passed in the `Authorization` header:

```
Authorization: Bearer <token>
```

Two token types are supported:

| Token Type | Prefix | Source | Description |
|------------|--------|--------|-------------|
| Session JWT | *(none)* | `POST /api/v1/login` or OIDC flow | Short-lived browser session token |
| API Key | `vk_` | `POST /api/v1/me/apikeys` | Long-lived credential for CI/CD and CLI use |

For API key management see [API Keys](security/api-keys.md).

## Conventions

### Cluster-Scoped Path Scheme

Per-controller resource paths are scoped under `/clusters/{cluster}/controllers/...`. The flat `/controllers/{ns}/...` paths were **removed** (they return 404 — there is no deprecation window):

| Removed Path | Current Path |
|--------------|--------------|
| `/controllers/{ns}` | `/clusters/{cluster}/controllers/{ns}` |
| `/controllers/{ns}/{name}` | `/clusters/{cluster}/controllers/{ns}/{name}` |
| `/controllers/{ns}/{name}/reconcile` | `/clusters/{cluster}/controllers/{ns}/{name}/reconcile` |

The aggregated list at `GET /api/v1/controllers` is retained and returns controllers from all known clusters, decorated with a `clusters` fan-out status sibling array.

Every controller DTO in list and detail responses carries a required `cluster` field identifying which cluster owns the controller.

### Cluster Membership

`GET /api/v1/clusters` returns the list of all known clusters with health and version stamps:

```json
{
  "items": [
    {
      "name": "core",
      "core": true,
      "healthy": true,
      "lastHeartbeat": "2026-07-06T12:00:00Z",
      "operatorVersion": "1.2.3",
      "k8sVersion": "1.28",
      "controllerCount": 5,
      "connectedCount": 3
    }
  ]
}
```

Exactly one row has `core: true` — the BFF's own cluster. Hives are discovered via heartbeat entries in the `varroa_clusters` KV bucket (TTL 90s). An expired entry (three missed heartbeats) is absent from the list.

### Aggregated List Siblings

`GET /api/v1/controllers` returns a `clusters` sibling alongside `items`:

```json
{
  "items": [...],
  "clusters": [
    {"name": "core", "ok": true},
    {"name": "dev-cluster", "ok": true},
    {"name": "prod-east", "ok": false, "error": "cluster unreachable"}
  ]
}
```

This conveys partial-result semantics: a hive whose operator did not respond within the 3s timeout contributes zero rows to `items` but appears as `{ok: false}` in `clusters`.

### Hive Limitations

The following operations are **not available for hives** and return `501 Not Implemented`:

| Operation | 501 Error Message |
|-----------|-------------------|
| Logs | `"logs is not available for hives"` |
| Preview | `"preview is not available for hives"` |
| Diff | `"diff is not available for hives"` |

A cluster whose operator leader is unreachable returns `502 Bad Gateway`:

```json
{
  "error": "cluster <name> unreachable"
}
```

### Authorization

**Cluster is a routing dimension, not a permission boundary.** Namespace-scoped RBAC grants apply identically in every cluster. A caller with `controllers:read` in namespace `team-a` can read controllers in that namespace on any known cluster. Authorization is evaluated *before* the request is proxied to the hive — an unauthorized caller never triggers a bus publish.

### Error Envelope

Every 4xx/5xx response returns a JSON error envelope:

```json
{
  "error": "human-readable message"
}
```

The response has `Content-Type: application/json`. Some endpoints include additional structured fields alongside `error` (e.g. preflight failures carry a `checks` array).

### Collection Responses

List endpoints return items wrapped in an envelope:

```json
{
  "items": [ ... ]
}
```

This uniform shape applies to controllers, users, groups, teams, built-in roles, version profiles, activity events, search results, API keys, logs, and all RBAC CRD lists. The `/api/v1/clusters/{cluster}/catalogitems` endpoint also includes an `operatorNamespace` sibling field, and its `items` are catalog summaries without rendered `status.content`; `GET /api/v1/clusters/{cluster}/catalogitems/{ns}/{name}` returns the full `CatalogItem` including content.

Catalog item lists accept optional `type`, `source`, and `q` query parameters. `type` matches the summary's `type` field exactly; `source` matches `sourceRef` exactly; and `q` is a case-insensitive substring search across `name`, `displayName`, `description`, and each entry in `tags`. Set parameters use AND semantics; an empty parameter is unset.

### Slash-Action Routes

Controller sub-resources and action endpoints use cluster-scoped paths:

| Pattern | Example |
|---------|---------|
| `GET /api/v1/clusters/{cluster}/controllers` | List controllers (scoped to cluster) |
| `POST .../controllers/{ns}` | Create controller in namespace |
| `POST .../controllers/{ns}/preflight` | Preflight validation |
| `POST .../controllers/{ns}/render` | Render YAML draft |
| `GET .../controllers/{ns}/{name}` | Get controller detail |
| `PATCH .../controllers/{ns}/{name}` | Update controller spec |
| `DELETE .../controllers/{ns}/{name}` | Delete controller |
| `GET .../controllers/{ns}/{name}/yaml` | Get raw CR YAML |
| `POST .../controllers/{ns}/{name}/reconcile` | Trigger reconciliation |
| `POST .../controllers/{ns}/{name}/approve` | Approve a pending restart |
| `POST .../controllers/{ns}/{name}/approve-deletion` | Approve an item deletion |
| `POST .../controllers/{ns}/{name}/reprovision` | Reprovision a controller |
| `POST .../controllers/{ns}/{name}/restart` | Restart (deletes the Jenkins pod) |
| `POST .../controllers/{ns}/{name}/preview` | Preview changes (local cluster only, 501 remote) |
| `GET .../controllers/{ns}/{name}/logs` | Logs (local cluster only, 501 remote) |
| `GET .../controllers/{ns}/{name}/diff` | Bundle diff (local cluster only, 501 remote) |
| `POST .../{ns}/{name}/sync` **(catalog sources)** | Trigger catalog sync |

### 202 Accepted

Action endpoints that trigger asynchronous operations return `202 Accepted` with a JSON status body:

```json
{
  "status": "triggered"
}
```

The caller polls the parent resource to observe completion (e.g. the controller's phase transitions from `Provisioning` to `Running`).

## Endpoints

The full endpoint inventory is documented in the OpenAPI spec at `/api/v1/openapi.json`. Key resource families:

| Resource | Base Path | Description |
|----------|-----------|-------------|
| Auth & Session | `/auth-config`, `/login`, `/logout`, `/me` | Authentication, profile, preferences |
| API Keys | `/me/apikeys`, `/users/{name}/apikeys` | Long-lived credential management |
| Users | `/users` | User CRUD (admin, local mode) |
| Groups | `/groups` | Group CRUD (admin, local mode) |
| Teams | `/teams` | Team CRUD with namespace scoping |
| Controllers | `/controllers` (aggregated), `/clusters/{cluster}/controllers/...` | Jenkins controller lifecycle — cluster-scoped per-resource paths |
| Clusters | `/clusters` | Cluster membership and health |
| RBAC | `/roles`, `/rolebindings`, `/jenkinsroles`, `/jenkinsrolebindings` | Role and binding CRUD |
| Catalog | `/catalogsources`, `/catalogitems` | Template catalog management |
| Composed Bundles | `/composedbundles` | Bundle composition, validation, preview |
| Provisioning | `/clusters/{cluster}/provisioningdefaults`, `/clusters/{cluster}/provisioning/config`, `/clusters/{cluster}/version-profiles`, `/clusters/{cluster}/namespaces/deployable` | Cluster-scoped brood defaults and version catalogs |
| Activity & Search | `/activity`, `/search` | Event feed and cross-resource search |
| Streams | `/stream/brood`, `/stream/ticket` | Brood-wide SSE streams (`/stream/brood` and brood-scoped tickets require a cluster-wide controllers:read binding) |
| Hibernation wake | `/hibernation/{token}/clusters/{cluster}/ns/{ns}/{queue\|redirect\|status}/{ctrl}/...` | Wake a hibernated controller and durably queue/replay SCM webhooks. Unauthenticated but token-guarded; **not** under `/api/v1`. The status response is `{"varroaWake":true,"phase":"<phase>"}`. See [Lifecycle → hibernate](operations/lifecycle.md#how-to-hibernate-idle-controllers) |
| Navigation-wake status | `/.varroa/wake/status` on the controller's own origin (or below its `/jenkins/{ns}/{name}` path prefix) | Reserved same-origin poll path served while traffic is flipped to the operator. Returns no-store JSON with `varroaWake: true` and `phase`; after the flip Jenkins answers instead, telling the interstitial to redirect. |

## Generated Go Client

The repo includes a generated Go client with a hand-written wrapper, used by the `varroactl` CLI and CI pipelines.

### Generate

```makefile
make generate-client
```

This bundles the OpenAPI spec, runs oapi-codegen, and formats the output.

### Staleness Gate

```makefile
make check-client
```

Fails if the generated files (`api/openapi/varroa.yaml`, `api/openapi/varroa.json`, `pkg/client/gen.go`) are out of date with the source specs. Run in CI after lint.

### Usage

```go
import "github.com/varroaci/varroa-jenkins/pkg/client"

c, err := client.New("https://varroa.example.com", "vk_xxx...")
if err != nil {
    log.Fatalf("create client: %v", err)
}

ctx := context.Background()

// List controllers (aggregated, all clusters).
controllers, err := c.ListControllersWithResponse(ctx, &ListControllersParams{})
if err != nil {
    log.Fatal(err)
}
if controllers.JSON200 == nil {
    apiErr := client.DecodeError(controllers.HTTPResponse)
    log.Fatalf("API error: %v", apiErr)
}
for _, ctrl := range controllers.JSON200.Items {
    fmt.Printf("  %s/%s/%s (%s)\n", ctrl.Cluster, ctrl.Namespace, ctrl.Name, ctrl.Phase)
}
// Check per-cluster fan-out status.
for _, cs := range controllers.JSON200.Clusters {
    if !cs.Ok {
        fmt.Printf("  cluster %s: %s\n", cs.Name, cs.Error)
    }
}

// Get a controller on the core cluster.
ctrl, err := c.GetControllerWithResponse(ctx, "core", "team-a", "my-ctrl")
if err != nil {
    log.Fatal(err)
}
if ctrl.JSON200 == nil {
    return client.DecodeError(ctrl.HTTPResponse)
}
fmt.Printf("Controller cluster: %s\n", ctrl.JSON200.Cluster)

// Decode any non-2xx response.
if apiErr := client.DecodeError(controllers.HTTPResponse); apiErr != nil {
    log.Printf("error %d: %s", apiErr.StatusCode, apiErr.Message)
}
```
