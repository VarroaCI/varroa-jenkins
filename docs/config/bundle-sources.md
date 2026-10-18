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
For an annotated tag named by `revision`, `status.observedRevisions` records the commit that the tag points to. A 40-character SHA is recorded as given, even when it names a tag object.

The operator and `varroactl export` copy the files of the commit with `git archive`. They do not keep a `.git` directory. Files that `.gitattributes` marks `export-ignore` are not copied. `export-subst` placeholders are expanded. This applies with and without the Git cache.

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

`path` selects one bundle in a hand-built artifact whose root holds more than one bundle.

## Export a Git bundle to OCI

Use `varroactl export bundle` to publish one Git bundle as an OCI artifact:

```bash
varroactl export bundle --repo https://github.com/example/bundles.git \
  --path bundles/team-platform --revision main \
  --to oci://registry.example.com/bundles/team-platform:v1
```

`--to` accepts `oci://`, `dir://`, and `tar://`. The command refuses `uc://`, because the update center serves plugin packs only.

The artifact root is the bundle directory. Import it with `ref` only:

```yaml
spec:
  inputs:
    - ociSource:
        ref: registry.example.com/bundles/team-platform:v1
```

- An `oci://` destination with an explicit `:tag` uses that tag.
- A `dir://` or `tar://` destination uses the tag `<repo>-<path>`, for example
  `github.com-example-bundles-bundles-team-platform`.
- The paths `a-b` and `a/b` produce the same derived tag. Use an explicit tag in that case.
- The manifest records the source URL without credentials, the commit SHA, and the bundle path.

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
