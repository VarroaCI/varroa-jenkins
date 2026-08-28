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

## Troubleshoot

| Symptom | Check |
|---|---|
| Plugin set is not ready | ConfigMap location, key, and YAML |
| Prerequisites fail | Resolve the closure against the core |
| Unexpected version | Profile match and [plugin precedence](plugin-pinning.md) |
| Change is held | Reconciliation policy and conditions |
