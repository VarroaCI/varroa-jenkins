# Publish bundle sources

A bundle source is a Git repository or OCI artifact with a bundle directory.

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
```

| Field | Contract |
|---|---|
| `id`, `version`, `apiVersion` | Required; `apiVersion` is `1` or `2` |
| `jcasc` | One or more JCasC files |
| `plugins`, `items`, `rbac`, `variables` | Optional file lists |
| `jcascMergeStrategy` | `errorOnConflict` or `override` |

Item removal does not come from `bundle.yaml`. See [Control removal](items.md#control-removal).
Varroa ignores any top-level `bundle.yaml` key that it does not declare, such as `itemRemoveStrategy` or `rbacRemoveStrategy`.

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
so content changes only with a spec change.

For private HTTPS Git, set `secretRef` to a same-namespace Secret with
`username`, `password`, and an allowed-hosts annotation:

```bash
kubectl create secret generic bundle-git -n teams-platform \
  --from-literal=username=git \
  --from-literal=password='<token>'
kubectl annotate secret bundle-git -n teams-platform \
  varroa.dev/allowed-hosts=github.com
```

Username and password credentials require the comma-separated
`varroa.dev/allowed-hosts` annotation. SSH key Secrets do not.

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

Each operator replica has its own `emptyDir` cache with automatic eviction.
Size it for repository count and checkout size.

## Troubleshoot

| Symptom | Check |
|---|---|
| Missing `bundle.yaml` | Input `path` points to the bundle directory |
| Authentication failure | Secret keys, namespace, and allowed host |
| Git change is absent | `revision` and `status.observedRevisions` |
| OCI change is absent | Tag or digest and `status.observedRevisions` |
| `context deadline exceeded` | Remote reachability; previews stop at 55 s, syncs at 2 min |
