# CasC Catalog (Varroa-style)

<!-- sources: api/v1alpha1/types.go (CatalogSource, CatalogItem, CatalogVariable), internal/bundle/catalog.go, internal/bundle/composer.go (splitEmbeddedPlugins), internal/controller/catalog_controller.go -->

The catalog is Varroa's native way to publish **reusable, parameterized configuration snippets** — a shared theme, a standard pod template, a compliance JCasC fragment — that teams compose into their bundles by reference instead of copy-pasting. Where a [bundle source](bundle-sources.md) is one team's complete configuration, a catalog is a menu.

## Concepts

- A `CatalogSource` points at a git repository or OCI artifact; the operator syncs it on an interval and materializes each entry as a **`CatalogItem`**.
- `CatalogItem` objects are **operator-owned and read-only for users** — you author in the git repo, never with `kubectl apply` on items. Each item's status carries the verbatim content and a `contentHash` at sync time.
- Items are typed: `jcasc` (jenkins.yaml fragment), `plugin` (plugins.yaml entries), `item` (jobs/folders), `rbac` (role definitions), `podtemplate` (agent pod templates), `pipeline-template` (a reusable pipeline/multibranch job with typed parameters — same item-YAML schema as `item`, just tagged for discoverability), and `groovy` (an execution-only Groovy script consumed by [BroodOperation.executeGroovy](../operations/brood-operations.md#executegroovy-semantics)).
- Items declare **variables** with defaults and required flags; consumers supply values when they reference the item. A variable may also declare a `type` (`string`, `number`, `boolean`, or `credentials`) and, for `string`/`number`, an `allowedValues` list — the wizard renders a dropdown/checkbox/typed input accordingly instead of a plain text field. Omitting `type` means `string`; every catalog variable written before this feature existed remains valid unchanged. Typed-variable validation is currently enforced for `pipeline-template` items only — the other types accept the fields but don't validate them yet.
- A `jcasc` item may carry an **embedded `plugins:` block**: the composer splits it out and routes it to the plugin set instead of the JCasC content, so one item can ship both a configuration and the plugin it depends on (e.g. a theme item bundling `simple-theme-plugin`).
- **`groovy`** items are **execution-only**: consumed exclusively by [BroodOperation.executeGroovy](../operations/brood-operations.md#executegroovy-semantics) and **rejected as a `ComposedBundle` input** — the composer appends a non-fatal error to `status.errors` and the item's content is silently excluded from the bundle's output. This is by design: `groovy` scripts run with `Jenkins.ADMINISTER` on the target controller and must never be embedded in a bundle's JCasC/items/rbac content.

Catalog item list APIs return a compact summary only: `name`, `namespace`, display metadata, type, source, version, tags, validity, message, and `contentHash`. They do not include rendered `status.content`, `spec.path`, variables, requirements, or Kubernetes metadata. Fetch a single item detail to get the full `CatalogItem` CR including `status.content`; this split keeps large catalogs listable across clusters.

## How to author a catalog repository

Two equivalent layouts.

**Directory convention** (zero manifest — types inferred from directory names):

```
catalog/
├── jcasc/
│   └── varroa-theme.yaml
├── plugins/
│   └── sonar.yaml
├── items/
│   └── standard-folders.yaml
├── rbac/
│   └── readonly-auditors.yaml
├── pod-templates/
│   └── go-builder.yaml
└── pipeline-templates/
    └── go-service.yaml
```

**Explicit `catalog.yaml` index** (needed when items declare variables or metadata):

```yaml
# catalog.yaml at the catalog root
apiVersion: "1"
name: platform-catalog
items:
  - type: jcasc
    name: varroa-theme
    displayName: Varroa theme
    description: Corporate look-and-feel plus the theme plugin
    path: jcasc/varroa-theme.yaml
    version: "2"
    tags: [ui, baseline]
    variables:
      - name: accent_color
        default: "#2aa198"
        description: Primary accent color
      - name: org_name
        required: true
```

An item file is plain YAML of its type. A `jcasc` item with variables and an embedded plugin:

```yaml
# jcasc/varroa-theme.yaml
plugins:                       # embedded block — routed to the plugin set, not jenkins.yaml
  - artifactId: simple-theme-plugin
    version: "196.v96d9592f4efa"
appearance:
  simpleTheme:
    elements:
      - cssUrl:
          url: https://assets.example.com/${org_name}/theme.css
```

A `pipeline-template` item uses the same `items.yaml` schema as a plain `item`, restricted to `kind: pipeline` or `kind: multibranch`, with typed variables declared in `catalog.yaml`:

```yaml
# catalog.yaml entry
- type: pipeline-template
  name: go-service
  displayName: Go microservice pipeline
  path: pipeline-templates/go-service.yaml
  variables:
    - name: environment
      type: string
      allowedValues: [dev, staging, prod]
      default: dev
    - name: run_integration_tests
      type: boolean
      default: "false"
    - name: deploy_credentials_id
      type: credentials
      description: Jenkins credentials ID used to deploy
```

```yaml
# pipeline-templates/go-service.yaml
items:
  - name: go-service
    kind: multibranch
    sourcesList:
      - branchSource:
          source:
            github:
              repoOwner: example
              repository: go-service
```

## How to publish the catalog

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CatalogSource
metadata:
  name: platform-catalog
  namespace: varroa                  # the operator namespace makes it brood-visible (see resolution below)
spec:
  repoURL: https://github.com/example/platform-catalog.git
  revision: main
  path: catalog                      # catalog root within the repo; default "."
  syncIntervalSeconds: 300           # min 30, default 300
  secretRef: catalog-repo-creds      # optional; Secret in the same namespace
```

> **Note for basic-auth Secrets:** You must add the `varroa.dev/allowed-hosts`
> annotation listing the git host(s) this credential may be used with. SSH
> deploy-key Secrets do not require this annotation.
>
> ```bash
> kubectl annotate secret catalog-repo-creds -n varroa \
>   varroa.dev/allowed-hosts='github.com'
> ```

```bash
kubectl apply -f catalog-source.yaml
```

**Verify:**

```bash
kubectl get catalogsource platform-catalog -n varroa \
  -o jsonpath='{.status.phase}{" items="}{.status.itemCount}{" rev="}{.status.observedRevision}{"\n"}'
# Ready items=5 rev=<commit-sha>
kubectl get catalogitems -n varroa
# NAME              TYPE     SOURCE
# varroa-theme      jcasc    platform-catalog
# ...
```

An invalid item shows `valid: false` with the parse error in its `status.message` — check items after each catalog change.

## Publishing a catalog via OCI

Instead of a git repository, a `CatalogSource` can reference an OCI artifact:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CatalogSource
metadata:
  name: platform-catalog
  namespace: varroa
spec:
  ociRef: ghcr.io/example/platform-catalog:v1
  path: catalog                      # sub-path within the artifact
  syncIntervalSeconds: 300
  # secretRef references a Kubernetes Secret with a .dockerconfigjson key
  # (standard k8s docker-registry secret) or username/password keys.
  secretRef: oci-pull-creds
```

`spec.ociRef` and `spec.repoURL` are mutually exclusive — exactly one must be set (enforced by CRD validation). The `secretRef` uses the same Secret pattern as git auth, but the Secret must contain either a `.dockerconfigjson` key (a standard `kubectl create secret docker-registry ...` secret) or `username`/`password` keys for basic registry auth.

Create the pull-credential Secret:

```bash
kubectl create secret docker-registry oci-pull-creds -n varroa \
  --docker-server=ghcr.io \
  --docker-username=myuser \
  --docker-password=<token>
```

The operator resolves the artifact's manifest digest on each sync and stores it as `status.observedRevision`. When the digest changes, the operator re-pulls and syncs the catalog items — the same `syncIntervalSeconds` gate applies.

## How to consume an item

Reference it from a [`ComposedBundle`](composed-bundles.md) input, supplying variable values:

```yaml
spec:
  inputs:
    - itemRef:
        name: varroa-theme
        variables:
          org_name: acme            # itemRef variables have the highest precedence
    - gitSource:
        repoURL: https://github.com/example/casc-bundles.git
        path: bundles/team-platform
        revision: main
```

Pin a consumer to an exact content version with `pinnedContentHash: <sha256>` (from the item's `status.contentHash`); leave it empty to track latest. When a pinned item changes upstream, the bundle reports phase `Drifted` and lists it in `status.driftedItems` — your cue to review and re-pin.

### Cross-namespace resolution

An `itemRef` **without** a `namespace` resolves **local-first**: the bundle's own namespace, then falls back to the operator namespace. Setting `namespace` looks up only there. Publish shared items in the operator namespace; teams can shadow them locally with a same-named item.

**Verify:** `kubectl get composedbundle <name> -n <ns> -o jsonpath='{.status.inputSummary}'` — each `itemRef` entry shows the namespace it actually resolved in.

## Troubleshooting

- `CatalogSource` phase `Error` → fetch/auth failure; `status.message` has the git or OCI error.
- Item missing from a bundle → bundle phase `Invalid` with the name in `status.missingItems`; check the item exists in a resolvable namespace.
- Edited an item but bundles didn't recompose → wait out `syncIntervalSeconds`, then check the item's `observedRevision` advanced.

## Related pages

- [Composed bundles](composed-bundles.md) — how itemRef + gitSource inputs merge
- [Bundle sources](bundle-sources.md) — the git-repo input kind
- [Plugin pinning](plugin-pinning.md) — where embedded/plugin-item entries land
- [Jobs & items](items.md) — schema for `item`-type content
