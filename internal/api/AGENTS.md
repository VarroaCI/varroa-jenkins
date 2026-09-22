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
  under `/clusters/{cluster}/...`, observability, hibernation, pipelines and runs under
  `/pipelines/{team}/...` and `/runs/{run}/...`), the `Authorizer`
  (per-request RBAC capability checks), the `Brood`/`BroodOps`/`ConfigBrood`/
  `RBACFederationReconciler` multi-cluster abstractions, the activity read/publish
  seam (`internal/api/activity/`) over `internal/recordstore`, the pipelines
  `TenantResolver` seam over the same package, and SSE fanout (`internal/api/sse/`).
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
  `router.go`'s registrations) as a coverage gate. The gate compares each manifest
  entry with the OpenAPI paths, so an entry that names no spec path fails the contract
  test: a new route needs an edit to `router.go`, to `api/openapi/paths`, and to
  `routeManifest` in the same change.
  `contractCase` carries `RawBody`/`ContentType` for non-JSON request
  bodies, so a `multipart/form-data` operation is exercised as its real request shape. `make check-client` (CI) only guards `pkg/client`, not these handlers —
  a path/shape change here must land in `api/openapi/paths|components` first, then
  `make generate-client`, for both `check-client` and the contract tests to pass.
- `Dependencies` (`deps.go`) is the DI container threaded through every handler via
  `Server.deps`: `ResourceClient` (deep ops) + `Store` (`crdstore.Backend`, all CRD
  reads/writes), a read-only controller-runtime `client.Client`,
  `Authorizer`, mite `transport.Transport`, `auth.Provider` (+ optional `Local`/
  `LDAP`), `KeyVerifier`, `TicketIssuer`, SSE `Broadcaster`, `ActivityPublisher`/
  `Backfill`, `RecordStore`, `ReconcilerAPI`, and the nil-checkable `Brood`/`BroodOps`/
  `BroodSchedules`/`ConfigBrood` (tests may omit multi-cluster wiring). There is no
  `ActivityStore` field: the in-memory ring is gone, and `Backfill` is nil, not a ring
  fallback, when `VARROA_RECORDSTORE_DSN` is unset. `BlobStore` (`blob.Store`) reads a
  run's console log and artifact bytes, nil when `VARROA_BLOBSTORE_URL` is unset.
  `TenantResolver` maps a pipelines `{team}` URL segment onto record-store tenants, nil
  in tests that do not exercise the pipelines surface. A handler on the log, artifact,
  or pipelines path must check its dependency for nil before use, matching every other
  optional field on `Dependencies`.
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
- **Cell pools** (`handlers_cellpools.go`) serve `GET /cellpools`,
  `GET /cellpools/{name}`, and `PATCH /cellpools/{name}` over the
  cluster-scoped `CellPool` CRD. There is deliberately **no `POST` and no
  `DELETE`**: the surface is a scale control, and a pool is created and
  deleted with kubectl or GitOps. The `PATCH` body is `{"spec": {…}}` holding
  only the four dials (`warm`, `maxCells`, `maxTenantsPerCell`,
  `unreadyGracePeriod`); `profileRef`, `cellTemplate`, an unknown key, or no
  recognised key at all is a `400`. The write is read-modify-write, not SSA:
  `crdstore` exposes no spec-level merge patch, and an SSA field manager would
  fight the operator that writes the spec of an auto-created pool. A conflict
  is retried three times (the pool reconciler writes the same object on its
  own tick) and then answers `409`. The handler repeats the CRD's numeric
  bounds before the write so a bad dial returns the uniform `400` envelope
  rather than a raw apiserver `422`; the CRD schema stays the authority.
- **Cell usage** (`handlers_cells.go`, `cells_service.go`) serves the flat
  `GET /cells/usage` and `GET /cells/{namespace}/{name}/usage` family: a cell
  usage row is a fleet view, never a controller sub-resource, so it does not
  live under `/clusters/{cluster}/controllers/`. Both gate each cell with
  `CanReadController`; the list drops a refused cell, and the single read
  answers **404** for an unknown controller, a controller that is not a cell,
  and a refused caller — deliberately unlike the neighbouring controller
  reads, which answer 403. `Server.CellUsage` (`cells_service.go`) is the one
  implementation the HTTP path and the `get_cell_usage` MCP tool both call.
  The counts come from `recordstore.Reader`'s `CellUsage`/`CellUsageMany`;
  the labels, the stored mite status, and `status.cell.tenants` come from the
  cell `Controller`; `queueLength`/`queueLengthAt` come from
  `MiteRegistry.IdleGauges` and are **absent, never zero**, for a remote
  cluster. `cellCapResolver` (`resolveCellCap` for one cell) is the
  first-match cap chain — the cell's `spec.cell.maxTenants` when non-zero,
  else the owning `CellPool`'s `maxTenantsPerCell`, else `deps.CellMaxTenants`
  (`VARROA_CELLS_MAX_TENANTS`, rendered from the same chart value as the
  operator's), else ten. It is never a minimum. A listing holds one resolver
  so the pools are listed once per request; a failed list skips the pool tier.
- **Cell grants** (`handleCellGrants`, `handlers_cells.go`;
  `Server.CellGrants`, `cells_service.go`) serves
  `GET /cells/{namespace}/{name}/grants`: which subjects may build on each
  tenant of a cell. It applies **two** gates in order — `CanReadController`
  on the cell, then cluster-wide `CanReadRoleBindings` — because the body
  carries subject names. `404` covers an unknown controller, a controller
  that is not a cell, and a refused controller check; `403` covers a passed
  cell check with no `rolebindings:read`, since that caller already knows the
  cell exists. It is resolver backed (`Authorizer.TenantGrantSources` over
  `rbac.TenantGrantSourcesTruncated`), reads no record store, and therefore
  answers no `503`. `jobs` carries `omitempty` and is never an empty array:
  an absent list narrows nothing.
- **Fleet locks** (`handleListTeamFleetLocks`/`handleListAllFleetLocks`,
  `handlers_pipelines.go`; `Server.ListFleetLocks`, `pipelines_service.go`)
  serves `GET /pipelines/{team}/locks` and `GET /locks` over
  `Reader.ListFleetLocks`/`ListAllFleetLocks`. `GET /locks` is the fleet-wide
  read: it requires a cluster-wide `CanReadFleetLocks`, and it resolves its
  cluster exactly as the cell-usage paths resolve theirs.
  `Server.ListFleetLocks` is the one implementation the team route and the
  `list_fleet_locks` MCP tool share, so both authorize a caller and shape a
  row identically. Every row always carries `tenant` and `jobPath`, and
  carries `runLink` only when the caller's own grant reads that row's run,
  through the same `tenantGrants`/`grantsAllow` pair the team listing uses.
  The row shape carries no holder and no queue position: a consumer that
  wants a lock's holder groups the rows it already has by name.
- **The team cell read** (`handleGetTeamCell`, `handlers_pipelines.go`) serves
  `GET /pipelines/{team}/cell`. It reads the `Team` object first and falls
  back to `TenantResolver`, so a pipelines Team awaiting its first cell
  answers 200 with `unplaced` instead of 404. The state ladder is
  `notCellHosted` → `unplaced` → `moving` → `rehydrating` → `placed`, tested in
  that order; rule four reads `status.cell.appliedTenants` and never
  `status.cell.rehydration`, which stays internal to the operator.
  The four counts come from `CellTenantUsage`'s per-job rows, summed by
  `admittedJobCounts` over the job paths `grantsAllow` admits for this
  caller — the same helpers the team listing uses. The tenant check admits
  the empty job path for a grant narrowed by `jobs` globs, so the gate
  alone cannot scope a number; a whole-tenant count here would disclose
  the load of jobs the caller may not see. Every other field stays
  tenant-level. It is the
  one pipelines path that calls a controller-scoped check, and only to set
  `canOpenCell`: the hint gates no field, so a caller without
  `controllers:read` still receives the whole body.
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
- Activity (`internal/api/activity/`): there is no in-memory ring and no retention
  dial. `deps.ActivityPublisher.Publish` (`notifyActivity` in `handlers.go`) is the
  single write path for user-action events, routed onto the NATS bus by
  `routeSubject`; the gateway publishes mite-observed events onto the same bus
  subjects. `deps.Backfill` is `internal/recordstore`'s `pgBackfill`, reading the
  Postgres `activity` table through `Reader.ListActivity` (keyset paging on
  `(ts, event_key)`, D5's scope pushed into SQL); it is nil when
  `VARROA_RECORDSTORE_DSN` is unset, and both readers — the REST handler and MCP
  `list_activity` — already branch on that nil rather than falling back to
  anything in-process. `Scope`/`ScopeFromPermissions`/`Admits` (`scope.go`) build
  and apply D5's sound-superset read scope; the per-event predicate
  (`Authorizer.CanReadActivityEvent`) stays authoritative on every returned row.
  Only audit-worthy events belong on `activity.*`: mite heartbeat/snapshot
  telemetry stays on `events.brood.>` (see `activityMessageForBroodEvent` in
  `internal/mite`), or it would flood the recorder's bounded transport buffer and
  evict real audit history. Live tailing (`GET /activity/stream`) is a separate
  path over `sse.BusFanout`, unaffected by `deps.Backfill` being nil.
- **Break-glass** (`handleBreakGlass`, `handlers.go`) is `POST
  /clusters/{cluster}/controllers/{ns}/{name}/break-glass` (route key `break-glass` in
  `controllerRoutes`, `handlers_clusters.go`, bare — no trailing slash). It mints a
  short-lived Jenkins admin token for a headless controller only, gated on the
  `controllers`/`break-glass` verb (`Authorizer.CanBreakGlass`, independent of
  `approve-restart`/`approve-deletion`), local-cluster only, and a required
  `reason` (capped, never truncated). It publishes
  `controller.breakglass.granted` through `deps.ActivityPublisher` before it mints,
  and answers `503` when the signer or the publisher is unavailable, so a grant is
  never issued unaudited. `internal/mcp`'s `call_jenkins_tool` gate reaches the same
  `CanBreakGlass` check and publishes the same event with `Source: "mcp"`.
  `controllerResponse`/`controllerDetailResponse` carry `headless bool`
  (`v1alpha1.IsHeadless(cr)`); `controllerEndpoint` returns `""` for a headless
  controller before it reads `Status.Endpoint`, and the create/update handlers skip
  the path-mode shared-host check for one. The hibernation redirect endpoint
  (`handlers_hibernation.go`) answers `404` for a headless controller, after the
  token guard and before any wake publish — a headless controller has no
  interstitial page to send a browser to.
- Hibernation and the webhook door (`handlers_hibernation.go`): `HandleHibernationDispatch`
  serves two shapes at the host root, outside `/api/v1` and outside auth. The controller shape
  (`/hibernation/{token}/clusters/{cluster}/ns/{ns}/{queue,redirect,status}/{ctrl}/...`) wakes a
  hibernated controller and buffers its webhook on `webhook.<cluster>.<ns>.<ctrl>`. The tenant
  shape (`/hibernation/{token}/teams/{team}/queue/{path...}`) is the pipelines tenant's own door:
  it verifies the token against `Team.status.pipelines.webhookToken`, verifies the forge signature
  against `Team.status.pipelines.webhookSecret` (`identifyForge`/`verifyForgeSignature`), strips
  `X-Gitlab-Token` from the envelope, and buffers on `webhook.<cluster>.tenant.<team>` — a cell
  never hibernates, so it publishes no wake. Both shapes share the JetStream `varroa_webhooks`
  stream and its existing `webhook.>` capture; the door never resolves placement, only the
  operator's replay consumer does, at delivery time.
- The agent WebSocket door (`handlers_runner_door.go`, design `add-external-runners`
  D7/D8): `HandleRunnerDoor` serves `/runners/{token}/{login,jnlpJars/agent.jar,wsagents/}`
  at the host root, outside `/api/v1` and outside auth — the URL's token is the only
  credential, the same property the hibernation door relies on. `RunnerTokenIndex`
  (`runner_token_index.go`) narrows a token to a candidate `Runner` by SHA-256 digest;
  the door still re-fetches that CRD and re-verifies `status.token` with
  `constantTimeEqual` before trusting it. The door resolves a `Runner`'s cell through
  its `Team.status.placement` and `deps.Brood.Get(ctx, placement.cluster, ...)` — never
  the local store by name/namespace alone, since a cell name is unique only within one
  cluster. It answers 503 on an empty placement (including an empty
  `placement.cluster`) so the agent's own retry loop backs off. The websocket upgrade
  goes through `httputil.ReverseProxy` with `Rewrite`/`ModifyResponse` (not a hand-
  rolled hijack): `Rewrite` rebuilds the outbound header set from nothing, so a header
  not named in the design's contract is dropped by construction; `ModifyResponse`
  applies the same allowlist to the response, since `Rewrite` cannot touch it. The door
  never trusts a client-supplied `Node-Name`; it always sets the runner's own
  `status.nodeName` and a freshly-fetched Jenkins agent secret toward the cell.
  `cmd/bff/main.go` feeds `RunnerTokenIndex` from a `runners` informer built the same
  way as the existing RBAC/Team informers.
- Pipelines and runs (`handlers_pipelines.go`, `handlers_pipelines_write.go`,
  `handlers_pipelines_auth.go`, `handlers_runs.go`, `handlers_runlog.go`,
  `runlog_events.go`, `runlog_manifest.go`, `runlog_live.go`, `runlog_poll.go`,
  `runlog_tail.go`, `runlog_notes.go`, `tenantresolver.go`, `pipelines_service.go`):
  serves the `/pipelines/{team}/...` and `/runs/{run}/...` paths over
  `internal/recordstore`. `TenantResolver` (`tenantresolver.go`, `deps.TenantResolver`)
  is the one seam that maps a `{team}` URL segment onto record-store tenants: a placed
  Team resolves to one tenant, an unplaced Team fans out over its namespaces, and a
  name matching no Team falls back to a same-named controller as a synthetic
  single-controller tenant, ambiguous across clusters. Every pipelines path narrows
  through it, then calls one of three checks in `handlers_pipelines_auth.go` —
  `canReadPipeline`, `canWritePipeline`, `canReadRun` — never a controller-scoped
  check. `canReadRun` builds its `RunTenantRow` from the run row's own `tenant` and
  `job_path` columns, so a run whose cell was deleted still resolves for a permitted
  caller. **`GET /pipelines/{team}` filters every row of its response by the
  caller's `jobs` globs**: `handleListPipelines` calls
  `(*Authorizer).tenantGrants` once per `TenantRef` and then `grantsAllow` per job
  summary, per declared job path, and per running run, dropping a refused row
  **before** it reaches the shared `jobs`/`trendByJob`/`kindByJob` maps, so a
  refused row of one tenant never replaces an admitted row of another on the same
  job path. `tenantGrants`/`grantsAllow` (`authorizer_tenant.go`) are the split
  halves of `canAccessPipeline` and hold no second copy of the rule; `tenantGrants`
  denies on a nil receiver, so the listing keeps the nil-authorizer rule. The
  response reports no count of the removed rows, and `truncated` keeps reporting
  the running-now page the store answered, not the filter. The envelope carries
  `tenantFolder` (the first admitted ref's `Folder`; every ref of one team shares
  it) and each item carries `kind`, merged through `kindByJob` rather than on the
  row: a record summary writes no kind, a declared item sets it when unset,
  `multibranch` replaces a stored `pipeline`, and the assembly loop defaults an
  absent entry to `unknown`. Every handler on this surface answers `503` when `deps.RecordStore` is nil,
  through `pipelinesUnavailable`; a handler that also reads console log or artifact
  bytes answers the same `503` when `deps.BlobStore` is nil. `GET /runs/{run}` is the stated
  exception: `run_digest.go` reads the console log for the digest's `logTail`, and a nil blob
  store or an undecodable log index degrades `logTail` to absent instead, so the run detail
  answers either way. The live log tail
  (`GET /runs/{run}/log`) authenticates over SSE's `?ticket=` query param with scope
  `run:<run_id>`, minted by `POST /stream/ticket`, never a bearer token. `runTailRegistry`
  (`runlog_tail.go`) is this BFF replica's shared lease table: at most one lease and one
  bus subscription per run per replica, reference-counted across every local viewer,
  renewed every 30 seconds, released on every disconnect. `pipelines_service.go`
  (`internal/mcp`'s non-HTTP entry point) wraps the same unexported handler logic —
  `listRunsMerged`, `resolveTeamTenants`, `createRun`, `runByID` — so the MCP tools and
  the REST paths share one implementation of every rule.
  The run detail carries `triggerError`, `triggerReasonCode`, and
  `interruptedReason`, read out of `runs.payload` in `buildRunDetail`
  alongside the log summary. The stored `trigger_http_status` reaches no
  response field. `runDetailJSON` (`handlers_runs.go`) and `RunDetail`
  (`pipelines_service.go`) must carry the same JSON tags, because
  `convertJSON` matches them by tag.
- **Item manifest source** (`controllerItemManifest`) — a placed Team
  (`TenantRef.Folder` set) reads its own tenant bundle's `items.yaml`
  (`teamPipelineBundleContent`) and re-roots it under the Team folder with
  `items.ReRoot`, which mirrors the cell's assembly; the cell's bundle holds
  the cell's own items and never a tenant job, so reading it lists the starter
  bundle's jobs under a tenant. A single-controller tenant (empty `Folder`)
  reads its controller's bundle. Job paths on this surface are Jenkins full
  names, the same shape the record store stores.
- **Pipeline template resolution** (`pipelines_definition.go`) — the job detail's
  `template` row (`buildPipelineTemplateInfo`) picks one of three sources, never
  guessing: an items-declared job (`definition.varroaTemplate`) resolves its
  version constraint against the bundle's own delivered set
  (`controllerPipelineTemplates`, reading `pipeline-templates.yaml`); a
  marker-defined job reads the four template-resolution fields off its most
  recent run's payload (`runTemplatePayload`, set by C1/C2 from
  `BuildEvent.template_name`/`template_version`/`marker_path`/
  `marker_revision`); a marker-defined job with no run yet
  (`markerContainerPath`) reports only the fixed marker path, no resolved
  template. Every read here is best-effort: an unreadable or malformed
  bundle, item, or run answers nil/empty rather than an error, because the
  bundle-validation gate is the place that already rejects bad content.
- The job detail (`GET /pipelines/{team}/jobs/{job}`) carries a `dependencies` object
  (`upstream`/`downstream` edge lists, `buildPipelineDependencies` in
  `handlers_pipelines.go`), read from `RecordStore.JobDependencies` and always present
  as `[]`/`[]` for a job with no edge, never omitted. Each edge names the other
  tenant, its job, the result threshold, and whether an export currently grants it
  (`granted`); an ungranted edge stays visible rather than being dropped. It uses the
  same `tenantForJob` authorization gate every other pipelines path uses — no separate
  check for this field.
- **Marker lint** (`handlers_pipelines_markers.go`, design D13) — `POST
  /pipelines/{team}/markers/validate` resolves the team's delivered
  templates through the same `TenantResolver`/authorization seam as every
  other pipelines path, parses the posted marker with
  `pipelinetemplate.ParseMarker`, resolves and validates its values with the
  same Go validator the bundle composer and `varroactl lint marker` use, and
  always answers `200` with a findings list — even a rejection is data, per
  D11's L4 layer, never a transport error. It contacts no cell and reads no
  record-store data, so it works while a cell is down and while the record
  store is disabled.
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
  `handlers_teams.go` maps `spec.pipelines` and `status.placement` by hand, in both
  directions; it never maps `status.pipelines.webhookToken` or `.webhookSecret` on any
  read path, because both are the tenant webhook door's credentials.
- **Cell fields on the controller DTOs** — `controllerResponse` carries seven
  (`kind`, `cellPool`, `tenantCount`, `maxTenants`, `warm`, `createdAt`,
  `unreadySince`), stamped by `fillCellSummary` (`handlers.go`). `kind` and
  `createdAt` are always present; the other five are absent for a controller
  that is not a cell. `tenantCount`, `maxTenants`, and `warm` are pointers,
  because a zero count and a false warm flag are real states an `omitempty`
  value type cannot tell from an absent field. `maxTenants` calls C2's
  `resolveCellCap`, never a second copy of the rule. `tenantCount` counts
  `status.cell.tenants`, never `appliedTenants`. The list carries no
  free-slot field: the cell usage read owns that number.
  `controllerDetailResponse` carries `kind` and a `CellStatusJSON`
  projection of `status.cell` with **ten** keys and no `rehydration` key,
  which `cell-controllers` keeps inside the operator; a contract test asserts
  the absence.
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
