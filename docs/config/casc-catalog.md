# Publish a CasC catalog

A `CatalogSource` turns reusable Git or OCI content into read-only
`CatalogItem` resources.

## Choose an item type

| Type | Content |
|---|---|
| `jcasc` | JCasC fragment |
| `plugin` | Plugin entries |
| `item` | Jobs and folders |
| `rbac` | Jenkins role definitions |
| `podtemplate` | Jenkins agent pod templates |
| `pipeline-template` | Parameterized pipeline or multibranch item |
| `groovy` | Script for `executeGroovy` only |

`groovy` items cannot enter a `ComposedBundle`. Their controls are documented
in [executeGroovy security](../security/execute-groovy.md).

## Describe catalog content

Use conventional directories for simple catalogs, or add `catalog.yaml` for
metadata and variables:

```yaml
apiVersion: "1"
name: platform-catalog
items:
  - type: jcasc
    name: standard-theme
    displayName: Standard theme
    path: jcasc/standard-theme.yaml
    version: "1"
    tags: [baseline]
    variables:
      - name: organization
        required: true
      - name: accent_color
        type: string
        default: "#336699"
```

Variable types are `string`, `number`, `boolean`, and `credentials`.
`allowedValues` is valid for string and number variables.

## Publish from Git

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CatalogSource
metadata:
  name: platform-catalog
  namespace: varroa-system
spec:
  repoURL: https://github.com/example/platform-catalog.git
  revision: main
  path: catalog
  syncIntervalSeconds: 300
```

`syncIntervalSeconds` defaults to 300 and must be at least 30. Private Git uses
a same-namespace `secretRef`; see [bundle source authentication](bundle-sources.md#reference-git).

## Publish from OCI

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CatalogSource
metadata:
  name: platform-catalog
  namespace: varroa-system
spec:
  ociRef: ghcr.io/varroaci/platform-catalog:v1
  path: catalog
  secretRef: catalog-pull
```

User-created sources set exactly one of `repoURL` and `ociRef`. OCI credentials
use `.dockerconfigjson` or `username` and `password` in a same-namespace Secret.

## Consume an item

```yaml
spec:
  inputs:
    - itemRef:
        name: standard-theme
        variables:
          organization: example
```

An unqualified item resolves in the bundle namespace, then the operator
namespace. Set `itemRef.namespace` for an exact namespace. Pin reviewed content
with `pinnedContentHash: <status.contentHash>`.

## Verify synchronization

```bash
kubectl get catalogsource platform-catalog -n varroa-system
kubectl get catalogitems -n varroa-system
```

| Symptom | Check |
|---|---|
| Source is `Error` | `status.message`, URL, credentials, path, and network access |
| Item is invalid | Item `status.message` and source format |
| Bundle cannot find item | Item namespace and local-first resolution |
| Update is absent | Sync interval and `status.observedRevision` |
