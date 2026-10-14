# Pool Shared Libraries

A cell pool can declare trusted shared libraries. Every cell of the pool serves them as global libraries to all of its tenants. Read [Cell pools](cell-pools.md) first.

## Trust model

Two kinds of shared library exist in a cell.

| Kind | Declared by | Runs | Use |
|---|---|---|---|
| Pool library | The operator, on `CellPool.spec.libraries` | Outside the Groovy sandbox | An organisation library that needs full trust |
| Folder library | The tenant, in a folder property | Inside the Groovy sandbox | A tenant library |

For the approval policy that covers both kinds, see [Script approvals](script-approvals.md).

A pool library is trusted. Its code can call any Jenkins API. A folder library is sandboxed. See [Tenant shared libraries](tenant-libraries.md).

## Spec fields

Each entry of `spec.libraries` has these fields. The list holds at most 16 entries. Each `name` is unique.

| Field | Type | Meaning |
|---|---|---|
| `name` | string, required | The library name. A pipeline loads the library by this name. |
| `remote` | string, required | The repository URL. It must not contain `${`. |
| `defaultVersion` | string, required | The branch, tag, or commit to load when a pipeline names no version. |
| `credentialRef` | string, optional | The name of a labelled Secret that holds the login. Required for an `ssh://` or `user@host:path` remote. |
| `implicit` | bool, default false | Loads the library in every job, without an `@Library` annotation. |
| `allowVersionOverride` | bool, default false | Lets a pipeline load another version. |

## Credential Secret

Create the Secret in the operator namespace. Set the label `varroa.dev/pool-library-credential: "true"`. The operator reads only Secrets with this label.

Use one of these two shapes.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: platform-lib-key
  namespace: varroa-system
  labels:
    varroa.dev/pool-library-credential: "true"
stringData:
  username: git
  privateKey: |
    <contents of the private key file>
  passphrase: optional-passphrase   # valid only with privateKey
```

Use `password` instead of `privateKey` for an HTTPS remote. Set exactly one of them. Do not put `${` in any value.

The operator makes the Jenkins credential ID `varroa-pool-library-<name>`, where `<name>` is the library name.

## Load the library

A pipeline loads a library by name. The pipeline needs no credential.

```groovy
@Library('platform-lib') _
```

With `implicit: true`, the library loads in every job of the pool. The pipeline needs no annotation.

A run page links the library to its repository. A library on a Gitea or Forgejo host links only when `spec.giteaServers` lists that host.

## Warning: version override

`allowVersionOverride: true` lets a pipeline load any branch or tag of the library. Anyone who can push a branch to the library repository then runs trusted code. That code runs on every cell of the pool, for every tenant. Keep this value `false` unless the repository has strict branch protection.

## Egress

A cell reaches the library repository from its own JVM. The operator adds one egress rule for each library remote. The rule allows the git host and port of the remote.

- The operator adds one egress rule for each resolved address of each library host.
- The port is the explicit port in the URL. Without one, it is 443 for `https` and 22 for `ssh` and `user@host:path` remotes.
- The operator resolves a hostname on every reconcile. A lookup failure skips that library's rule only.
- A host with rotating addresses rewrites the policy often. For such a host, add a CIDR entry to `networkPolicy.cellIsolation.extraEgress` instead.
- The rules apply only when `networkPolicy.cellIsolation.enabled` is `true`.

## Condition

A cell reports the `PoolLibraries` condition on its `Controller`.

| Reason | Status | Holds when | Action |
|---|---|---|---|
| `Rendered` | `True` | Every library rendered. | None. |
| `CredentialMissing` | `False` | The named Secret is missing, or the list read failed. | Create the labelled Secret. |
| `CredentialMalformed` | `False` | The Secret has no username, has both or neither of `password` and `privateKey`, or has a `${` value. | Fix the Secret keys. |
| `LibraryInvalid` | `False` | The `remote` or `defaultVersion` holds `${`. | Fix the library entry. |
| `PluginMissing` | `False` | The pinned plugin set lacks `pipeline-groovy-lib` or `git`. No library renders. | Use a version profile that pins both plugins. |

A library that fails is not rendered. The other libraries still render, unless a required plugin is missing. A missing plugin holds every library of the pool.

If the operator cannot read the `CellPool`, the cell's bundle resolve fails. The reconcile retries. The `PoolLibraries` condition is not updated in that pass.

## Edits

A change to `spec.libraries` reaches each placed cell on its next reconcile. The cell does not roll its pod. A library that you remove stops loading after that reconcile.
