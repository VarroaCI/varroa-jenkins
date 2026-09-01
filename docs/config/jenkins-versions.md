# Select Jenkins versions

`JenkinsVersionProfile` binds a Jenkins core version to a resolved plugin set
and optional JCasC overlay.

## Select a profile

```bash
kubectl get jenkinsversionprofiles
```

Resolution uses this order:

1. Exact version profile.
2. Matching LTS-line profile.
3. Plugin-lock baseline embedded in the operator.

An empty controller version or `lts` selects the embedded baseline. It does not
track a moving latest image. For an LTS-line profile, `resolveVersion` selects
the exact patch release deployed and used for plugin resolution.

```bash
kubectl patch controller demo -n teams-platform --type merge \
  -p '{"spec":{"version":"2.555"}}'
```

Version changes follow the controller's reconciliation policy and may require
approval.

## Default profiles

The operator seeds a small set of default `JenkinsVersionProfile` resources
and their pluginset ConfigMaps on every start, from content built into the
operator binary. This content is no longer installed or managed by
`helm upgrade`. The operator is the source of ship-time profile data going
forward, and a version bump to the operator can bring new or updated
default profiles with it.

A hand-authored profile or ConfigMap that shares a default's name is left
alone: the seeder only ever writes objects it created itself, and never
overwrites one it doesn't recognize as its own. Deleting or renaming a
seeded default profile is not a durable way to remove it. The seeder
reconciles on a 60-second tick and re-applies any default profile it still
owns, so a deleted one returns within about a minute. What does persist is
a profile whose seed label was stripped, typically because a candidate was
promoted onto it, or a hand-authored profile or ConfigMap that shares a
seeded name; the seeder leaves both alone. To run something different on a
line, author your own profile instead of deleting the seeded one.

Because seeding happens after the operator starts, there's a brief window
right after an operator upgrade or restart where a default profile may not
yet be available. Controllers pinned to an LTS line covered by a default
profile fall back to the embedded plugin-lock baseline until the profile
seeds, then resolve normally.

## Create a profile

Create a ConfigMap in the operator namespace. Its `plugins.yaml` must contain a
fully resolved closure with exact versions:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: jenkins-2-555-pluginset
  namespace: varroa-system
data:
  plugins.yaml: |
    core:
      - artifactId: configuration-as-code
    plugins:
      - artifactId: configuration-as-code
        version: "<compatible-version>"
      - artifactId: workflow-api
        version: "<compatible-version>"
```

Then create the cluster-scoped profile:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: JenkinsVersionProfile
metadata:
  name: jenkins-version-2-555
spec:
  version: "2.555"
  resolveVersion: "2.555.3"
  channel: lts
  recommended: true
  pluginSetRef:
    name: jenkins-2-555-pluginset
  jcasc:
    content: |
      unclassified:
        location:
          url: ${varroa_controller_external_url}
    requiredPlugins:
      - configuration-as-code
```

`channel` is `lts` or `weekly`. `eol` is optional. Use actual plugin versions
resolved against the effective core version.

`PluginSetReady=True` means the materialized set is available.
`status.contentRef` names the operator-owned copy. `LockJcascMismatch` warns
that a JCasC-required plugin is missing from the lock but does not block the
profile.

## Default profiles vs. discovered candidates

The default profiles described above are ship-time content: the operator seeds them from a fixed set built into its own binary, and they only change when the operator itself is upgraded. They cover a small, known set of LTS lines.

A `ProfileCandidate` is different. It's discovered at runtime, from a profile already present in the cluster (default or hand-authored), when Varroa notices upstream has published a newer patch on that profile's line. It never appears until an operator promotes it. See [Jenkins upgrades](../operations/jenkins-upgrades.md) for how candidates are discovered, checked, and promoted, and how the `upgradePolicy` dial controls whether a promoted version actually rolls out.

## Troubleshoot

| Symptom | Check |
|---|---|
| Plugin set is not ready | ConfigMap location, key, and YAML |
| Prerequisites fail | Resolve the closure against the core |
| Unexpected version | Profile match and [plugin precedence](plugin-pinning.md) |
| Change is held | Reconciliation policy and conditions |
