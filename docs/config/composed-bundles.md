# Compose controller bundles

A `ComposedBundle` merges catalog items, Git bundles, and OCI bundles in a
defined order.

## Create a bundle

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ComposedBundle
metadata:
  name: platform-baseline
  namespace: teams-platform
spec:
  inputs:
    - itemRef:
        name: standard-theme
        variables:
          organization: example
    - gitSource:
        repoURL: https://github.com/example/casc-bundles.git
        path: bundles/team-platform
        revision: main
    - ociSource:
        ref: ghcr.io/varroaci/casc-bundles:v1
  variables:
    timezone: UTC
  jcascMergeStrategy: errorOnConflict
```

Each input sets exactly one of `itemRef`, `gitSource`, or `ociSource`. Inputs
merge from top to bottom. `errorOnConflict` rejects conflicting scalar JCasC
values. `override` lets a later input replace an earlier value.

The `update_composed_bundle` MCP tool accepts `inputs` and replaces the whole
list in the order given, the same way a REST `PUT` does. Omit the argument to
leave the stored inputs alone. An empty list is rejected, because a bundle with
no inputs composes to nothing.

Attach the bundle:

```yaml
spec:
  composedBundleRef:
    name: platform-baseline
```

An omitted namespace means the controller namespace. A controller with no
reference uses the built-in `varroa-starter` bundle. An explicit reference
replaces that bundle.

## Supply variables

`spec.variables` applies across the composition. Variables on an `itemRef`
take precedence. Varroa also supplies:

| Variable | Value |
|---|---|
| `varroa_controller_name` | Controller name |
| `varroa_controller_namespace` | Controller namespace |
| `varroa_controller_endpoint` | In-cluster Jenkins URL |
| `varroa_controller_external_url` | External Jenkins URL |
| `varroa_controller_path_prefix` | Path ingress prefix or empty string |
| `varroa_frontend_url` | Dashboard URL |
| `varroa_oidc_issuer` | Configured OIDC issuer |
| `varroa_oidc_client_id` | Configured OIDC client ID |
| `varroa_login_url` | Configured login URL |

Unresolved required variables prevent a controller from applying the new
content. The last valid configuration remains active.

## Pin catalog content

```yaml
- itemRef:
    name: standard-theme
    pinnedContentHash: <sha256>
```

When the catalog item changes, the composed bundle enters `Drifted` until the
pin is updated. Unpinned items track current catalog content.

## Inspect and control rollout

| Status field | Use |
|---|---|
| `phase` | `Pending`, `Ready`, `Drifted`, or `Invalid` |
| `resolvedHash` | Digest of merged unresolved content |
| `contentRef` | ConfigMap containing that content |
| `observedRevisions` | Resolved item hashes, Git commits, and OCI digests |
| `missingItems`, `errors`, `warnings` | Resolution and validation findings |

Pause dependent controller rollout during review:

```bash
kubectl annotate composedbundle platform-baseline -n teams-platform \
  varroa.dev/rollout-paused=true
```

Remove the annotation to resume. See [rollout waves](../operations/rollout-waves.md)
for staged application.

## Preflight plugin pins

The validate and preview responses also include `pinPreflight`, computed
against the same resolved plugin set a controller would apply. `conflicts`
names any non-core artifact whose bundle-pinned version differs from that
set; `missing` names any pinned artifact the set doesn't carry at all. Both
arrays come back empty, never `null`, when nothing is amiss.

`pinPreflight` is advisory: it exists so a conflict shows up while the bundle
is still being authored, before it's ever attached to a controller. See
[pin Jenkins plugins](plugin-pinning.md) for how the equivalent
`PluginPinConflict` condition behaves once a bundle is in use.

## Troubleshoot

| Symptom | Check |
|---|---|
| Merge conflict | Input order or deliberate `override` strategy |
| Missing item | `status.missingItems` and namespace resolution |
| Ready bundle is not applied | Variables, rollout pause, waves, and reconciliation mode |
| Authorization content disappears | Configure [Jenkins RBAC](../security/jenkins-rbac.md) |
