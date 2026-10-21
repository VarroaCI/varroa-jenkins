# internal

## Purpose

Home of the Go backend implementation: the `varroa-operator` reconciler, the
`varroa-gateway` mite gRPC server, and the `varroa-bff` HTTP/SSE API server
share this tree. Each subpackage is either a durable domain (gets its own
`AGENTS.md`, listed below) or a focused, single-responsibility helper package
(documented inline here).

## Ownership

- Owns: repo-wide conventions for how backend packages are organized and
  cross-referenced. Does not restate root-level architecture (see
  `../AGENTS.md` Architecture Map) or CRD schemas (`api/v1alpha1/`).
- Each child package below owns its own internals; this doc is the index and
  the contract for the ones too small to warrant their own file.

## Local Contracts

One-line contract per package without its own `AGENTS.md` (verified by
reading each package's entry point):

- **`crdstore/`** — the generic typed store for `varroa.dev` CRDs: an
  10-method unstructured-level `Backend` seam (implemented by
  `controller.ClientsetClient` and the in-memory `Fake`), package-level
  generic helpers (`Get[T]`/`List[T]`/`Apply[T]`/`ApplyOwned[T]`/`Create`/
  `Update`/`UpdateStatus`/`MutateStatus`/`Delete`/`PatchStatus`/`PatchAnnotations`/`PatchFinalizers`/`PatchMetaAt`), and a static
  type→GVR registry covering all 18 kinds. `PatchMetaAt` sends a
  `resourceVersion` precondition in the metadata patch and returns the next
  version; a write chained from one read uses it. Every CRD read/write in the
  operator, BFF, and MCP goes through it; new kinds register here instead
  of adding interface methods. `registry.go` is the **only** place a kind
  registers: a kind that also needs a deep Kubernetes operation carries a
  second GVR variable in `internal/controller/clientset_client.go`, and a kind
  this store alone serves (`runners`, `profilecandidates`, `cellpools`) does
  not. `ApplyStatusClears` (`registry.go`) is the one implementation of the
  clearable-key rule: an `omitempty` status key a merge patch omits leaves a
  stale server value, so `registry.go` names, per kind, the keys a patch must
  carry as an explicit zero. A `NestedClear` value names such keys **inside**
  one status object, for a writer that sends the object and clears a field of
  it. Only `ClientsetClient.PatchObjectStatus` applies it: the `Fake` does not,
  so a test asserts on the recorded patch or applies the helper itself. Tests
  seed `crdstore.Fake` (upsert
  semantics; re-seed after mutating a seeded object) and assert on its
  `StatusPatches`/`MetaPatches` recorders. `ApplyOwned` is the guarded sibling
  of `Apply`: `Apply` is a full-object replace whose internal `Get` never
  consults ownership, so a pre-call check cannot protect anything. `ApplyOwned`
  Gets, creates when absent (looping back to `Get` on `AlreadyExists`), returns
  `ErrNotOwned` for a live object the caller's predicate rejects, and Updates
  carrying the **live** resourceVersion so a concurrent write conflicts instead
  of being clobbered. `MutateStatus` is the status compare-and-swap: it reads
  the live object, calls a side-effect-free mutate function, and writes the
  status subresource with the read resourceVersion, reading again on each
  conflict. A writer that owns only some status fields uses it in place of a
  blind `PatchStatus`. The `Fake` keeps one `resourceVersion` counter. `Seed`, `CreateObject`, and each write that changes an object set the next value, and `Seed` writes it back to an object that has none. `UpdateObject` and `UpdateObjectStatus` answer Invalid for an empty `resourceVersion`, and the `*At` patches and both updates answer Conflict for a stale one.
- **`auth/`** — OIDC/Dex, local, and LDAP auth providers (`AuthMode` =
  `oidc|local|ldap`) plus HTTP middleware (`middleware.go`) that resolves
  request claims; subpackages `auth/local`, `auth/ldap`, `auth/identity`,
  `auth/userdirectory`, `auth/schedule` hold provider-specific logic.
  `auth/userdirectory.RecordLogin` stores the configured custom claims (`Claims.Extra`) in `varroa.dev/oidc-user-claims` and moves a replaced or dropped preferred username, email, or custom claim value to `varroa.dev/oidc-former-identifiers`, which only `deprovisionUser` reads (through `api.DeleteUser`). `RecordLogin` writes with the read `resourceVersion` and reads again on a conflict, up to `retry.DefaultRetry` attempts.
  `multi.go`'s `MultiProvider` serves local accounts beside OIDC
  (`--local-accounts`). It selects the verifier by the unverified `iss`. It
  refuses an IdP token whose `sub`, `preferred_username`, `email`, `name`, or any
  configured user claim starts with `LocalSubjectPrefix` (`local:`). The check trims
  space and ignores letter case. Only a local session beside OIDC
  carries that prefix. The OIDC `Validator` resolves `Claims.Groups` from the
  configured group claims at verify time (`SetClaimNames`). Every consumer reads
  `Claims.Groups`, and no code reads a claim name. `Claims.Extra` carries the
  configured custom user claims and travels in stream tickets. `auth.IsLocalSession` is the one test for it. Every claim-to-`User`
  mapping (`identity.UserResourceName`, the API-key group ladder) strips it. The
  API-key owner label is `local.<name>`, because a label value cannot hold `:`.
- **`apikey/`** — `vk_`-prefixed API key generation/parsing (`token.go`:
  `Generate`/`Parse`, prefix+secret split), verification HTTP handler
  (`httphandler.go`), and in-memory last-used tracking with a periodic flush
  (`lastused.go`).
- **`jenkins/`** — HTTP client for the Jenkins REST API, used by mite
  sidecars to manage their local Jenkins instance (crumb handling, retries).
  Every error that quotes a 4 KiB error-body read goes through `refusalText`:
  a plugin refusal is plain text and passes whole; a Jenkins core HTML error
  page keeps only its `<title>`, without the ` - Jenkins` suffix. The text is
  cut at 300 runes. `CheckConfig` and
  `ExportConfig` keep their 8 KiB raw read, because a JCasC error body can be
  longer than 300 runes and stays useful.
  Its `items` subpackage models `items.yaml` (the manifest, `config.xml`
  rendering, and the engine's incremental apply) and adds a tenant
  composition seam for a cell: `ReRoot` wraps one tenant's items under a
  single top-level folder, and `Merge` joins several tenants' manifests into
  one, rejecting a duplicate top-level name and fixing the remove strategy to
  `sync` — the only way a removed tenant's folder gets deleted.
  `Manifest.Retain` lists top-level paths the engine neither writes nor
  deletes: their managed items keep their previous hash in the managed
  cache, matched on whole path segments. Only the cell merge writes it, for
  a held tenant folder; `ReRoot` drops it. The engine continues past a refused
  item (generate, create, or update). It keeps that item's previous hash, skips the
  children of a refused create, and aborts only on a failed existence check. A failed managed-cache read or save sets `ApplyResult.CacheError`, and the mite reports a failed items section. A failed read deletes nothing and saves no cache. The save writes a temporary file and renames it over the cache. `Engine.Forget` removes a path and its descendants from the cache after an approved delete; a failed read changes nothing. A `gitea` source or navigator
  always renders its discovery traits (the defaults when none are declared)
  and a `WebhookRegistrationTrait` set to `DISABLE`. A `github` source
  renders its declared traits (`githubSkipNotifications` renders the
  `skip-notifications-trait` plugin's `GithubSkipNotifications`), and renders no
  `traits` element when it declares none. The sibling
  `items/challengecorpus` loads the challenge corpus manifest. It refuses a case directory with no manifest entry, and a manifest entry with no directory.
  The `items/lint` package owns the cell rule set (`lint.CellRules`: `no-poll-scm`,
  `no-global-credentials`, `no-controller-config-dependency`, `no-job-dsl`) and
  `lint.Summary`. The cell runs it over each tenant's re-rooted manifest before a
  push, and the pipelines read runs it too. `no-job-dsl` also runs over the
  inline `jenkinsfile` of each tenant pipeline template (`lint.RunScript`).
  `items.Parse` still accepts every shape a non-cell controller accepts and
  ignores an unknown key. `items.ParseStrict` refuses an unknown key, and only
  the cell's tenant materialization calls it; the mite engine keeps `Parse`.
  The plugin's `VarroaCellJobDslBlock` blocks the step at run time on a cell
  (`VARROA_CELL`), for every script source. A cell does not install `job-dsl`. The listener is a second guard. An item declares its build parameters two ways — a flat list at
  the item level (`Item.Parameters`) or a parameters property nested under
  `properties` (`Property.Parameters`) — and `configxml.go` merges both
  sources into one `ParametersDefinitionProperty`, the same merge the
  pipelines API's `buildPipelineParameters` already performs;
  `Item.Validate` rejects a name repeated across the two sources and a
  `durabilityHint` value outside Jenkins core's three `FlowDurabilityHint`
  names, for every controller. A `pipeline` item's `quietPeriod` renders as
  the job's `<quietPeriod>` element, and `resumeBlocked: true` renders the
  same single `DisableResumeJobProperty` a `disableResume` property renders.
  Two item properties feed the cell's agent fence and shared-library support:
  `KubernetesFolderProperty` (`kubernetes.permittedClouds`) restricts which
  kubernetes-plugin clouds a folder's items may use — the cell assembly
  pass sets it to the tenant's own cloud name. `FolderLibrariesProperty`
  (`folderLibraries`) renders `org.jenkinsci.plugins.workflow.libs.FolderLibraries`.
  A library takes one retriever: `modernSCM` with a GitHub or git source, or
  `legacySCM` with a gitSCM block. `defaultVersion` is written through.
  `Item.Validate` (`validateFolderLibraries`) refuses a malformed library
  before any renderer runs. `LibraryConfiguration.RequiredPlugins` and
  `CredentialRefs` feed the controller's plugin and credential checks. A
  `multibranch` item's `orphanedItemStrategy` renders `DefaultOrphanedItemStrategy`
  with `pruneDeadBranches` on; a zero or absent limit renders `-1`, and
  `Item.Validate` (`validateOrphanedItemStrategy`) rejects the key on another kind
  and a negative limit.
  `configxml.go` also injects a fixed `buildStrategies` element onto every
  `multibranch` item and `organizationFolder` item. It holds two strategies: the
  skip-initial-build strategy (plugin `basic-branch-build-strategies`), so a branch
  scan starts no build, and `VarroaPushedBranchBuildStrategy` (plugin
  `varroa-mite-auth`), which builds a branch that a push creates, once. The skip
  strategy needs plugin `basic-branch-build-strategies`, which `internal/controller`'s
  `checkBranchBuildStrategiesPlugin` requires before it resolves a bundle
  holding either item kind. A `triggers.varroaUpstream` block
  (`VarroaUpstreamTrigger{Tenant, Job, Result}`) declares a job's upstream
  producer for the cross-tenant fan-out: `Result` defaults to `SUCCESS` and
  is otherwise restricted to `SUCCESS`/`UNSTABLE`/`FAILURE`, and `Job` names
  the producer job relative to the producer tenant's own folder, with no
  tenant prefix. `buildTriggersXML` is the one trigger renderer: a
  `freeStyle` job gets a top-level `<triggers>`, and a `pipeline` job gets
  `PipelineTriggersJobProperty` holding the item-level `triggers` merged
  with every `properties[].pipelineTriggers`. `validateTriggers` rejects
  triggers on any other kind, `pipelineTriggers` off `pipeline`, a
  repeated trigger type across both spellings, an empty `cron.spec`, and a
  `genericTrigger` with no `token`. A `triggers.genericTrigger` block
  (`GenericTrigger{Token, GenericVariables, ...}`) renders the
  `generic-webhook-trigger` plugin's trigger; its `token` is the value a
  webhook call carries in `?token=`.
  A cell rewrites each job from `items.yaml` on a restart, so only an item
  trigger survives it; a Jenkinsfile `triggers {}` block does not. Two
  item forms select a delivered pipeline
  template: `definition.varroaTemplate`
  (`VarroaTemplateDefinition{Template, TemplateVersion, Values}`) on a
  regular job renders to
  `org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateFlowDefinition`,
  and `projectFactory.varroaMarkerProjectFactory`
  (`VarroaMarkerProjectFactory`, no fields) on a `multibranch` item renders
  to `...template.VarroaTemplateBranchProjectFactory`, which hands every
  branch the marker-driven flow definition at run time instead of a fixed
  Jenkinsfile path. `composer.go`'s `validateItemTemplateValues` validates
  the first form's `values` against the named template's parameter schema
  at compose time; the second form's marker is read and validated only at
  branch-build time, in the plugin.
- **`overlay/`** — strategic-merge-patch engine over StatefulSet/Service/
  Ingress for `Controller.spec.podOverrides`/`resourceOverlay`; shared by the
  operator's apply path and the BFF's live merge-preview endpoint.
- **`transport/`** — the interface through which the operator sends
  commands to and reads state from mites, decoupled from gRPC/NATS specifics:
  `LocalRegistry` (in-process, gateway-colocated) and a NATS JetStream-backed
  bus transport (`bus_transport.go`, `bus_read_model.go`,
  `bus_read_model_transport.go`) for the multi-cluster case where operator
  and gateway run in different clusters. `BusTransport.OnCommandResult` runs
  after a result enters the buffer. It must not block. `StreamDegraded` reports whether the
  gateway's bus→stream bridge is broken (read from `bus.Presence`; always false
  for `LocalRegistry`, which has no bus hop) — a mite can hold a healthy gRPC
  stream while receiving no desired state at all. `BusTransport.Send` is a
  core NATS request answered by the gateway holding the stream; it stores
  nothing and returns `ErrMiteUnreachable` when no gateway answers.
  `BusTransport.SendImperative` publishes nothing for an ended context. There is
  no `ClearDesired`: no desired state is stored. `BusTransport.ensureSub`
  reads the inbound subscription's denial watcher under `b.mu` on every
  call. After a server denial, it subscribes again and keeps the result
  channel. `Transport.EnsureInbound` opens the inbound subscription. The
  reconciler calls it on every owned tick in Provisioning, Running, Connected,
  and Failed, before the phase dispatch. A mite can request a token before the
  first desired-state push.
  `Transport.Forget` drops a deleted controller's entries: the
  reconciler calls it in the deletion branch and on a `Controller`
  NotFound; `ApproveRestart` refuses a controller under deletion and
  reads it again after its send. `BusTransport.Forget` unsubscribes, releases the denial
  watcher, and deletes the result channel and the cached report; a report
  callback stores only while its entry is still current.
  `Transport.ForgetMatching` does the same for every mite that a predicate
  matches; the operator uses it to drop the mites of controllers that it no
  longer owns (see `controller/AGENTS.md`, `sharding/`). Not related to git — bundle
  git materialization lives in `bundle/resolver.go`.
- **`mcp/`** — MCP (Model Context Protocol) Streamable-HTTP server exposing
  Varroa's CRUD surface as MCP tools; built on `internal/api.Dependencies` so it
  stays in lockstep with the REST API surface. `handler.go` wires the server,
  `proxy.go` forwards to per-controller Jenkins MCP servers, and tool
  registration is split one file per domain (`tools_controllers.go`,
  `tools_users.go`, …) with `tools.go` holding `registerAllTools`, `resultJSON`,
  and the shared argument helpers. Six invariants the compiler will not enforce:
  (1) return every tool result through `resultJSON`, never
  `mcp.NewToolResultJSON` — MCP requires `structuredContent` to be a JSON
  object, and `resultJSON` boxes collections as `{"items": …, "count": n}` so
  array-returning tools stay legal; (2) tools that reach the operator through
  `deps.Brood` must pass `Namespace` explicitly in `ControllersCreateArgs` — the
  operator sets `cr.Namespace` from the request field, not from the marshalled
  CR, so omitting it 404s; (3) credential stripping happens inside `resultJSON`
  via `api.SanitizeObject`, shared with the REST layer — it is deliberately at
  that one chokepoint rather than per tool, because a per-call-site omission
  is exactly the failure mode a broad tool surface makes inevitable, so a tool
  that bypasses `resultJSON` bypasses redaction; (4) register tools through `addTool`
  with a `toolKind`, never `mcpServer.AddTool` — mcp-go's `NewTool` defaults
  every annotation to its most alarming value (`destructiveHint` and
  `openWorldHint` true), so an unclassified tool ships those. `openWorldHint`
  must be set explicitly even when false. `kindUpdate` is Apply-backed and so
  genuinely destructive (an absent field is removed); `kindUpdatePartial`
  writes a named subset and removes nothing, so it is non-destructive and
  idempotent. Both mutate, so both emit an activity event — a new kind must
  join **both** the `annotationsFor` and the `mutates` switch, and a mutating
  tool also needs an entry in `guardArgsFor` (and often a seeded object in
  `seedGuardObjects`) or `TestEveryMutatingToolEmitsActivity` panics.
  `expectedToolKinds` in
  `annotations_test.go` is exhaustive and fails on any unlisted tool;
  (5) guard every optional `api.Dependencies` field before an *unconditional*
  dereference, returning a tool error the handler cannot work without it
  (`if deps.Authorizer == nil { return mcp.NewToolResultError(…) }`) — the BFF
  wires them conditionally, and an unguarded dereference panics the request
  instead of returning a usable error. A dep a tool uses only
  conditionally needs no such guard: `get_controller_logs` deliberately skips
  authorization when `Authorizer` is nil while still guarding `Client`, which it
  cannot read logs without; (6) declared arguments and
  applied fields must match in both directions. Input schemas *are* validated, so
  a field the handler reads but the schema omits is unreachable, and an argument
  the schema declares but the handler ignores is accepted and silently dropped.
  The reverse also bites: a spec field the REST layer patches freely but no
  tool argument names is unreachable from MCP (e.g. `spec.hibernation`).
  `create_controller`/
  `update_controller` answer that with named shorthands over a
  `controllerSpecPatch` merge map plus a generic `spec` object passthrough.
  Unrecognized arguments are **rejected by name** (`unknownControllerArgsError`),
  not dropped: "nothing to update" without naming them reads as a malformed
  request rather than an unsupported field, and dropping one that arrives
  alongside a valid field would succeed while discarding half the call. The
  named shorthands never zero a field (an empty `version` means "not supplied");
  `spec` is a literal merge patch, so an explicit `""`/`null` there clears,
  matching the REST body. Two update tools instead decide on **key presence**
  (omit = preserve, present = set/replace/clear): `update_composed_bundle` and
  `update_catalog_source`. Every other update tool still reads the value, so
  `""` there means "not supplied" — do not generalize either rule across the
  package. `update_composed_bundle` replaces `inputs` wholesale (the list is the
  merge order, so a partial edit has no meaning) and rejects an empty one;
  `update_catalog_source` clears the abandoned source kind's fields when the
  other kind is set, mirroring the CRD's at-most-one rule via
  `validateCatalogSourceSpec`. The catalog-source and composed-bundle tools read
  non-string scalars only through `boolArg`/`intArg` (`tools.go`): both report
  presence separately, and `intArg` rejects a fractional or out-of-range JSON
  number rather than truncating it. Older tools still cast `float64` inline;
  move them to `intArg` when touched; (7) activity reads go through
  `deps.Backfill` (the source the REST `/activity` handler uses; there is no
  `deps.ActivityStore` and no ring) — `deps.Backfill` is nil when
  `VARROA_RECORDSTORE_DSN` is unset, and a tool must treat that nil the same
  way the REST handler does: an empty feed, not a panic; and brood-path
  mutation errors must be built with `broodFailureToolError` so
  the operator's failing preflight checks survive into the tool error instead
  of collapsing to a bare "preflight failed".
  A list tool that fans out across clusters (`list_controllers`, `list_cell_pools`, `search`) adds `clusters`, the `api.ClusterFanoutStatus` rows, to `{items, count}`; `project.go`'s `withClustersProperty` declares it.
  Listing tools return a projection (`project.go`), not full CRs; a declared
  `outputSchema` covering more than one result shape must use `anyOf`, since
  `oneOf` requires exactly one match and the shapes overlap. Output schemas are
  **not** server-validated — `handler.go` enables input validation only — so
  they are asserted in tests.
  `tools_pipelines.go` registers twelve pipeline tools: `run_pipeline`,
  `proceed_run_input`, `abort_run_input`, `replay_run`, `restart_run`, `scan_multibranch`, and
  `release_fleet_lock` (action), and `list_runs`, `get_run`, `get_run_log`,
  `list_job_branches`, and `list_fleet_locks` (read). Each
  calls the same `internal/api` service method (`pipelines_service.go`'s
  `RunPipeline`, `ProceedRunInput`, `AbortRunInput`, `ReplayRun`, `RestartRun`,
  `ListRuns`, `GetRun`, `GetRunLog`, `ListJobBranches`, `ListFleetLocks`,
  `ReleaseFleetLock`) the matching HTTP handler calls
  (`replay_run` reaches `POST /runs/{run}/replay`; the script read
  `GET /runs/{run}/script` has no tool, because a script can reach 256 KiB),
  so the
  MCP surface and the REST surface authorize and read identically; no tool
  here issues an HTTP request to the BFF or calls a controller-scoped check.
  The catalog and bundle read tools check `CanReadConfigInNamespace` for a single
  object or filter list items with `ConfigReadFilter`; the write tools use the
  `...InNamespace` gates.
  `get_run_log` returns a bounded tail (`tailLines`, default 100, clamped 1
  to 500) as plain text, never a stream, and starts no live tail.
  `tools_cellpools.go` registers `list_cell_pools`, `get_cell_pool`, and
  `update_cell_pool` over `internal/api`'s `Server.ListCellPools`,
  `GetCellPool`, and `ScaleCellPool`, the same service the `/cellpools`
  routes call; each takes an optional `cluster`, and each result names the
  pool's cluster.
  `tools_cells.go` registers one read tool, `get_cell_usage`, over
  `internal/api`'s `Server.CellUsage` — the same entry point the single-cell
  HTTP path uses. It is a cell read, so it **does** call `CanReadController`,
  unlike the four pipeline tools; a nil authorizer denies it outright, which
  makes it stricter than `list_controllers`/`get_controller`/
  `get_controller_logs`, which skip the check in that state.
- **`preflight/`** — provisioning preflight `Check`s (pass/warn/fail) run
  before/alongside `handleProvisioning`, consuming `internal/tenancy` for
  namespace-state checks.
- **`observability/`** — parses/validates the
  `observability.varroa.dev/{providers,capabilities}` intent annotations on
  `CatalogItem`/`ComposedBundle`; downstream consumers push the resolved
  intent to mites for remote-cluster metrics.
- **`tenancy/`** — namespace classification and lifecycle (`NamespaceState`)
  for the control plane; the seam multi-cluster/team-scoping features build on.
- **`telemetry/`** — OpenTelemetry config helpers, incl.
  `Disabled()` reading `VARROA_TELEMETRY_DISABLED`. `NewMeterProvider` builds
  the provider that `InitTelemetry` installs as global; a test uses it and
  never calls `InitTelemetry`. `initTelemetry` takes the install step as a
  setter, so `init_meter_test.go` covers the install without the global provider. `meter_guard_test.go` is the repo-wide guard:
  only a `cmd/*/main.go` or `cmd/recorder/run.go` reads `otel.Meter` or
  `otel.GetMeterProvider`, and no test calls `SetMeterProvider` or
  `InitTelemetry`. The guard has no allowlist.
- **`signing/`** — shared RSA JWT signing + JWKS primitives; both the mite
  operator-JWT signer (`internal/mite`) and `auth/local` consume the same
  `Signer` so the JWK `kid` matches across every token a given keypair issues.
- **`oci/`** — oras-go v2 wrapper: `BlobStore` interface (`store.go`) with
  `LayoutStore` (OCI image-layout dir) and `RegistryStore` (remote registry,
  docker-config-path auth via `CredentialConfigPath`) implementations, plugin-pack
  build/read (`pluginpack.go`), and cross-store `Copy`. Owned by the update-center
  epic's C1; consumers (`internal/updatecenter`, `internal/bundle`, `cmd/varroactl`)
  use the interface, never redefine it. Media types/annotations are a pinned
  contract shared with `charts/varroa`'s update-center values.
  `PackConfig.Kind` (`profile`|`addon`) is the sole pack discriminator — an
  empty `Profile` is not an implicit one — and is required on write and on
  read. `internal/oci` owns the **only** encoder for the structured layer
  annotations (`tags`, `dependencies`); consumers must go through
  `ResolvedPlugin`'s typed fields rather than hand-rolling the JSON.
  `ApplyHPIMetadata` is the single implementation of the derived-annotation
  contract every pack producer shares. The encoder writes `[]` for a
  known-empty dependency list and OMITS the annotation only when the list is
  unknown, so a missing annotation means "unknown", never "empty". The
  discriminator for a pack that predates the annotation contract is
  `PackConfig.Kind` being unset.
  `PackConfig.UploadedBy`/`UploadedAt` are provenance for an authenticated user
  upload and are empty for every other producer.
- **`updatecenter/`** — the in-cluster Jenkins update center service behind
  `cmd/updatecenter`: metadata generation (`update-center.actual.json` +
  JSONP wrapper, and `plugin-versions.json`, which lists every stored version for exact pins and, with pull-through on, each declared exact pin the store does not hold yet (filled from upstream metadata; the archive-only pins stay out); each plugin's dependencies come from the layer annotation,
  with a lazy read of the stored manifest as fallback, cached by layer digest;
  a plugin whose list is unknown is left out of the metadata but stays
  downloadable), blob download with sha256-verified pull-through from
  `updates.jenkins.io` (upstream sha256s are **base64** in update-center.json —
  not hex), Bearer-token OCI-layout-tarball import (the import refuses a whole
  request with `400` when any plugin pack layer has a version that
  `ucmeta.IsLatest` matches. It copies nothing.), and the `/api/v1/inventory`
  JSON consumed by the operator's gap reconciler and the BFF proxy.
  `POST /api/v1/prefetch` (Bearer import token, 409 when pull-through is off)
  fetches posted plugins through the `/download` path and shares its `dlGroup` key.
  `ResolveAvailable` resolves `latest` to the highest version a healthy metadata
  source lists. `/download` resolves `latest` from the served metadata first.
  The upload planner checks a `latest`-only declaration against the version it
  resolves to, and `DeclaredSet.Highest` ignores unpinned versions. The planner
  and the inventory take a dependency list in one order: the layer annotation
  (an annotation of `[]` is a known empty list), then the stored manifest
  (`storedDependencies`), then upstream metadata (planner only). A missing
  annotation never means "no dependencies". `/api/v1/inventory` sets
  `dependenciesKnown` on every entry. A consumer reads `dependencies` only when
  it is true. One inventory request waits at most `inventoryDepsBudget` (15 s)
  for stored-manifest reads, below the operator's 30 s inventory client
  timeout. A digest unresolved at the budget is reported as unknown. A read
  already started fills the dependency cache for the next request, and the
  feed of reads not yet started stops at the budget.
  A metadata request with `?version=<core>` lists the highest stored version
  that core runs, and `ResolveLatestForCore` supplies a compatible upstream entry
  for a declared `latest` plugin (`corecompat.go`).
  The servability route resolves a `latest` entry that carries `core` through
  `ResolveLatestForCore`. A pinned entry ignores `core`.
  `POST /api/v1/plugins` (`upload.go` + `closure.go`) is the **push** supply
  tier: PARSE → declared-set precondition → D4 admission → PLAN → VALIDATE →
  COMMIT, writing nothing until every mandatory dependency resolves. It requires
  `VARROA_UC_SINGLE_WRITER` — an upload is a read-modify-write against a store
  with no conditional-push primitive, so a second writer cannot be excluded by
  any in-process lock. `/api/v1/inventory` additively carries the five
  `dev.varroa.plugin.*` metadata fields, dedupes on the **whole canonical
  entry** rather than `(name, version)` — `ListManifests` answers
  reference-name order on both backends (the layout store sorts; a registry
  lists tags sorted), so the first pack for a `(name, version)` is stable — and **degrades gracefully
  on an incomplete store scan**: it serves every readable pack and discloses
  each unreadable one by ref+error in the response's `skippedPacks` array
  (empty/`omitempty` when nothing was skipped), only failing closed with 503
  when the scan found **no readable pack at all**. `listPackInfos` returns
  those per-ref failures (`[]skippedPackInfo`), not just a count; the metadata
  and download routes discard the second value and keep serving partial
  results as before. A consumer that **prunes** against `/api/v1/inventory`
  (the operator's derived-catalog sync) MUST treat a non-empty `skippedPacks`
  as a reason to withhold pruning for that pass — a partial listing is a lower
  bound on store contents, not proof the missing plugins are gone; a
  non-pruning consumer (BFF display, gap analysis) may serve/log the partial
  result directly. It writes one **addon pack per plugin** (the uploaded
  plugin last, at `upload-<hex12>`), because an addon pack is exactly one plugin
  with an empty profile. `ucmeta` additionally exposes outcome-typed
  `ResolveExact`/`ResolveSatisfying`: a provable negative (`NotListed`, 422) and
  an unprovable one (`SourcesDegraded`, retryable 503) must not be conflated,
  and a healthy source's answer always beats another source's degradation.
  `ResolveAvailable` is the single serving entry point: pull-through
  (`ResolveSHA256` delegates to it), the `POST /api/v1/servability` endpoint,
  and the upload planner's declared-pin check all call it. Do not add a second
  resolution path. `POST /api/v1/servability` is unauthenticated, bounded to
  1 MiB and 2000 entries, and runs 8 lookups at once. The operator's
  `PluginsServable` condition calls it for plugins absent from the store.
  The declared plugin set arrives as a mounted file
  (`VARROA_UC_DECLARED_PLUGINS_FILE`, re-read per request) because the service
  has no Kubernetes client; an UNREADABLE file rejects an upload rather than
  being treated as "nothing is declared". Served-metadata version precedence is
  total: declared-version eligibility, then highest by `pluginver`, then lowest
  layer sha256 — never `ListManifests` order, which is a reference-name order
  and not a version order.
- **`jenkinsver/`** — parses/compares Jenkins version strings from container
  tags (e.g. `2.479.3-jdk17`); backs `JenkinsVersionProfile` resolution
  (exact → LTS-line → embedded baseline). **Core tags only.** It cuts at the
  first `-` and requires numeric segments, so a plugin version such as
  `4.5.14-269.vfa_2321039a_83` silently truncates. Do NOT extend it to plugin
  versions — that is `pluginver`'s job.
- **`plugininv/`** — pure controller plugin inventory collection, canonical
  hashing, and closure-aware provenance classification. No imports from
  `internal/mite`, `internal/controller`, or `internal/api`. Consumers:
  `cmd/mite` (collector), `internal/controller` (classifier).
- **`pluginver/`** — orders Jenkins **plugin** versions: a structure-for-structure
  port of `hudson.util.VersionNumber` (Maven `ComparableVersion` with Jenkins'
  qualifier table, where `snapshot` sits below `alpha`). It is what makes a
  `Plugin-Dependencies` entry a **minimum** rather than a pin. The API is
  **total** — `Compare(a, b string) int` and `AtLeast(have, want string) bool`,
  every string parses, no error and no `ok`. Do not add a parse-failure return.
  Deliberately separate from `jenkinsver`: merging them would truncate plugin
  versions at the first `-` and cannot express `1-1 < 1.1`. Upstream's ordering
  is antisymmetric but NOT transitive across degenerate qualifier forms; the
  port reproduces that rather than "correcting" it, because a corrected ordering
  would disagree with what Jenkins resolves. Consumers: `internal/updatecenter`
  (closure planner, served-metadata precedence), `ucmeta`.
- **`pluginrange/`** — parses and matches version-range expressions against
  installed plugin versions (e.g. `<=4.0.0`, `>=4.0.0,<4.1.0`). Consumes
  `internal/pluginver` only; never extends `internal/jenkinsver`. The grammar
  is comma-separated AND clauses with longest-prefix-first operators. There is
  no `unknown` match verdict — `pluginver` is total. Allocated to T2.2 by R24;
  T3.2's approved-plugin set must consume this package, not fork it.
  Consumers: `internal/api` (fleet plugin queries).
- **`hpi/`** — parses `META-INF/MANIFEST.MF` out of a Jenkins `.hpi`
  (`Short-Name`, `Plugin-Version`, `Long-Name`, `Jenkins-Version`,
  `Plugin-Dependencies`). Implements the JAR line format: continuations are
  unfolded **before** any key split, because writers wrap at 72 bytes and a
  real `Plugin-Dependencies` value is always folded. Contains **no version
  comparison at all** — `Dependency.Min` is a minimum recorded verbatim as an
  opaque string, never normalized or decomposed — comparison is `pluginver`'s.
  Consumers: `internal/oci` (layer annotations), `cmd/bootstrapdeps` (closure
  walk), `internal/updatecenter` (upload PARSE).
- **`pluginresolve/`** — pure plugin-dependency-closure library, no
  filesystem/network I/O of its own: `Resolve` walks a worklist of
  `(name, minVersion)` requirements against a caller-supplied `MetadataSource`
  (`UpstreamSource` wraps `ucmeta.Resolver`; `InClusterSource` walks an
  `oci.BlobStore`'s plugin packs with a version/completeness/manifest-digest
  tie-break), raising minimums with `pluginver.AtLeast` — never `jenkinsver`,
  which is reserved for the separate `checkCoreFloor`/`AssertCoreFloor` gate
  against a plugin's/the root HPI's `RequiredCore` — capped at
  `maxResolveDepth = 32`. Also holds the bootstrap-closure logic moved from
  `cmd/bootstrapdeps` (`ResolveClosure`, `AssertBootstrapClosure`,
  `HTTPFetcher`), presence-only with no version comparison.
- **`ca/`** — internal certificate authority (ed25519 + x509) issuing mTLS
  certs for mite↔gateway `CommandStream` connections; bootstrap HMAC token
  verification also lives here.
- **`profileview/`** — small DTO helpers for `JenkinsVersionProfile` views,
  e.g. `PluginLinesFromYAML` extracting pinned `artifactId[@version]` lines
  from a materialized `plugins.yaml` for display.
- **`logging/`** — thin `log/slog` wrapper (`logging.New`) standardizing
  level/format (`json`/`text`) across all three binaries.
- **`hibernation/`** — shared controller-hibernation contracts: webhook replay
  constants, the BFF/operator wake interstitial renderer, and the operator's
  informer-backed `wakeserver` HTTP handler.
- **`forge/`** — operator-only client for a repository forge (GitHub REST and Gitea API). Stdlib only. `NewHTTPClient` refuses non-global addresses and redirects. `Token` never renders its value. A 403 with `Retry-After`, `X-RateLimit-Remaining: 0`, or a body that names a rate limit, and every 429, map to `RateLimitError`, never `ErrForbidden`. `forge/forgeapi/` holds the stdlib wire types of the operator forge verbs.
- **`forgeurl/`** — leaf rules for the forge path: repository URL normalization, branch names, revisions, file paths, file sets, titles, bodies, relayed bodies, and commit messages. Stdlib only.
- **`secretredact/`** — replaces secret values in JCasC (`JCasC`) and items
  (`Items`) YAML with `<redacted>`. A key that contains `password`, `secret`,
  `passphrase`, `privatekey`, or `token`, and does not end in `id`, marks a
  secret, at every depth below it. Inside a `domainCredentials` or `folderCredentials` store, every
  scalar is a secret except the fields in `storeVisibleKeys`, because a
  credential type can hold its secret under any key name (`secretId` too).
  Only scalars change. `JCasC` keeps a value that is one whole `${...}`
  reference to a named entry, a file, or a system property; an inline-data
  lookup (`base64:`, `decodeBase64:`, `json:`, `trim:`) or a `:-` default is
  redacted. `Items` keeps no reference, because items never pass through
  JCasC. It removes every comment, because a comment can hold a secret. Every
  document is encoded again, so both sides of a diff share one style. Text
  that does not parse becomes `<redacted: unparseable content>`. The mite's
  content answer and the BFF diff read both use it.

## Verification

```bash
make test                                          # all Go tests, race detector
make lint                                          # golangci-lint v2
go test -race -count=1 ./internal/<pkg>/...        # single package
```

## Child DOX Index

- [controller](controller/AGENTS.md) — controller reconciler: CR lifecycle, provisioning, StatefulSet/RBAC/ingress, version resolution, pipelines-tenant cell placement and warm pool (`cells/`), external-runner status and node/credential/egress rendering (`runners/`)
- [api](api/AGENTS.md) — BFF HTTP/SSE handlers and REST API surface
- [mite](mite/AGENTS.md) — gateway gRPC server bridging mite sidecars to NATS/operator; owns the build-event/claim bridge (`build_events.go`, `claim_bridge.go`)
- [bundle](bundle/AGENTS.md) — ComposedBundle composition, git/catalog resolution, JCasC merge, and the delivered pipeline-template set (`pipelinetemplate/`)
- [rbac](rbac/AGENTS.md) — VarroaRole/JenkinsRole resolution and role-strategy YAML generation
- [bus](bus/AGENTS.md) — NATS/JetStream transport: subjects, streams, KV buckets, cluster directory, subscription-denial watchers, the self-healing request responder and listener, the `bustest` deny server
- [agent](agent/AGENTS.md) — assistant and team-agent core: session, event, approval, and settings types; the tool policy, data-sharing, budget, and prompt packages; the session store (`store/`); the model-provider contract and adapters (`provider/`); the sandbox contract (`sandbox/v1`).
- [teamagent](teamagent/AGENTS.md) — Team agent core: `accept/` (the one rule that maps a `TeamAgent` to its principal), `token/` (the HS256 session credential, redacted), `instructions/` (the standing-instruction read through the forge relay), and the runner's `cache/` (TeamAgent objects from informers), `config/`, `store/` (record-store queries), `triggers/` (the audit-feed path), and `sessions/` (the session runtime).
- `keylock/` — a set of mutexes, one for each key, with a context-bound wait. An entry lives only while a caller holds or waits for its key. It has no child doc. The controller tick lock and the forge pull-request Team lock use it.
- `forgerelay/` — bus framing of the forge `check`, `read`, `pullrequest`, and `issue` requests. It has no child doc. Requesters call it and the operator shard answers. `forgeops` answers `pullrequest` one at a time for each Team on a replica, and `pullrequest` and `issue` under the deadline that the request carries.
- [recordstore](recordstore/AGENTS.md) — build-record Postgres store: schema, migrations, ledger, the placement fence, the cross-tenant grant evaluator and responder, the upstream fan-out, the fleet lock table, the tenant-sync responder, the backstop scheduler, blob abstraction; the `runs` row carries the `source`, `summary`, `inputs`, and `replay_of` columns
