# Pin Jenkins plugins

Varroa combines the selected Jenkins core lock with one non-core plugin source.

## Understand precedence

1. The selected [version profile](jenkins-versions.md), or embedded baseline,
   supplies core plugin pins.
2. A non-empty `Controller.spec.pluginSpec.entries` supplies the complete
   non-core list.
3. Otherwise, the composed bundle's `plugins.yaml` supplies the non-core list.

The controller list replaces the bundle list. It does not merge with it. Core
pins always win. A conflicting non-core pin sets `PluginConflict=True` and
blocks provisioning.

## Preflight bundle pins before they block anything

`PluginPinConflict` is a separate, advisory check: it compares the composed
bundle's own `plugins.yaml` against the resolved plugin set and reports what
it finds as a condition on both the controller and the bundle, without ever
blocking provisioning. It runs at the same points `PluginConflict` does, but
the two are independent: a bundle can carry a pin problem while
`PluginConflict` stays clear, and vice versa.

A conflict means the bundle pins a non-core artifact to a version that
differs from the resolved set. A missing entry means the bundle pins an
artifact the resolved set does not carry at all; that case is reported the
same way but is never treated as a conflict. Either way, `PluginPinConflict`
only tells you something worth checking before you attach the bundle to a
controller. It doesn't change what gets installed.

## Add bundle plugins

```yaml
plugins:
  - artifactId: sonar
    version: "2.17.3"
  - artifactId: timestamper
    version: "1.28"
```

Commit the file referenced by the bundle manifest, then wait for composition
and controller reconciliation.

## Replace the list for one controller

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: teams-platform
spec:
  pluginSpec:
    policy: pinned
    entries:
      - artifactId: sonar
        version: "2.17.3"
      - artifactId: timestamper
        version: "1.28"
```

Include every required non-core plugin because the controller entries replace
the bundle entries.

## Apply a plugin roll

Adds, removals, and version changes require a controller roll. Automatic mode
rolls during reconciliation. Manual mode records `status.pendingPluginRoll`
until an authorized user applies action `plugin-roll` through the controller
apply endpoint or dashboard.

Check the pending change before approval:

```bash
kubectl get controller demo -n teams-platform \
  -o jsonpath='{.status.pendingPluginRoll}{"\n"}'
```

## Troubleshoot

| Symptom | Check |
|---|---|
| Bundle plugin is missing | Controller entries replacing the bundle list |
| `PluginConflict=True` | Pin conflicts with the core lock |
| `PluginPinConflict=True` | Bundle pin vs. resolved plugin set; advisory, provisioning proceeds |
| Roll is pending | Reconciliation mode and approval |
| Prerequisite failure | Dependency closure and Jenkins core |
