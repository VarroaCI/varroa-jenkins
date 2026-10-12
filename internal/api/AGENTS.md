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
- **Local accounts beside OIDC** (`IdentityConfig.LocalAccounts`): `deps.Auth` is an
  `auth.MultiProvider` and `deps.Local` the local provider; `Server.validator()`
  unwraps the OIDC primary. `localAccountsOn()` gates user and group writes (local
  mode or local accounts). A create or update beside OIDC needs `purpose`
  (`validatePurpose`). `POST /login` beside OIDC publishes `auth.local.login`
  (warning for break-glass, info for service) and `auth.local.login_failed` only
  for an existing `User` with a purpose, never for an unknown name. `GET /me`
  reports `authMode: local` for a local session, and logout of a local session
  never sends the token to the IdP end-session endpoint. `GET /me` reports
  `signedInAt` from the session token `iat` when the token has one.
- **Cell pools** (`handlers_cellpools.go`, `cellpools_service.go`) serve
  `GET /cellpools`, `GET /cellpools/{name}`, and `PATCH /cellpools/{name}`
  over the cluster-scoped `CellPool` CRD of **every cluster of the brood**.
  There is deliberately **no `POST` and no `DELETE`**: the surface is a scale
  control, and a pool is created and deleted with kubectl or GitOps.
  `Server.ListCellPools`/`GetCellPool`/`ScaleCellPool` is the one
  implementation the HTTP routes and the three MCP tools call. Each route
  takes `?cluster=`; an unknown cluster is `404`. The list reads every known
  cluster in parallel (or the named one), answers `{items, clusters}` with a
  `cluster` on each item, sorts the local cluster first, then cluster, then
  name, and reports a failed cluster as `ok: false` without failing the call.
  A single-pool read or a scale with no cluster acts on the local cluster
  (`Server.localCluster()`). The local cluster reads and writes
  `deps.Store`; a member goes through `ConfigBrood`'s member-only
  `ListCellPools`/`GetCellPool`/`ScaleCellPool`, and an unreachable member is
  `502`. The `PATCH` body is `{"spec": {…}}` holding only the four dials
  (`warm`, `maxCells`, `maxTenantsPerCell`, `unreadyGracePeriod`);
  `profileRef`, `cellTemplate`, an unknown key, or no recognised key at all is
  a `400`. The write is `cells.ScaleCellPool`: a read-modify-write, not SSA,
  on the cluster that holds the pool (the hive runs it in its
  `cellpools.scale` handler). `crdstore` exposes no spec-level merge patch,
  and an SSA field manager would fight the operator that writes the spec of
  an auto-created pool. A conflict is retried three times and then answers
  `409`. `cells.ApplyCellPoolDials` repeats the CRD's numeric bounds so a bad
  dial returns the uniform `400` envelope rather than a raw apiserver `422`;
  the CRD schema stays the authority. A successful `PATCH` publishes
  `cellpool.updated` with the pool's cluster.
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
  cell `Controller`; each tenant row's `role`, `agentNamespace`, and
  `placementGeneration` come from that tenant's `tenant_placements` row,
  which a member cluster's operator announces to the core, so a member
  cell reports them too; `queueLength`/`queueLengthAt` come from
  `MiteRegistry.IdleGauges` and are **absent, never zero**, for a remote
  cluster. `cellCapResolver` (`resolveCellCap(ctx, cluster, cr)` for one cell) is the
  first-match cap chain — the cell's `spec.cell.maxTenants` when non-zero,
  else the owning `CellPool`'s `maxTenantsPerCell`, else `deps.CellMaxTenants`
  (`VARROA_CELLS_MAX_TENANTS`, rendered from the same chart value as the
  operator's), else ten. It is never a minimum. The pool tier reads the pools
  of the cell's **own cluster** (`resolve(cluster, cr)`), because a pool name
  is unique inside one cluster only. A listing holds one resolver, which
  keeps one pool map per cluster and reads a cluster's pools on the first cell
  of that cluster that needs the pool tier; a failed read skips the pool tier
  of that cluster for the request.
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
  read: it requires a cluster-wide `CanReadFleetLocks`, takes no `cluster`
  parameter, and reads every cluster in one list. A holder and its waiters
  can run on different clusters, so a read of one cluster would show a
  waiter with no holder. Every row of both routes carries its `cluster`.
  `Server.ListFleetLocks` is the one implementation the team route and the
  `list_fleet_locks` MCP tool share, so both authorize a caller and shape a
  row identically. Every row always carries `tenant` and `jobPath`, and
  carries `runLink` only when the caller's own grant reads that row's run,
  through the same `tenantGrants`/`grantsAllow` pair the team listing uses.
  The row shape carries no holder and no queue position: a consumer that
  wants a lock's holder groups the rows it already has by name.
- **Fleet lock force release** (`fleetlock_release.go`) serves
  `POST /locks/release` (`{name, runId, reason}`, all required, no other
  key). `Server.ReleaseFleetLock` is the one implementation the route and
  the `release_fleet_lock` MCP tool share. It checks the cluster-wide
  `CanReleaseFleetLocks` gate and the body before it sends anything, then
  asks the recorder through the `FleetLockReleaser` seam
  (`deps.FleetLockReleaser`, wired over `bus.FleetLockReleaseSubject` only
  when the record store is on; nil answers 503). The BFF never writes
  `varroa.fleet_locks`. It maps `not_found` to 404, `not_releasing` and
  `holder_live` to 409, and `released` to 200. Only the `holder_live` 409
  carries `holderCell {cluster, namespace, name}` (`ServiceError.HolderCell`,
  schema `FleetLockReleaseConflict`), and its message names the restart of
  that cell. Only on `released` it
  writes one `fleetlock.force_released` activity row on the global subject
  of the lock's cluster (source `user` or `mcp`). The wait ignores the
  caller's cancellation (`context.WithoutCancel`), and a request a recorder
  received with no reply writes one `fleetlock.force_release_unconfirmed`
  row instead (on the cluster `fleetLockCluster` read from the row before
  the request; that read fails closed: a failed read answers 503, a
  missing row 404, and a caller gone during it sends nothing), because the recorder can have committed; so does an error
  reply the recorder marks `unknown` (a failed commit). No responder
  (`ErrFleetLockReleaseNotDelivered`) and any other error reply
  (`ErrFleetLockReleaseFailed`) change no row and write nothing.
- **The team cell read** (`handleGetTeamCell`, `handlers_pipelines.go`) serves
  `GET /pipelines/{team}/cell`. It reads the `Team` object first and falls
  back to `TenantResolver`, so a pipelines Team awaiting its first cell
  answers 200 with `unplaced` instead of 404. A resolved tenant whose tenant
  value equals the team name, with no local Team, is a member-placed Team: it
  answers the placed body with no Placed reason and one slot-0 member. The state ladder is
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
  (`Run`/`Reconcile`, not controller-runtime) that writes the
  `JenkinsRole`/`JenkinsRoleBinding` set `rbac.DesiredFederatedCRs` projects
  from the core `VarroaRoleBinding` objects to every non-core member cluster
  via `ConfigBrood`, and deletes each federation-labeled child that is no
  longer desired. The core cluster itself is skipped. Only a list failure
  stops the pass for a cluster; a failed create, update, or delete of one
  object is a warning, and the pass continues with the other objects and the
  garbage-collect loops. `reconcileAndLog` logs each warning once as
  `rbac federation warning` until it clears, so the warning text must stay
  the same across passes. The pass also writes each core binding's
  `status.federation` (`rbac_federation_status.go`): `reconcileCluster` and
  `isConvergentCreate` record each fault in a per-hive `hiveResult` keyed by
  child name. A failed delete of a stale binding child records a
  `StaleJenkinsRoleBindingDeleteFailed` fault in `hiveResult.staleFaults`,
  keyed by source binding name. `staleOwnerIndex` finds the owners from the
  child's `varroa.dev/federated-bindings` annotation, restricted to the pass
  snapshot. A child with no annotation falls back to every projected snapshot
  binding whose `roleRef` equals the child's `varroa.dev/federated-from` label. A
  child with no owner gets only the warning
  `cluster <c>: delete orphaned JenkinsRoleBinding <name>`. The stale fault
  comes after the binding write faults and before `Federated`, and it also
  replaces a projector skip on that hive. `bindingDrifted` compares the
  owner annotation key, so an owner change updates the child.
  `writeStatuses` builds one entry per visited hive and
  patches the status subresource through `crdstore.PatchStatus`. It patches
  only a status that differs from the stored one, and `lastTransitionTime`
  moves only on a change of state, reason, or message: every status write is
  an informer update that enqueues another pass, so an unconditional write
  never settles. A failed cluster list writes no status.
  The pass reads one `VarroaRole` snapshot (`sourceRoles`) for the
  projection, the role owner lookup, and the role status write. A failed
  delete of a stale role child records a `StaleJenkinsRoleDeleteFailed` fault
  in `hiveResult.staleRoleFaults`, keyed by `VarroaRole` name.
  `staleRoleOwnerIndex` reads the child's `varroa.dev/federated-roles`
  annotation, restricted to the snapshot, and falls back to the
  `varroa.dev/federated-from` label when that role is in the snapshot. A
  child with no owner gets only the warning
  `cluster <c>: delete orphaned JenkinsRole <name>`. `roleDrifted` compares
  that annotation key. `writeRoleStatuses` writes `VarroaRole.status.federation`
  with entries only for faults: a hive with no fault has no entry, a hive
  whose list failed keeps its stored entry, and a departed hive loses its
  entry. A role with no stored entry and no fault gets no write. An empty
  list clears through the `crdstore` `clearStatusFields` rule for
  `varroaroles`. This is BFF-owned cross-cluster federation —
  distinct from `role_controller.go`'s single-cluster built-in-role
  reconciliation in `internal/controller`.
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
  the path-mode shared-host check for one.
  `controllerEndpoint(ctx, cr)` and `controllerDetail(ctx, cr, cluster)` take the request
  context; the list builds one `endpointResolver` (`s.endpoints(ctx)`), so a list request
  reads `varroa-defaults` at most once. The hibernation redirect endpoint
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
  `internal/recordstore`. The run log manifest (`computeRunLogManifest`) serves each
  log part that `readRunLogUnattributed` names through `blob.UnindexedLogParts` (passing the
  highest index entry's `writer`; an index reservation at the next slot overrides it) as one
  unattributed chunk, never a missing sequence. `readRunLogIndex` skips a reservation line
  (`reserved`). `readRunLogObject` is the only console reader; it serves
  no byte past those accepted parts, so a lost cell's late part never reaches a viewer. `TenantResolver` (`tenantresolver.go`, `deps.TenantResolver`)
  is the one seam that maps a `{team}` URL segment onto record-store tenants: a placed
  Team resolves to one tenant, an unplaced Team fans out over its namespaces, a name
  matching no local Team resolves from the record store's placement rows of another
  cluster (`WithMemberPlacements`, `Reader.ListTenantPlacements`: a Team a member
  cluster places lives only on that member's API server; `livePlacementRow` skips a row
  of the local cluster, a row with no cell, and a generation-0 row a teardown leaves), and otherwise falls back to a
  same-named controller as a synthetic single-controller tenant, ambiguous across
  clusters. `publishImperative` sends a command for a tenant on another cluster through
  the transport's optional `SendImperativeToCluster`, never the local send: the local
  send addresses the BFF's own cluster subject, and a member cell never receives it. Every pipelines path narrows
  through it, then calls one of three checks in `handlers_pipelines_auth.go` —
  `canReadPipeline`, `canWritePipeline`, `canReadRun` — never a controller-scoped
  check. `canReadRun` builds its `RunTenantRow` from the run row's own `tenant` and
  `job_path` columns, so a run whose cell was deleted still resolves for a permitted
  caller. **`GET /pipelines` (`handlers_pipelines_list.go`) is the landing-page team
  list**: every local Team with `spec.pipelines`, every live placement row of
  another cluster (`Reader.ListAllTenantPlacements`), each filtered per name by
  `CanReadPipeline`, and each local Team without `spec.pipelines` whose
  `spec.namespaces` resolve to a readable controller tenant (`teamNamespaceTenants`,
  the design D1 step 2 core, with the same 20-tenant ceiling). A failed controller
  list leaves the third source out and does not fail the read. The route has no
  gate of its own. It is the one pipelines path that does not
  answer `503` without a record store: it answers the local rows with
  `recordStore: false`. **`GET /pipelines/{team}` filters every row of its response by the
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
  answers either way. The digest is absent when no part has a value, and an `INTERRUPTED` run
  never carries a `logTail`. `Server.GetRun` reads `ListTestCases` once and shares it between the
  run detail's `cases` and the digest's `failedTests.cases`; a failed read leaves both lists
  absent and the run detail still answers. `GET /runs/{run}/script` (`run_script.go`,
  run read check, `503` with a nil reader or a nil blob store) and `POST /runs/{run}/replay`
  (`replayRun`, tenant write check) share `runScriptSource`, one ordered rule set
  (`not-terminal`, `template-defined`, `masked-parameter`, `no-replica`, `unreadable`,
  `not-sandbox`, `script-too-large`), so the control and the endpoint cannot disagree. The
  read opens the object `recordstore.RunScriptObject` names through `NewRangeReader`, because
  the gateway stores run state through `Replace` and only the range reader reassembles that
  part layout; it tests the recorded size before the open and parses the body with a token
  decoder that skips the XML 1.1 declaration Jenkins writes. `runTemplatePayloadOf` reads the
  template keys at `payload -> 'run.started' -> 'payload'`, the level the record nests every
  event under, and falls back to the `run.completed` copy, for the script read and the job
  page alike. A refused run answers an empty `script` and an empty `loadedScripts`. The replay
  answers `409` with `run does not replay: <reason>`, `413` over 256 KiB, mints a UUIDv7,
  keys it `replay:<source>:<new>`, reads no intent, and publishes one `REPLAY_BUILD` to the
  job's `homeRef`. `GET /runs/{run}` answers `replayOf` from `runs.replay_of`. The live log tail
  (`GET /runs/{run}/log`) authenticates over SSE's `?ticket=` query param with scope
  `run:<run_id>`, minted by `POST /stream/ticket`, never a bearer token. `runTailRegistry`
  (`runlog_tail.go`) is this BFF replica's shared lease table: at most one lease and one
  bus subscription per run per replica, reference-counted across every local viewer,
  renewed every 30 seconds, released on every disconnect. The subscription is a
  `bus.Listener`, so it subscribes again after a NATS denial. Its `Unsubscribe` waits for
  the loop, so the release calls it outside the registry lock. A live chunk that starts
  after the assembler watermark (`runlog_live.go`) is a gap: the handler never joins it. It
  reads the console object and index again (`resyncRunLogFromBlob`) and emits the lost lines
  with continued numbers. The gateway relays a chunk only after its blob writes land. When the
  read fails or the blob ends before the chunk, the stream ends without `end`, and the client
  reconnects with `from`. The finalizing poll reads the blob once more before `end`, because a
  lost last chunk shows no gap. `pipelines_service.go`
  (`internal/mcp`'s non-HTTP entry point) wraps the same unexported handler logic —
  `listRunsMerged`, `resolveTeamTenants`, `createRun`, `runByID`, `answerRunInput` — so the
  MCP tools and the REST paths share one implementation of every rule.
  The run detail carries `triggerError`, `triggerReasonCode`,
  `interruptedReason`, and `commandRefusal` (the recorder's
  `payload.command_refusal`: the newest refused cancel or input answer),
  read out of `runs.payload` in `buildRunDetail` alongside the log summary.
  The run read check alone gates `commandRefusal`, so a tenant reader sees
  a refusal without `controllers:read` on the cell. The stored `trigger_http_status` reaches no
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
- The case history (`GET /pipelines/{team}/jobs/{job}/tests/{case}`,
  `handleListJobTestCaseHistory`) applies `canReadPipeline` to each resolved tenant with
  the decoded job path, reads each admitted tenant, and merges the rows newest first.
  `{case}` is the opaque key `TestCaseKey` mints and `ParseTestCaseKey` splits; an
  unparsable key answers `400`. A job with no stored case answers an empty `items` list,
  never `404`: the case rows hold no job inventory. `limit` defaults to 50 and is clamped
  at 200.
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
  subscribers. `/activity/stream` writes only `activity.Event` records; brood
  telemetry streams only on `/stream/brood`.
  `BusFanout` and `RunFanout` subscribe through `bus.Conn.Listen`, so they
  recover from a denied subscription without a restart. `NewRunFanout` takes
  a `metric.MeterProvider` and creates `bff.run_notify.unmatched` on its
  `varroa-bff` meter; a test passes `noop.NewMeterProvider()` or a test provider.
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
  `validateTeamPipelines` refuses a pipelines block without `profile`, `agentNamespace`, or
  export `callers` with `400` before the write. An apiserver validation refusal also answers `400`.
- **Controller diff read** — `handleControllerDiff` passes the `jcasc` and `items`
  text of both sides through `internal/secretredact`. No caller gets an unredacted
  credential value, whatever its grants. The mite redacts its own answer too; the
  BFF pass covers a mite that does not yet redact, and the incoming side.
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
  applyFailed > miteUnreachable` (`MiteConnected=False/StreamLost`); a `Hibernated`/`Stopped` controller reports none, because its
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
  per-plugin drilldown. Both handlers pass the request context to `IsKnown` and
  `ListAll`, so a client disconnect ends the fan-out. The request pipeline runs in a fixed order: `GET`-only,
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

- **Bounded mite reads in the controllers list**: `handleControllersFiltered`
  builds every row in a serial pass, then reads local mite state (`Info`,
  `Snapshot`) with at most `listMiteReadConcurrency` (16) concurrent goroutines.
  Each goroutine writes only its own `out[i]`. Keep the `fillCellSummary`
  caps resolver in the serial pass; it is not goroutine-safe.
- **Remote controllers list is paged**: `busBrood.listRemote` reads every
  `controllers.list` page through `readRemotePages` (`remote_pages.go`), up
  to 128 pages. A failed page, or a `next` that does not advance, drops the
  whole member: zero rows and an `ok: false` status row. Never return a
  partial member. `handleControllersFiltered` passes the request context
  into `ListAll`. `listRemote` sends each page through `requestCtx`, so a
  client disconnect stops the walk. An undecodable row fails the member with
  `invalid controller row`. A `readRemotePages` cancel, cap, or cursor error
  replaces the fetch's reason in the status row. The fan-out keeps the status
  row that `listRemote` returns, so the reason reaches the client.
- **Remote config, brood-operation, and brood-schedule lists are paged**: `busConfigBrood.listConfigPages`,
  `ViewVersionProfiles`, `busBroodOps.listRemoteBus`, and `busBroodSchedules.listCluster` read every page through
  `readRemotePages` (`remote_pages.go`), up to 128 pages. A failed page, or a
  `next` that does not advance, drops the whole member: zero rows and an
  `ok: false` status row.
- The `busConfigBrood`, `busBroodOps`, and `busBroodSchedules` reads send
  through `requestCtx`, which prefers the `requestWithCtx` seam
  (`conn.RequestWithContext`) and falls back to the plain `request` seam on a
  goroutine (`bus_request.go`). A client disconnect ends the read. A list walk
  then fails with `list cancelled`; a config get or compose preview fails with
  `request cancelled`; a brood-op get or preview fails with `get cancelled` or
  `preview cancelled`; a brood-schedule get or list fails with
  `get brood schedule on <cluster>: <ctx err>` or
  `list brood schedules on <cluster>: <ctx err>`. Writes stay on the plain
  `request` seam, so a disconnect does not hide the outcome of a write in
  flight.

- **Cached local Controller lists**: every local Controller list goes through
  `localControllers` or `Dependencies.LocalControllers`. In the BFF the
  `controllers` informer serves it through `ControllerLister`. The informer
  strips `managedFields` (`ControllerCacheTransform`), and the lister returns
  sorted deep copies. Gets and write paths read the apiserver. Do not call
  the generic list helper for Controllers directly in `internal/api` or
  `internal/mcp`.
- `handlers_jenkins_link.go` serves `GET /clusters/{cluster}/controllers/{ns}/{name}/jenkins-link?path=`.
  `parseJenkinsItemPath` reads the job path and build number from a Jenkins item path. The route
  finds the run through `RecordStore.ListRuns` and answers `{link}`, the build page or the job
  page. It authorizes the run with the run read check and answers 404 on every miss.

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
- Every request handler passes `r.Context()` to its reads and writes. Use `context.Background()` only for detached work: a background goroutine, a shared registry, startup, or a write that must complete after the client leaves.

## Verification

```bash
make test
make lint
go test -race -count=1 ./internal/api/...
make check-client   # if any route/payload shape changed
```
