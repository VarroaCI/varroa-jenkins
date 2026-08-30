# Publish bundle sources

A bundle source is a Git repository or OCI artifact containing a Varroa bundle
directory.

## Create the directory

Every bundle requires `bundle.yaml` and at least one JCasC file.

```text
team-bundle/
|-- bundle.yaml
|-- jenkins.yaml
|-- plugins.yaml
|-- items.yaml
`-- variables.yaml
```

```yaml
id: team-platform
version: "1"
apiVersion: "2"
jcasc:
  - jenkins.yaml
plugins:
  - plugins.yaml
items:
  - items.yaml
variables:
  - variables.yaml
jcascMergeStrategy: errorOnConflict
itemRemoveStrategy:
  items: none
  rbac: sync
```

| Field | Contract |
|---|---|
| `id`, `version`, `apiVersion` | Required; `apiVersion` is `1` or `2` |
| `jcasc` | One or more JCasC files |
| `plugins`, `items`, `rbac`, `variables` | Optional file lists |
| `jcascMergeStrategy` | `errorOnConflict` or `override` |
| `itemRemoveStrategy.items` | `none`, `sync`, `remove-supported`, or `remove-all` |
| `itemRemoveStrategy.rbac` | `sync` or `update` |

Use [plugin pinning](plugin-pinning.md) and [jobs and items](items.md) for those
file formats. Configure authorization through
[Jenkins RBAC](../security/jenkins-rbac.md), not JCasC.

## Reference Git

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ComposedBundle
metadata:
  name: platform-baseline
  namespace: teams-platform
spec:
  inputs:
    - gitSource:
        repoURL: https://github.com/example/casc-bundles.git
        path: bundles/team-platform
        revision: main
```

The URL must use HTTPS, SSH, or scp-style `git@host:path`. Pin a tag or commit
when publication must not move without a spec change.

For private HTTPS Git, create a same-namespace Secret with `username` and
`password`, annotate it with the allowed hosts, and set `secretRef`:

```bash
kubectl create secret generic bundle-git -n teams-platform \
  --from-literal=username=git \
  --from-literal=password='<token>'
kubectl annotate secret bundle-git -n teams-platform \
  varroa.dev/allowed-hosts=github.com
```

The comma-separated `varroa.dev/allowed-hosts` annotation is required for
username and password credentials. SSH private-key Secrets do not use it.

## Reference OCI

```yaml
spec:
  inputs:
    - ociSource:
        ref: ghcr.io/varroaci/casc-bundles:v1
        path: bundles/team-platform
        secretRef: oci-pull-creds
```

`path` and `secretRef` are optional. The same-namespace pull Secret must contain
`.dockerconfigjson` or `username` and `password`. A `.dockerconfigjson` secret
must contain exactly one registry entry; a secret with more than one `auths`
entry is rejected.

## Size the Git cache

```yaml
operator:
  gitCache:
    enabled: true
    maxRepos: 50
    maxSizeMiB: 2048
    volumeSizeLimit: 3Gi
```

The cache is per operator replica and uses `emptyDir`. Size it for active
repository count and checkout size. Eviction is automatic.

## Troubleshoot

| Symptom | Check |
|---|---|
| Missing `bundle.yaml` | Input `path` points to the bundle directory |
| Authentication failure | Secret keys, namespace, and allowed host |
| Git change is absent | `revision` and `status.observedRevisions` |
| OCI change is absent | Tag or digest and `status.observedRevisions` |
