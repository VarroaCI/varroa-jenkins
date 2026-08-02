# Bundle Sources (CloudBees-style git repos)

<!-- sources: internal/bundle/manifest.go, internal/bundle/git.go, internal/bundle/resolver.go, internal/bundle/validator.go, api/v1alpha1/types.go (GitBundleSource) -->

A bundle source is a git repository or OCI artifact holding Configuration-as-Code content in the CloudBees CasC bundle layout. If you're coming from CloudBees CI, your existing bundle repos are the model; if not, this page shows the layout from scratch. Git and OCI sources are two of the three input kinds a [composed bundle](composed-bundles.md) accepts (the third is [catalog items](casc-catalog.md)).

## Concepts: repository layout

A bundle is a directory with a required `bundle.yaml` manifest naming the files in each section:

```
my-bundle/
├── bundle.yaml        # manifest (required)
├── jenkins.yaml       # JCasC configuration (≥1 file required)
├── plugins.yaml       # plugin list (optional)
├── items.yaml         # jobs/folders — see Jobs & items (optional)
├── rbac.yaml          # Jenkins role definitions (optional)
└── variables.yaml     # substitution variables (optional)
```

```yaml
# bundle.yaml
id: team-platform
version: "3"                 # bump on every change you want rolled out
apiVersion: "2"              # "1" or "2"
description: Platform team baseline
jcasc:
  - jenkins.yaml             # at least one entry required
plugins:
  - plugins.yaml
items:
  - items.yaml
rbac:
  - rbac.yaml
variables:
  - variables.yaml
jcascMergeStrategy: errorOnConflict   # or: override
itemRemoveStrategy:
  items: none                # none | sync | remove-supported | remove-all
```

Section notes:

- **jcasc** — standard [JCasC](https://github.com/jenkinsci/configuration-as-code-plugin) YAML. One caveat: any `authorizationStrategy` you set is stripped — Varroa owns authorization; see [Jenkins RBAC](../security/jenkins-rbac.md).
- **plugins** — `plugins:` list of `{artifactId, version}`; how it interacts with version-profile pins and `pluginSpec` is covered in [Plugin pinning](plugin-pinning.md).
- **items** — declarative jobs and folders; the full schema lives in [Jobs & items](items.md), including what each `itemRemoveStrategy` value does.
- **variables** — `key: value` pairs substituted into the other files as `${key}`. Varroa also injects its own `varroa_controller_*` variables; see [Composed bundles](composed-bundles.md). A missing `variables.yaml` reference produces a validation warning, not an error.

## How to publish a bundle source

1. Create the repo and directory, commit the files above. Multiple bundles can live in one repo under different paths.

2. Reference it from a `ComposedBundle` input:

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
           path: bundles/team-platform        # directory containing bundle.yaml
           revision: main                     # branch, tag, or commit SHA
   ```

   ```bash
   kubectl apply -f composed-bundle.yaml
   ```

3. **Verify:**

   ```bash
   kubectl get composedbundle platform-baseline -n teams-platform \
     -o jsonpath='{.status.phase}{" "}{.status.resolvedHash}{"\n"}'
   # Ready 4f8a…      ← composed; hash changes when the repo content changes
   kubectl get composedbundle platform-baseline -n teams-platform \
     -o jsonpath='{.status.observedRevisions}'
   # {"0":"<commit-sha>"}   ← the exact commit materialized for input 0
   ```

`repoURL` accepts `https://`, `ssh://`, or scp-style `git@host:path` only — transport helpers (`ext::`, `file://`) are rejected by CRD validation.

## How to use a private repository

Create a Secret in the same namespace as the `ComposedBundle` and reference it:

```bash
kubectl create secret generic casc-repo-creds -n teams-platform \
  --from-literal=username=git \
  --from-literal=password=<token-or-app-password>
```

> **Important:** For basic-auth (username/password) Secrets, you must add the
> `varroa.dev/allowed-hosts` annotation listing the git host(s) this credential
> may be used with. This prevents a namespace member from reusing the credential
> against an attacker-controlled git host. SSH deploy-key Secrets
> (`ssh-privatekey`) do not require this annotation.
>
> ```bash
> kubectl annotate secret casc-repo-creds -n teams-platform \
>   varroa.dev/allowed-hosts='github.com,gitlab.com'
> ```

```yaml
      - gitSource:
          repoURL: https://github.com/example/private-bundles.git
          path: bundles/team-platform
          revision: main
          secretRef: casc-repo-creds
```

**Verify:** `status.phase` reaches `Ready`; on auth failure it goes `Invalid` with the fetch error in `status.errors`. If a basic-auth secret is missing the `varroa.dev/allowed-hosts` annotation (or the host is not in the list), the bundle goes `Invalid` with a host-scoping error — no git operation is attempted.

## How to use an OCI bundle source

Instead of a git repository, a `ComposedBundle` input can reference an OCI artifact containing a bundle directory:

```yaml
      - ociSource:
          ref: ghcr.io/example/casc-bundles:v1
          path: bundles/team-platform     # optional: sub-path within the artifact
          secretRef: oci-pull-creds       # optional: pull-credential Secret name
```

`ref` is the OCI artifact reference (required). `secretRef` names a Secret in the same namespace as the `ComposedBundle`, using the same `.dockerconfigjson` or `username`/`password` key format as the catalog OCI source. The operator detects drift by resolving the manifest digest and comparing it against `status.observedRevisions` — when the digest changes, recomposition is triggered, the same as for git-source SHA drift.

## Concepts: caching and change detection

The operator has two layers of git caching for bundle/catalog sources:

**Layer 1 — compose-skip (unchanged).** The operator caches materialized bundle content in memory and checks the remote cheaply with `git ls-remote`: when the revision's SHA is unchanged, no clone happens and composition reuses the cache. Pushing a commit changes the SHA, invalidates the cache, triggers recomposition, and produces a new `status.resolvedHash` — which is what controllers converge on (and what [rollout waves](../operations/rollout-waves.md) gate). Pinning `revision` to a tag or commit SHA freezes the input until you change it.

**Layer 2 — on-disk bare-clone cache (new, `operator.gitCache.*`).** Repeated composes and catalog syncs of an unchanged repo cost only a lightweight `ls-remote` (no clone). When a branch tip moves, the cache performs a shallow fetch rather than a full clone. Private repos are supported: credentials flow through the existing ephemeral auth mechanism and are never stored in the cache directory.

The cache is per-replica `emptyDir` (not PVC) and bounded by LRU eviction (`maxRepos`, `maxSizeMiB`). Disable it with `operator.gitCache.enabled: false` to revert to today's direct-clone behavior.

See [Scaling: Bundle cache](../architecture/scaling.md#bundle-cache) for metrics and tuning.

## Troubleshooting

- `Invalid` phase, "bundle.yaml is required but missing" → `path` doesn't point at the directory containing `bundle.yaml`.
- Composed but controller won't pick it up → check the completeness gate and rollout state; see [Composed bundles](composed-bundles.md) and [Troubleshooting](../operations/troubleshooting.md).
- Changes pushed but nothing happens → you pinned `revision` to a tag/SHA, or the change is on a different branch.

## Related pages

- [Composed bundles](composed-bundles.md) — how inputs merge, in what order
- [CasC catalog](casc-catalog.md) — the other input kind
- [Jobs & items](items.md) — the items.yaml schema in full
- [Plugin pinning](plugin-pinning.md) — plugins.yaml interaction with profiles
