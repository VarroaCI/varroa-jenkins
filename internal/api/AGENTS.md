# internal/api

## Purpose

The BFF half of Varroa (`cmd/bff`): hand-written `net/http` handlers serving the HTTP
API + SSE on `:8080` (`/api/v1/...`), plus the BFF-side components that don't belong in
the controller-runtime operator — multi-cluster ("brood") fan-out, cross-cluster RBAC
federation, activity persistence, and SSE fanout.

## Ownership

- Owns: HTTP handler implementations for every BFF resource (controllers, groups,
  teams, users, VarroaRole/VarroaRoleBinding, apikeys, brood operations/schedules,
  activity feed + SSE, provisioning defaults/version profiles/catalog/composed-bundles
  under `/clusters/{cluster}/...`, observability, hibernation), the `Authorizer`
  (per-request RBAC capability checks), the `Brood`/`BroodOps`/`ConfigBrood`/
  `RBACFederationReconciler` multi-cluster abstractions, activity JetStream backfill +
  in-memory ring fallback (`internal/api/activity/`), and SSE fanout
  (`internal/api/sse/`).
- Does not own: the OpenAPI contract itself (`api/openapi/` at repo root —
  `varroa.root.yaml` + `paths/` + `components/`, bundled by `hack/openapi-bundle` into
  `varroa.json`/`varroa.yaml`); the generated Go client SDK (`pkg/client/gen.go`, via
  `make generate-client`); session/API-key authentication middleware
  (`internal/auth/middleware.go` — wraps the whole mux in `cmd/bff/main.go`, not part
  of this package); RBAC YAML generation logic (`internal/rbac`); the actual
  controller-runtime reconcile loop (`internal/controller` — this package only calls
  the `ReconcilerAPI` interface, whose remote-cluster implementation is
  `controller.NATSReconcilerProxy`).

## Local Contracts

- Entry point: `NewRouter` (`router.go`) builds one `http.ServeMux`, strips the
  `/api/v1` prefix, and registers each resource's routes against `*Server` methods
  (`NewServer(deps)`, `handlers.go:48`). There is **no** oapi-codegen-generated server
  interface or sub-router — handlers are hand-written and cross-checked against the
  OpenAPI spec by `contract_test.go`/`contract_c2_test.go`/`contract_c3_test.go`/
  `contract_cases_test.go`, which validate real handler responses against
  `api/openapi.SpecJSON` via `getkin/kin-openapi`'s `openapi3filter`.
  `contract_test.go` also hand-maintains `routeManifest` (a method×path mirror of
  `router.go`'s registrations) as a coverage gate — **adding or removing a route in
  `router.go` requires updating `routeManifest` in the same change**, or the contract
  test fails. `contractCase` carries `RawBody`/`ContentType` for non-JSON request
  bodies, so a `multipart/form-data` operation is exercised as its real request shape. `make check-client` (CI) only guards `pkg/client`, not these handlers —
  a path/shape change here must land in `api/openapi/paths|components` first, then
  `make generate-client`, for both `check-client` and the contract tests to pass.
- `Dependencies` (`deps.go`) is the DI container threaded through every handler via
  `Server.deps`: `ResourceClient` (deep ops) + `Store` (`crdstore.Backend`, all CRD
  reads/writes), a read-only controller-runtime `client.Client`,
  `Authorizer`, mite `transport.Transport`, `auth.Provider` (+ optional `Local`/
  `LDAP`), `KeyVerifier`, `TicketIssuer`, SSE `Broadcaster`, `ActivityStore`/
  `Publisher`/`Backfill`, `ReconcilerAPI`, and the nil-checkable `Brood`/`BroodOps`/
  `BroodSchedules`/`ConfigBrood` (tests may omit multi-cluster wiring).
- **Auth is not middleware in this package.** `internal/auth.AuthMiddleware`
  (`internal/auth/middleware.go`) wraps the entire mux in `cmd/bff/main.go`: it gates
  every `/api/` path, allow-lists `POST /login`, `/auth-config`, `/otel/*`,
  `GET /openapi.json`+`/docs*`, and `GET /auth/login`+`/callback` (OIDC) as
  unauthenticated, and injects `*auth.Claims` into the request context. Every handler
  here reads identity via `auth.ClaimsFromContext(r.Context())` — never its own header
  parsing. SSE routes accept a scoped, single-purpose `?ticket=` query param (minted
  by `POST /stream/ticket` → `handleStreamTicket`) instead of a bearer token, since
  `EventSource` cannot set headers; `vk_`-prefixed tokens route to the API-key
  `Verifier`, everything else to the OIDC/local/ldap `Provider`.
- `Authorizer` (`authorizer.go`) wraps an `rbac.Resolver` + a `defaultRead` flag and
  exposes one `Can*(claims, ...) bool` method per capability (`CanReadController`,
  `CanApproveRestart`, `CanManageCatalogSourcesInNamespace`,
  `CanWriteComposedBundlesInNamespace`, ...). There is no generic authz middleware —
  each handler calls the specific `Can*` it needs.
- Multi-cluster fan-out: `Brood` interface (`brood.go`), implemented by `busBrood`
  over NATS (`bus.Conn` + `bus.ClusterDirectory`), is the cross-cluster Controller
  CRUD router; `BroodOps` (`broodops_bus.go`/`broodops_aggregate.go`/
  `broodops_grammar.go`) fans one `BroodOperation` out to member clusters and
  aggregates results; `ConfigBrood` (`configbrood.go`) routes provisioning-config/
  version-profile/catalog/composed-bundle CRUD to the right cluster.
  `handlers_clusters.go` is the `/clusters/{cluster}/...` dispatcher: local
  `ResourceClient` calls when `cluster == local`, `Brood`/`ConfigBrood` otherwise.
- `RBACFederationReconciler` (`rbac_federation.go`) is a **ticker-driven loop**
  (`Run`/`Reconcile`, not controller-runtime) that propagates JenkinsRole/
  JenkinsRoleBinding CRDs from the core cluster's source of truth out to every member
  cluster via `ConfigBrood`. This is BFF-owned cross-cluster federation — distinct
  from `role_controller.go`'s single-cluster built-in-role reconciliation in
  `internal/controller`.
- Activity (`internal/api/activity/`): `Store` is the in-memory ring buffer used only
  when retention is `off` (fed by an `activity.>` bus subscriber — never written to
  directly; see the "single-ring-writer rule" on `notifyActivity` in `handlers.go`).
  `Publisher.Publish` is the single write path for user-action events, routed onto the
  NATS bus by `routeSubject`. `jetstreamBackfill.Recent`/`Query` read the bounded
  JetStream stream (`varroa_activity`, subjects `activity.<cluster>.<ns>.<ctrl>` /
  `activity.<cluster>._global`) via an **ephemeral, end-anchored consumer**; backfill is
  pull/paginated (cursor-based `Query`) — live tailing is a separate path. Every
  reader — REST handler and MCP `list_activity` alike — consumes `deps.Backfill`
  (jetstream in stream mode, `NewRingBackfill(Store)` in off mode), never the ring
  directly. Only audit-worthy events belong on `activity.*`: mite heartbeat/snapshot
  telemetry stays on `events.brood.>` (see `activityMessageForBroodEvent` in
  `internal/mite`), or it floods the bounded stream and evicts real audit history.
  `ParseRetention` (`retention.go`) parses the `off|7d|30d|90d` dial.
- SSE (`internal/api/sse/`): `Broadcaster` is process-local pub/sub for
  handler-driven events; `BusFanout` (`bus_fanout.go`) additionally subscribes
  `activity.>` and generic bus subjects and redelivers them as `Record`s to per-key
  subscribers — this backs live tailing for `/activity/stream` and `/stream/brood`.
  `HandleBroodStream`/`HandleControllerStream`/`HandleMiteStream` (`sse/handlers.go`)
  are the actual chunked-flush SSE `http.HandlerFunc`s.
- `handlers.go` (~3350 lines, the largest file) is the central handler file:
  controller list/detail/create/update/delete, pod-log fetch/stream
  (`fetchPodLogsOneShot`/`streamPodLogsSSE` via `internal/api/logbuffer`), activity
  feed + stream, search, `/me/permissions`, and VarroaRole/VarroaRoleBinding CRUD —
  the only RBAC CRDs that stay **flat** (`/roles`, `/rolebindings`, core-cluster
  only). Jenkins-side RBAC and catalog/bundle resources moved under
  `/clusters/{cluster}/...`; the old flat routes are **removed, not redirected**
  (greenfield break-and-move — see `router.go`'s comments listing the dead routes).
- Per-resource handlers follow a `handlers_<resource>.go` naming convention (groups,
  teams, users, apikeys, broodops, broodschedules, clusters, hibernation, namespaces,
  provisioning, banner, builtin, identity, fleetplugins). `deprovision.go` and `preview_util.go`
  hold controller-deletion and overlay-preview-diff helpers; `observability.go`/
  `observability_backends.go` normalize a pluggable observability-links integration.
- `attention` (`attention.go`) is the single "why is this controller unhealthy"
  projection, carried on **both** the list summary (`controllerResponse`) and the
  detail DTO (`controllerDetailResponse`). Exactly one kind is reported, by
  precedence `failed > reconcileBlocked > bootFailed > pluginRollFailed >
  applyFailed`; a `Hibernated`/`Stopped` controller reports none, because its
  runtime conditions and `LastApplyResult` are stale until it wakes. Adding a
  kind means extending `buildAttentionJSON`, the `ControllerAttention` enum in
  `api/openapi/components/schemas.yaml`, and `ATTENTION_LABEL` in the frontend
  together. `reconcileBlocked` stays on the detail DTO alongside it — the detail
  banner still consumes it.
- `FleetPluginInventory` (`fleetplugins_inventory.go`) is the reader seam for T2.2's
  fleet plugin inventory surface: it reads the classified per-controller inventory
  from T2.1's `invc/` read model via `Transport.PluginClassification`. The BFF never
  classifies — it carries the label verbatim (R19/R27). Wired through
  `Deps.FleetPluginInventory`, mirroring `UpdateCenterInventory`. The `fakeFleetInventory`
  test double keeps "no observed inventory" and "observed an empty inventory"
  distinguishable via an explicit absent-key set.
- `handlers_fleetplugins.go` serves `GET /fleet/plugins` and
  `GET /fleet/plugins/{name}` — the fleet-wide installed-plugin rollup and
  per-plugin drilldown. The request pipeline runs in a fixed order: `GET`-only,
  parse `affected` (`400` on malformed, present-but-blank rejected), validate
  cluster, nil-reader guard (`502`), unfiltered `Brood.ListAll` fan-out, mark
  non-local clusters not-covered (R22), deny-by-default authorization (`Authorizer`
  nil → zero rows), read via `FleetPluginInventory.List`, aggregate. The rollup
  accepts a `q` parameter (case-insensitive substring on plugin name); the drilldown
  matches the `{name}` segment exactly. Coverage is fleet-relative — `complete`
  means every cluster answered, independently of the `?cluster=` filter.
  `fleetplugins_aggregate.go` is the pure (no context, no clock) aggregator:
  `Rollup`, `Drill`, version-histogram ordering (rank-based, not comparator-sort),
  class breakdown (bytewise, not a ranking), envelope cross-check (hash + all
  seven flags), and `Coverage`/`reason(phase)`.

## Work Guidance

- A new or changed HTTP path/payload shape must land in
  `api/openapi/paths|components` (or `varroa.yaml`) first, then
  `make generate-client && make check-client`, in the same change as the handler —
  otherwise CI's `check-client` fails and `contract_test.go` rejects any response that
  doesn't validate against the spec.
- Adding/removing a route in `router.go` requires the matching edit to
  `routeManifest` in `contract_test.go`.
- New multi-cluster resources should route through `Brood`/`ConfigBrood` rather than
  hand-rolling per-cluster HTTP/NATS calls.

## Verification

```bash
make test
make lint
go test -race -count=1 ./internal/api/...
make check-client   # if any route/payload shape changed
```
