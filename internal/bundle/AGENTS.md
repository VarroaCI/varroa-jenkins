# AGENTS.md — internal/bundle

## Purpose

Implements the unified `ComposedBundle` model: composing a `ComposedBundleSpec`
(ordered `inputs[]`, each an `itemRef`/`gitSource` union) into merged,
still-**unresolved** content — seven keys: `jenkins.yaml`, `plugins.yaml`,
`items.yaml`, `rbac.yaml`, `variables.yaml`, `pipeline-templates.yaml`, and
`dropped-item-keys.yaml` (the top-level item keys the composer drops, from
`DroppedItemKeys`; never part of the resolved hash) — plus bundle.yaml
manifest parsing, git materialization with an on-disk clone cache, catalog
index parsing, and pre-apply content validation.

## Ownership

- Owns: composition/merge (`composer.go`), git clone/materialize
  (`resolver.go`, `git.go`, `clonecache.go`), `bundle.yaml` manifest + jcasc
  file merging (`manifest.go`), catalog index/content validation
  (`catalog.go`, `catalog_validate.go`), the `${var}` substitution primitive
  `ResolveVars` + canonical `InjectedVariableNames` (`resolver.go`),
  unresolved-variable scanning (`unresolved.go`), the tenant `variables.yaml`
  parse, single-pass replacer, and strict tenant items parse (`tenantvars.go`:
  `ParseVariables`, `VarsReplacer`, `RemainingVarRefs`, `ParseTenantItems`,
  `SplitDroppedItemKeys`, `TenantItemsManifest`, `LintTenantTemplates`, shared by
  the cell and the pipelines read), `unclassified.location.url`
  injection (`location.go`), the `jenkins.updateCenter.sites` injection
  (`updatesite.go`), structural/content validation (`validator.go`),
  the pipeline-template set (`pipelinetemplates.go`'s
  `InjectPipelineTemplates`, package `pipelinetemplate/`), and tenant-cloud
  injection for a cell (`tenant_clouds.go`).
- Does not own: the `ComposedBundle` CRD reconcile loop, owner-referenced
  ConfigMap persistence (`status.contentRef`), or the completeness check —
  those live in `internal/controller/` (`catalog_controller.go` for
  `ReconcileComposedBundle`; `controller_controller.go` for the per-controller
  hot path calling `ResolveVars`/`MergeJenkinsYAML`/`InjectLocationURL`/`InjectUpdateSites`). This
  package produces content; it does not decide when/where to apply it.

## Local Contracts

- **`Composer.Compose`** (`composer.go`) — validates each input is exactly
  one of `ItemRef`/`GitSource`/`OCISource`, then per input in list order.
  `composeItems` keeps only `items` from each item fragment. It reports each
  other top-level key, with its source, as a `Warnings` entry:
  - `itemRef`: resolved via `ItemLookup.GetCatalogItemCRD`, **local-first with
    operator-namespace fallback** when `ref.Namespace` is unset (explicit
    namespace = exact lookup, no fallback, no shadow warning). A local hit
    shadowed by a same-named operator-namespace item emits a `Warnings` entry
    (never on a fallback hit). Drift recorded in `result.Drifted` when
    `ref.PinnedContentHash != item.Status.ContentHash`. `pipeline-template`
    does not route into any `groups` entry: its content is one
    `pipelinetemplate.Document`, parsed and appended to a template list kept
    outside `groups`, deduplicated by `name@version` — two refs at the same
    `name@version` with differing content is a compose error naming both
    refs. A `jcasc` item may embed a top-level `plugins:` block
    (`splitEmbeddedPlugins`) so config ships with its dependent plugin.
  - `gitSource`: requires `c.resolver != nil`; caller pre-resolves `GitAuth`
    into `resolvedAuth[i]` (Secrets are never read here). Each input clones
    into its own `os.MkdirTemp` under `c.workDir`, removed via `defer` after
    materialization.
  - `ociSource`: requires `c.resolver != nil`; caller pre-resolves `OCIAuth`
    into `resolvedOCIAuth[i]`. Each input pulls via `MaterializeOCI` into its
    own `os.MkdirTemp` under `c.workDir`, removed via `defer` after
    materialization. Merges identically to `gitSource`.
  - Variable precedence (low→high): item `spec.variables[].default` →
    `spec.Variables` → `ref.Variables` (per-ref always wins).
  - jcasc merge strategy defaults `errorOnConflict`; pod templates inject
    **after** the jcasc merge (`injectPodTemplatesIntoJCasC`) because
    `jenkins.clouds` is a list and `mergeMaps` replaces lists wholesale rather
    than deep-merging.
  - Output `ComposeResult{Materialized, ResolvedHash, BundleYAML, Missing,
    Drifted, Warnings, Errors}`. `Materialized.PipelineTemplatesYAML` holds
    the collected `pipeline-template` list, serialized by
    `composePipelineTemplates` (`pipelinetemplates.go`) as `templates: [...]`
    — one `pipelinetemplate.Document` per entry. `ResolvedHash` = SHA-256
    over unresolved content (jenkins, plugins, items, rbac,
    `PipelineTemplatesYAML`) + sorted vars (`computeResolvedHash`) — the
    content-hash used to detect itemRef catalog drift and trigger recompose.
    `varroa_*` auto-vars are deliberately **not** injected here.
    `validateItemTemplateValues` cross-checks every `items.yaml` job whose
    `definition.varroaTemplate` names a delivered template: it resolves the
    named template by version constraint (`pipelinetemplate.ResolveTemplate`)
    and validates the job's declared `values` against that template's
    parameter schema, appending a compose error per failure.
- **`MergeJenkinsYAML(base, overlay)`** (`composer.go`) — merges a
  `JenkinsVersionProfile` JCasC overlay onto composed `jenkins.yaml`; overlay
  wins on scalar conflict, maps deep-merge. Empty side short-circuits.
- **`InjectTenantClouds(jcascYAML, clouds)`** (`tenant_clouds.go`) — appends
  one kubernetes cloud per placed tenant to `jenkins.clouds`, for a cell.
  Modelled on `injectPodTemplatesIntoJCasC`: it edits the parsed document
  rather than merging a separate document via `mergeMaps`, because
  `mergeMaps` replaces a list instead of deep-merging it, which would drop
  every cloud the bundle itself declares. It never replaces an existing
  cloud and rejects a name collision, against an existing cloud or between
  two tenant clouds in the same call. A `TenantCloudTemplate` carries typed
  keys and an optional `Raw` map, the pass-through keys of a pod template
  lifted from a tenant bundle. The renderer writes `Raw` first and the typed
  `name`, `namespace`, `serviceAccount`, and `label` after, so a typed key
  always wins. It writes typed `containers` only when that field is not
  empty. A `TenantCloud.GarbageCollectionTimeout` above zero writes
  `garbageCollection: {timeout: <n>}` with no `namespaces` key, so the
  kubernetes plugin garbage-collects orphan pods in the cloud's own namespace
  only. Zero writes no key; the caller owns the value.
- **`ParsePipelineTemplates(templatesYAML)` / `RenderPipelineTemplates(docs)`**
  (`pipelinetemplates.go`) — parse a `pipeline-templates.yaml` body into its
  documents (blank gives nil) and write documents back in the same shape (empty gives "").
  The controller lints a tenant fragment with them.
- **`InjectPipelineTemplates(jenkinsYAML, templatesYAML, folder)`**
  (`pipelinetemplates.go`) — appends every document in `templatesYAML` to
  `unclassified.varroaPipelineTemplates.templates` in `jenkinsYAML`, flattening
  each document's `parameters`/`uiSchema`/`jenkinsfile` to strings so JCasC
  never interprets a template's own YAML (design D7). It never replaces an
  existing entry, so the controller's own bundle and each placed tenant's
  fragment (`MaterializedBundle.CellTemplateFragments`, one entry per tenant,
  `folder` stamped by the operator) can each call it once, in turn. Call this
  only after `ResolveVars` and the unresolved-variable check — a template body
  holds its own `${...}` Groovy text that is not a catalog variable and must
  never pass through either step. A blank `templatesYAML` is a no-op.
- **`pipelinetemplate/`** — the PipelineTemplate document, its JSON Schema
  subset, and the marker file that selects one, all mirrored bit-for-bit by
  the Jenkins plugin (`plugin/.../template/VarroaMarkerValidator.java`) and
  the frontend (`frontend/src/components/specform/templateValidator.ts`)
  against one shared golden corpus (`testdata/schema-corpus/`, read by
  `corpus_test.go`; the plugin mirrors the same fixtures under its own
  `src/test/resources/` because the Maven build cannot see the Go module
  tree). `doc.go` defines `Document`/`Metadata`/`Spec`/`SCMDefinition` with
  strict YAML decoding (unknown key = error). `schema.go` is the JSON Schema
  subset checker (keyword allowlist, root rules, depth/property-count/enum/
  size limits); `pattern.go` is the regex-pattern whitelist scanner
  (`NormalizePattern` rewrites an unescaped `$` outside a character class to
  `\z`, so `$` means end-of-string under Go's RE2 the same way it does under
  the plugin's and the browser's engines); `uischema.go` checks a fixed
  uiSchema subset against the schema's own properties. `version.go` parses
  and compares strict `MAJOR.MINOR.PATCH` and evaluates the four constraint
  forms a marker's `templateVersion` may use. `marker.go` parses
  `.varroa/pipeline.yaml` (`MarkerPath`, fixed, no override) and resolves it
  against a delivered template list. `values.go`'s `ValidateValues` applies
  schema defaults then validates against the schema; it never opens a schema
  or marker file itself — callers own I/O. This package composes documents,
  it does not decide which items or markers use them; that lives in
  `composer.go` (compose time) and `internal/controller`/`internal/jenkins/items`
  (run time). Documented inline here rather than in its own AGENTS.md:
  it has no CRD, no reconcile loop, and no consumer-facing HTTP surface of
  its own — every entry point is a pure Go function this package's own
  callers document at their call sites.
- **`Resolver`** (`resolver.go`) — `Materialize` (git path: clone →
  `materializeDir`) returns a `MaterializedBundle` with `${var}` placeholders
  **retained**, reusable across controllers. `MaterializeOCI` (OCI path:
  `Pull` → `FetchBlob` → untar → `materializeDir`) is the OCI sibling that
  shares the same parse/validate/merge pipeline via `materializeDir`.
  `materializeDir` is the shared seam (`ParseManifest` → `Validator.Validate`
  → merge jcasc → read plugins/items/rbac → parse variable files).
  `MaterializedBundle.PipelineTemplatesYAML` carries the controller's own
  delivered template set; `CellTemplateFragments` carries one entry per
  placed tenant, for a cell — neither is ever subject to `ResolveVars`.
  `CellFolderCredentials` carries each placed tenant's projected folder
  credentials, for a cell. It holds Secret values, so it is never subject to
  `ResolveVars` and no ConfigMap write reads it.
- **`ResolveVars(content, vars)`** — plain `${name}` `strings.ReplaceAll`, no
  templating engine. `InjectedVariableNames` (resolver.go:288) is the
  authoritative list of injectable `varroa_*` names — keep in lockstep with
  injection call sites in `internal/controller/controller_controller.go`.
- **`CloneCache`** (`clonecache.go`) — per-replica on-disk cache of bare repos
  under `<root>/repos/<sha256(normalizedURL)>.git` + `.meta.json`
  (lastFetch/lastUsed/size drive LRU eviction bounded by `maxRepos`/
  `maxSizeMiB`). `Checkout` resolves the SHA via `ls-remote` (skipped if
  already a 40-hex SHA), locks per cache key, fetches on miss, then runs
  `archiveCommit` (`gitarchive.go`). It streams `git archive --format=tar <sha>`
  from the bare store into `extractTar`. The target gets the tree files and no
  `.git` directory. `extractTar` writes through `os.Root`. It rejects a
  non-local name, an entry below a symlink, a symlink that leaves the target,
  and any type other than directory, regular file or symlink. A failure removes
  what it wrote. `export-ignore` and `export-subst` apply on this path. The
  `refs/varroa/*` refs stay in the bare store; `fetchBareRef` and the re-resolve
  in `fetchCommit` still use them. A nil `*CloneCache` = disabled. Then
  `Resolver.Materialize` calls `GitCloner.Clone`, which fetches into a temporary
  bare repository with the same `fetchCommit`, archives with `archiveCommit`,
  removes the bare repository, and returns the SHA. Export attributes apply on
  both paths. The per-key lock is a one-slot channel, not a mutex: a waiter
  leaves when its context is done, so a compose never inherits another caller's
  stuck fetch.
  - `RemoteSHA` asks ls-remote for `<ref>` and `<ref>^{}` and prefers the peeled line.
  - `peelCommit` peels the SHA to its commit on the hit check and in `fetchCommit`. `Checkout` and `Clone` therefore always return a commit SHA.
  - The composer records a tag object SHA in `observedRevisions` for a ComposedBundle input pinned to one. That is the accepted gap.
  - A pinned-SHA fetch passes `--no-tags`.
- **`GitCloner`** (`git.go`) — `validateRepoURL` is the RCE defense: only
  `https://`, `ssh://`, scp-like `[user@]host:path`; any `"<transport>::"`
  form and `file://` (except test-only `AllowLocalTransportForTest`) rejected
  before git sees the URL, plus `GIT_ALLOW_PROTOCOL` as defense-in-depth.
  `GitAuth` supports basic (askpass script, avoids argv leak) or SSH (temp
  key, mode 0600, optional pinned `known_hosts`). `redactURL` strips creds
  before logging. `remoteGitEnv` is the single builder of the auth plus
  `GIT_TERMINAL_PROMPT`/`GIT_ALLOW_PROTOCOL` env for every remote git call.
- **Git deadlines** — every git process in this package starts through
  `gitCommand(ctx, ...)` (`gitcmd_unix.go`). It runs git in its own process
  group, kills the whole group when ctx is done, and sets a 5s `WaitDelay`.
  `exec.CommandContext` alone is not sufficient: it kills only `git`, and
  the `git-remote-https`/`ssh` child keeps the stderr pipe and the connection
  open. `Clone`, `RemoteSHA`, and `CloneCache.Checkout` take a context, and a
  failure that ctx causes wraps `ctx.Err()`. `gitcmd_other.go` (non-Unix) has
  no group kill. Never add a bare `exec.Command("git", ...)` here.
- **Manifest/validation** — `ParseManifest` requires `bundle.yaml` with
  `id`/`version`/`apiVersion` (`"1"`/`"2"`) and ≥1 `jcasc` file; all
  referenced paths must exist and must not escape the bundle directory.
  `ParseManifest`/`ParseCatalogIndex`/`Materialize`/`MaterializeOCI` reject
  any declared path that is non-local (`filepath.IsLocal`) or that resolves
  through a symlink outside its base directory (`ResolveContainedPath`) —
  see the `validateRelPath`/`ResolveContainedPath` pair in `path.go`. This
  is the single canonical validator for path-join call sites; don't add a
  second copy. `ValidateContent` (validator.go:123) is the
  materialize-time floor: YAML parseability, plugin `artifactId`/`version`
  well-formedness (blank version = error, `latest` in any case or with spaces
  = warning, matched by `ucmeta.IsLatest`), unresolved
  `${var}` detection (skips `varroa_*` and `^${var}` JCasC escapes).
  `FindUnresolvedVariables` is the read-only sibling for callers wanting just
  the missing-var list. `CheckPluginPins` skips every pin that `ucmeta.IsLatest`
  matches.
- **`catalog_validate.go`** — per-`CatalogItem.Spec.Type` content validation,
  run at catalog sync time (`ValidateItemContent`, one case per type). The
  `pipeline-template` case (`validatePipelineTemplateContent`) parses the
  content as one `pipelinetemplate.Document` (design D2, L1 in D11) and
  rejects a declared `spec.variables` entry outright: `pipeline-templates.yaml`
  is never resolved by `${var}` substitution, so a variable declared on this
  item type can never reach its body.

## Work Guidance

- New input types on `ComposedBundleSpec.Inputs` must extend both the union
  check and the `switch` in `Compose` — no default case, so a forgotten branch
  silently drops content rather than erroring. The three current branches are
  `ItemRef`, `GitSource`, and `OCISource`; the union guard is a count-based
  `(has(self.itemRef)?1:0)+(has(self.gitSource)?1:0)+(has(self.ociSource)?1:0)==1`.
- `MaterializeOCI` (`resolver.go`) uses `oci.RegistryStore` + `WriteDockerConfigJSON`
  (both in `oci.go`) to create a temp docker config.json from `OCIAuth`; the caller
  must clean up the temp directory. `OCIAuthFromSecret` snaps `.dockerconfigjson`
  first, then falls back to `username`/`password` keys. A `.dockerconfigjson` with
  more than one `auths` entry is rejected outright (map iteration order is
  undefined, so picking one would be non-deterministic) — every consumer
  (`UpdateCenter.spec.seed.secretRef`, `CatalogSource`/`ComposedInput` OCI
  `secretRef`) requires a secret scoped to exactly one registry.
- `computeResolvedHash` argument order
  `(jenkins, plugins, items, rbac, pipelineTemplates, vars)` must stay
  stable — drift/recompose detection depends on it across releases.
- `NewResolver` and `NewCloneCache` take a `metric.MeterProvider` as their
  first parameter. They create `varroa.bundle.clone.duration` and the
  `varroa.bundle.git.cache.hits`/`misses` counters on its `"varroa-operator"`
  meter. A caller with no telemetry (`varroactl`, most tests) passes
  `noop.NewMeterProvider()`. Never bind an instrument to a package variable.
- Git URL validation lives solely in `validateRepoURL`; `GitCloner.Clone`,
  `GitCloner.RemoteSHA`, and `CloneCache.Checkout` all call the one function —
  don't add a second copy.

## Verification

```bash
go test -v -race -count=1 ./internal/bundle/...
make test
make lint
```

Key tests: `composer_test.go`/`composer_namespace_test.go` (merge strategy,
itemRef fallback/shadowing), `clonecache_test.go` (checkout tree, export
attributes, exec bit and symlinks, and cache-off `GitCloner.Clone` parity
with the cached path, revision forms resolve to the commit on both paths),
`gitarchive_test.go` (extractor rejects
unsafe entries; archive cleans its target), `git_test.go` (URL
validation/RCE guard), `git_deadline_test.go` (git stops at the context
deadline, against a silent 127.0.0.1 listener), `validator_test.go`, `unresolved_test.go`,
`location_test.go`, `bundle_test.go`, `pipelinetemplates_test.go`
(`InjectPipelineTemplates`), `pipelinetemplate/corpus_test.go` (the shared
golden corpus, `testdata/schema-corpus/`, that the plugin and the frontend
also read from their own test suites).
