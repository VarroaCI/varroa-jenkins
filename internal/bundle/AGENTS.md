# AGENTS.md — internal/bundle

## Purpose

Implements the unified `ComposedBundle` model: composing a `ComposedBundleSpec`
(ordered `inputs[]`, each an `itemRef`/`gitSource` union) into merged,
still-**unresolved** JCasC content — plus bundle.yaml manifest parsing, git
materialization with an on-disk clone cache, catalog index parsing, and
pre-apply content validation.

## Ownership

- Owns: composition/merge (`composer.go`), git clone/materialize
  (`resolver.go`, `git.go`, `clonecache.go`), `bundle.yaml` manifest + jcasc
  file merging (`manifest.go`), catalog index/content validation
  (`catalog.go`, `catalog_validate.go`), the `${var}` substitution primitive
  `ResolveVars` + canonical `InjectedVariableNames` (`resolver.go`),
  unresolved-variable scanning (`unresolved.go`), `unclassified.location.url`
  injection (`location.go`), and structural/content validation (`validator.go`).
- Does not own: the `ComposedBundle` CRD reconcile loop, owner-referenced
  ConfigMap persistence (`status.contentRef`), or the completeness check —
  those live in `internal/controller/` (`catalog_controller.go` for
  `ReconcileComposedBundle`; `controller_controller.go` for the per-controller
  hot path calling `ResolveVars`/`MergeJenkinsYAML`/`InjectLocationURL`). This
  package produces content; it does not decide when/where to apply it.

## Local Contracts

- **`Composer.Compose`** (`composer.go:74`) — validates each input is exactly
  one of `ItemRef`/`GitSource`/`OCISource`, then per input in list order:
  - `itemRef`: resolved via `ItemLookup.GetCatalogItemCRD`, **local-first with
    operator-namespace fallback** when `ref.Namespace` is unset (explicit
    namespace = exact lookup, no fallback, no shadow warning). A local hit
    shadowed by a same-named operator-namespace item emits a `Warnings` entry
    (never on a fallback hit). Drift recorded in `result.Drifted` when
    `ref.PinnedContentHash != item.Status.ContentHash`. `pipeline-template`
    routes into the `item` group. A `jcasc` item may embed a top-level
    `plugins:` block (`splitEmbeddedPlugins`) so config ships with its
    dependent plugin.
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
    Drifted, Warnings, Errors}`. `ResolvedHash` = SHA-256 over unresolved
    content + sorted vars (`computeResolvedHash`) — the content-hash used to
    detect itemRef catalog drift and trigger recompose. `varroa_*` auto-vars
    are deliberately **not** injected here.
- **`MergeJenkinsYAML(base, overlay)`** (`composer.go:351`) — merges a
  `JenkinsVersionProfile` JCasC overlay onto composed `jenkins.yaml`; overlay
  wins on scalar conflict, maps deep-merge. Empty side short-circuits.
- **`Resolver`** (`resolver.go`) — `Materialize` (git path: clone →
  `materializeDir`) returns a `MaterializedBundle` with `${var}` placeholders
  **retained**, reusable across controllers. `MaterializeOCI` (OCI path:
  `Pull` → `FetchBlob` → untar → `materializeDir`) is the OCI sibling that
  shares the same parse/validate/merge pipeline via `materializeDir`.
  `materializeDir` is the shared seam (`ParseManifest` → `Validator.Validate`
  → merge jcasc → read plugins/items/rbac → parse variable files).
- **`ResolveVars(content, vars)`** — plain `${name}` `strings.ReplaceAll`, no
  templating engine. `InjectedVariableNames` (resolver.go:288) is the
  authoritative list of injectable `varroa_*` names — keep in lockstep with
  injection call sites in `internal/controller/controller_controller.go`.
- **`CloneCache`** (`clonecache.go`) — per-replica on-disk cache of bare repos
  under `<root>/repos/<sha256(normalizedURL)>.git` + `.meta.json`
  (lastFetch/lastUsed/size drive LRU eviction bounded by `maxRepos`/
  `maxSizeMiB`). `Checkout` resolves the SHA via `ls-remote` (skipped if
  already a 40-hex SHA), locks per cache key, fetches on miss, then clones
  locally from the bare store into a private `refs/varroa/*` namespace — never
  `refs/heads/*` directly, since git refuses to fetch into a non-bare repo's
  checked-out (even unborn) branch. Nil `*CloneCache` = disabled;
  `Resolver.Materialize` falls back to `GitCloner.Clone` directly.
- **`GitCloner`** (`git.go`) — `validateRepoURL` is the RCE defense: only
  `https://`, `ssh://`, scp-like `[user@]host:path`; any `"<transport>::"`
  form and `file://` (except test-only `AllowLocalTransportForTest`) rejected
  before git sees the URL, plus `GIT_ALLOW_PROTOCOL` as defense-in-depth.
  `GitAuth` supports basic (askpass script, avoids argv leak) or SSH (temp
  key, mode 0600, optional pinned `known_hosts`). `redactURL` strips creds
  before logging.
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
  well-formedness (empty version = error, `"latest"` = warning), unresolved
  `${var}` detection (skips `varroa_*` and `^${var}` JCasC escapes).
  `FindUnresolvedVariables` is the read-only sibling for callers wanting just
  the missing-var list.

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
- `computeResolvedHash` argument order `(jenkins, plugins, items, rbac, vars)`
  must stay stable — drift/recompose detection depends on it across releases.
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
itemRef fallback/shadowing), `clonecache_test.go`, `git_test.go` (URL
validation/RCE guard), `validator_test.go`, `unresolved_test.go`,
`location_test.go`, `bundle_test.go`.
