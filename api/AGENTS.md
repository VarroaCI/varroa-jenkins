# AGENTS.md — api/

## Purpose

Varroa's wire/cluster contracts: the CRD Go types (`v1alpha1/`) the
operator/BFF reconcile against, and the OpenAPI 3 contract (`openapi/`) the
BFF server and every Go client are generated from. Changing either requires
regenerating downstream artifacts in the same change (greenfield/no-legacy).

## Ownership

- `api/v1alpha1/` — CRD types, group `varroa.dev/v1alpha1`.
- `api/openapi/` — split OpenAPI 3 spec (`paths/`, `components/`) + bundled output.

## Local Contracts

### CRD types (`api/v1alpha1/`)

- `types.go` `CellStatus.PendingSignatures` and `PendingSignaturesTotal` carry the refused script signatures of a cell (`CellPendingSignature`, `CellSignatureRequester`). The operator writes them from the mite snapshot; their DeepCopy is hand-written in `zz_generated.deepcopy.go`.
- `CellRehydrationEntry.Unseeded` (`[]string`, max 256) lists the jobs that wait for a
  number seed. Its DeepCopy is hand-written in `zz_generated.deepcopy.go`.
- `register.go` — `addKnownTypes` is the single registration list. Kinds:
  `Controller`, `VarroaRole`/`VarroaRoleBinding`, `JenkinsRole`/`JenkinsRoleBinding`,
  `User`, `Group`, `PodTemplate`, `CatalogSource`, `CatalogItem`,
  `ComposedBundle`, `ProvisioningDefaults`, `JenkinsVersionProfile`, `Team`,
  `BroodOperation`, `BroodSchedule`, `Runner`, `CellPool`. New Kind → add here + `zz_generated.deepcopy.go`.
  `BuildMetric` does not exist: build records live in the record store
  (`internal/recordstore`), not a CRD.
  `Runner` is cluster-scoped, like `Team`: it binds one external machine to one
  pipelines tenant (`RunnerSpec.Team`), immutable `team` and `launcher`
  (`ssh`/`inbound`), and six CEL `XValidation` rules. `RunnerSpec.Labels`
  carries `+kubebuilder:validation:MaxItems=64` and
  `+kubebuilder:validation:items:MaxLength=63` — a bound the label-token CEL
  rule needs to stay under the apiserver's estimated cost budget; without it
  the apiserver refuses the CRD outright. `RunnerStatus.Connected` carries no
  `omitempty`, the same rule `Event.Cluster` uses, so a merge patch can clear
  a `true` value back to `false`.
  `CellPool` is cluster-scoped too: it sizes one pool of cells for one
  `JenkinsVersionProfile`. Two CEL `XValidation` rules guard it — `profileRef`
  is immutable (a pool's cells carry the profile label they were created with,
  and the pass never restamps it), and `maxCells` is 0 or at least `warm`. The
  `libraries` list (`CellPoolLibrary`, max 16) holds trusted global libraries:
  an ssh remote needs `credentialRef`, a CEL rule on the item type. Each
  item has its own DeepCopy. The `approvedSignatures` list (`[]string`, max 256,
  each entry a script-security signature with no `${` token) is the pool's
  script approval set. A cell renders it; the BFF projects it read-only. The
  `mail` field (`CellPoolMail`, optional) holds the SMTP setting. Its login
  is a Secret name (`credentialRef`), never a value; the BFF projects the
  name only. The `giteaServers` list (`CellPoolGiteaServer`, max 16, map key
  `serverUrl`) declares the Gitea and Forgejo servers of every cell. It holds
  no credential, and the BFF projects it read-only.
  second rule guards **both** field reads with `has()`: a stored object written
  before the defaults existed leaves a field absent, and a bare access raises
  `no such key`. `CellPoolStatus`'s four counters (`warmCells`, `placedCells`,
  `unreadyCells`, `freeTenantSlots`) carry no `omitempty`, so a merge patch
  writes a count that falls to zero; `tenants` is `omitempty`, so
  `internal/crdstore/registry.go` clears it explicitly. `CellTemplate` reuses
  `ControllerSpec`'s own `resources`, `podOverrides`, and `miteSpec.resources` types
  (`miteResources`) and exposes no
  raw strategic-merge overlay, which is why the generated CRD is large.
- `types.go` — Spec/Status structs with kubebuilder markers. CEL
  `XValidation` rules embedding a regex with literal `\.` **must** use a raw
  CEL string (`r'...'`) — e.g. `types.go:346` (ingress annotation keys) and
  `types.go:370-372` (CPU/memory/storage quantities). Otherwise the
  apiserver rejects the CRD at apply time. `TestCRDManifestsPassAPIServerValidation`
  reports it offline.

  The `Controller` cell-role rule sits on `ControllerSpec`, not on `cell`: a field
  transition rule skips an add or a remove of the block. Both reads are `has()`-guarded.
  `CommandCRUD.HandleUpdate` mirrors the rule through `v1alpha1.IsCell`.

  `cell.go` `CellHoldsTenant` is the "cell holds tenant" read for the BFF pipelines
  read and the `bundles.teamcontent` reply. The cell's `heldTenants` scan
  (`cell_items_assembly.go`) repeats the same rule for the cell's own drain; change
  both together.

  The `Controller` ingress mode rule also sits on `ControllerSpec`, not on `ingressSpec`, for the same reason.
  `CommandCRUD.HandleUpdate` mirrors it through `IngressSpec.RoutingMode()`.

  `ControllerStatus` carries `PluginInventory` (`*PluginInventoryStatus`),
  a bounded summary of installed plugins with per-class counts, capped drift
  and version-drift lists, freshness/degradation flags, and an in-flight
  `PendingCollect` command record. The deepcopy methods for
  `PluginInventoryStatus`, `PluginInventoryDriftEntry`, and
  `PendingCollect` were added by hand — never run `controller-gen object`.
  New/edited fields in batch W2-B1b: `CatalogSourceSpec.OCIRef` (string),
  `ComposedInput.OCISource` (`*OCIBundleSource`), and the new
  `OCIBundleSource` type. DeepCopy was hand-added (no controller-gen).
  The six cell-pool types (`CellPool`, `CellPoolList`, `CellPoolSpec`,
  `CellPoolStatus`, `CellTemplate`, `CellPoolLibrary`, `CellPoolMail`) carry hand-added DeepCopy for the same
  reason.
  `CatalogSource` validation is split across **two** levels: the spec-level
  rule is at-most-one of `repoURL`/`ociRef`, and an **object-level** rule binds
  the zero-source-field shape to the reserved name `varroa-update-center`
  (the update-center-backed variant). Both branches guard on `has(self.spec)` —
  `spec` is optional in the schema, so an object created with no spec block at
  all reaches the rule and a bare `has(self.spec.repoURL)` raises "no such key:
  spec" instead of the intended message. CEL at the root schema can read
  `metadata.name`/`metadata.generateName` but **never** `metadata.namespace`,
  so "reserved name only in the operator namespace" is a runtime guard in
  `CatalogReconciler`, not a validation guard. The reserved name itself is
  `UpdateCenterCatalogSourceName`; every Go site aliases that constant, but
  the CEL rule spells the literal out because a rule cannot reference a Go
  constant, so the two must be changed together.
  `VarroaRoleBinding` has a status subresource that only the core BFF
  federation pass writes: `status.observedGeneration` plus
  `status.federation`, a list map keyed by `cluster` with `state`
  (`federated`/`skipped`/`failed`), `reason`, `message`, and
  `lastTransitionTime`. `federation` is `omitempty`, so
  `internal/crdstore/registry.go` clears it explicitly.
  `VarroaRole` has the same status shape (`VarroaRoleStatus`, element type
  `BindingFederationStatus`), also written only by the core BFF federation
  pass and cleared the same way. Its `federation` list holds an entry only
  for a hive with a failed stale-role delete.
  `CatalogItemStatus` carries three `omitempty` list fields — `closure`,
  `compat`, `conditions` — which means `internal/crdstore/registry.go` must
  clear each explicitly (`nil`, not `""`) or a merge patch leaves stale entries
  in etcd forever.
- `UserSpec.Purpose` (`service`/`break-glass`) is optional in the schema: the
  CRD cannot see the release mode, so the BFF enforces "a local account beside
  OIDC needs a purpose" at sign-in and on user writes.
- `validation.go` — hand-written Go validation (`ValidateJenkinsRole`, …)
  supplementing CEL. `routing.go` — ingress routing-mode helpers shared by
  operator+BFF, including `ExternalURL`, the one builder of a controller's external URL. `bundleref.go` — `EffectiveBundleRef`/`StarterBundleName`, the
  one answer to "which ComposedBundle is this Controller using?"; it lives here
  precisely because the operator, the BFF, and brood filtering all need it.
  `labels.go`/`team_consts.go` — label/annotation/condition string constants;
  add new ones here, not as literals at call sites.
- Policy decisions that both the operator enforces and the BFF pre-checks are
  methods on the policy type itself (`BroodPolicy.ExecuteGroovyAllowed`), not
  duplicated logic on each side. Two copies of a gate eventually disagree, and
  the disagreement is invisible until someone bypasses the BFF. Every such
  method must be nil-receiver safe: the CRDs carrying them are optional, and
  absence must mean the permissive default.
  `TeamPipelinesSpec.Retention` (`*TeamPipelinesRetention`) sets the
  record-store run windows: a tenant `runs` window and up to 64 `jobs`
  rules (`TeamJobRetention`). Only an operator may write it.
  `TeamPipelinesSpec.Exports` (`[]TeamExport`, at most 64 entries) lists the
  grants a pipelines tenant makes to other tenants: each entry names exactly
  one of `job` (a folder-relative job glob) or `locks` (bare
  lock-name globs), plus `callers` (at least one Team name). The CEL rules on
  `TeamExport` enforce the exactly-one-of, the non-empty job/locks/caller
  entries, and that `job` never starts with `/`; the matching OpenAPI schema
  (`TeamExport` in `components/schemas.yaml`, referenced from
  `TeamPipelines.exports`) mirrors the same shape and requires `callers`.
- CEL rule tests (`crd_cel_test.go`) load each generated CRD from
  `charts/varroa/crds/` and run its `x-kubernetes-validations` through the
  apiserver validator (`k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel`).
  Every `XValidation` rule needs at least one `accept` and one `reject` case in
  `testdata/cel/<crd file>`. `TestCRDValidationRuleCoverage` fails a rule with a
  missing case. `schemaShadowed` lists the only rules exempt from a reject case:
  the schema refuses the value first. Add an entry only with a reason.
  A rule that reads an optional field without `has()` fails with `evaluating rule`.
- `Profile` and `AgentNamespace` in `TeamPipelinesSpec`, and `Callers` in
  `TeamExport`, are required in the CRD schema (`+kubebuilder:validation:Required`).
  A pipelines block names a profile and an agent namespace. An export names callers.
  The OpenAPI `TeamPipelines` schema requires both, with `minLength: 1`, and `TeamExport`
  requires `callers`.
- `zz_generated.deepcopy.go` — header claims controller-gen, but **never run
  `controller-gen object`** — nukes this file on this type set. Hand-add
  `DeepCopyInto`/`DeepCopy` for new types/fields.
- CRD manifests: `make generate-crds` runs `controller-gen` in Docker, writes
  `charts/varroa/crds/*.yaml`. `make check-crds` regenerates and fails on
  diff (CI gate) — run after any kubebuilder-marker change.

### OpenAPI contract (`api/openapi/`)

- `varroa.root.yaml` — hand-edited entrypoint (info/paths/schemas index) —
  edit this to add/change an endpoint. Path files live in `paths/` (one per
  resource: `controllers.yaml`, `fleetplugins.yaml`, `updatecenter.yaml`, …).
- `varroa.json`/`varroa.yaml` — bundled canonical output, **generated by
  `hack/openapi-bundle`**, never hand-edited.
- `oapi-codegen.yaml` — codegen config, output `pkg/client/gen.go`; excludes
  SSE/MCP/docs operation IDs (handled by hand-written `pkg/client/stream`).
- `embed.go` — embeds `varroa.json` (`/api/v1/openapi.json`) and
  `docs/index.html`+`docs/rapidoc-min.js` (`/api/v1/docs`, vendored — see
  `docs/VENDORED.md`).
- `make generate-client`: bundles via `hack/openapi-bundle` → `oapi-codegen`
  → `gofmt pkg/client`. `make check-client` requires a clean tree first, then
  regenerates and diffs — spec and generated client are committed together.
- Endpoint changes flow: `paths/`+`components/schemas.yaml` → `internal/api`
  (BFF) → `pkg/client` (regenerated) → frontend. No dual-shape responses.
  `ControllerDetail` carries `pluginInventory` (bounded summary from
  `Controller.status.pluginInventory`). `GET /clusters/{cluster}/controllers/
  {ns}/{name}/plugins` serves the full classified inventory from the read
  model, cross-checked against the status.

- `Team.spec.pipelines.concurrencyLimits` and `CellPool.spec.maxRunsPerTenant` are the concurrency cap fields. A tenant pipelines admin may write `concurrencyLimits` only. The cell pool field is operator-only.

## Work Guidance

- Edit `varroa.root.yaml`/`paths/`/`components/`, run `make generate-client`
  — never hand-edit generated bundle/client files.
- Edit `types.go`, run `make generate-crds`, hand-update deepcopy.
- CEL regex with `\.`: use `r'...'`.
- A new or changed `XValidation` rule needs an accept and a reject case in
  `testdata/cel/<crd file>`. Guard each optional read with `has()`.

## Verification

- `go test ./api/...` runs the CEL fixture tests and the apiserver manifest check.
- `make check-crds`
- `make check-client`
