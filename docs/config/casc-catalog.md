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
| `pipeline-template` | A `PipelineTemplate` document: a parameterized pipeline body, its schema, and its version |
| `groovy` | Script for `executeGroovy` only |

`groovy` items cannot enter a `ComposedBundle`. Their controls are documented
in [executeGroovy security](../security/execute-groovy.md).

On a cell, the `podtemplate` items of a tenant bundle go on the tenant's own
cloud, with the tenant's name, namespace, service account, and label written on
each template. See [Tenant pod templates](../operations/tenant-pod-templates.md).

## Describe catalog content

Use conventional directories for simple catalogs, or add `catalog.yaml` for
metadata and variables. With no `catalog.yaml`, a source scans its root for these directory
names:

| Directory | Type |
|---|---|
| `jcasc/` | `jcasc` |
| `plugins/` | `plugin` |
| `items/` | `item` |
| `rbac/` | `rbac` |
| `pod-templates/` | `podtemplate` |
| `pipeline-templates/` | `pipeline-template` |

A `pipeline-templates/` file is a `PipelineTemplate` document, not items YAML. See
[Pipeline templates](pipeline-templates.md) for the document's fields, its parameter schema, and
the marker file that selects it.

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

`syncIntervalSeconds` defaults to 300. A set value must be between 30 and 31536000 (one year); 0 or an omitted field means the default. The API, the MCP tools, and kubectl all reject any other value. Private Git uses
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
use `.dockerconfigjson` or `username` and `password` in a same-namespace Secret;
see [bundle source authentication](bundle-sources.md#reference-oci) for the
one-registry-per-secret constraint.

The `create_catalog_source` and `update_catalog_source` MCP tools accept
`ociRef`, `trusted` and `syncIntervalSeconds` alongside the git fields, so an
agent can manage either source kind. On update, a field is changed only when
the argument is present: omit it to keep the stored value, pass an empty string
to clear it. Setting `ociRef` clears `repoURL` and `revision`, and setting
`repoURL` clears `ociRef`, keeping the exactly-one rule satisfied.

### Manage sources in the dashboard

The Catalog Sources page lists the location of each source. The location is
the repository URL, the OCI reference, or the label "Update center" for the
reserved `varroa-update-center` source. The New Source form has a source type
select. The OCI type has no revision field. For an OCI source, the Revision
column shows the resolved digest. The operator manages the reserved source, so
the page has no Edit or Delete action for it. The API, the MCP tools, and
`varroactl` refuse a create, an edit, or a delete of `varroa-update-center`
with HTTP 409. A sync of the reserved source is permitted.

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

The `REPOSITORY` column shows a git source. The `OCI-REF` column shows an OCI source. The `varroa-update-center` source shows neither.

| Symptom | Check |
|---|---|
| Source is `Error` | `status.message`, URL, credentials, path, and network access |
| Item is invalid | Item `status.message` and source format |
| Bundle cannot find item | Item namespace and local-first resolution |
| Update is absent | Sync interval and `status.observedRevision` |
