# Plugin Pinning

<!-- sources: api/v1alpha1/types.go (PluginSpec, PluginEntry, PendingPluginRoll), internal/controller/controller_controller.go (managedPluginLines, nonCorePluginEntries, coreSetForCr), internal/api/handlers.go (apply actions) -->

Every plugin a controller runs is pinned and managed — there is no update-center roulette. This page covers where plugin lists come from, how conflicts resolve, and what happens when the desired set changes on a running controller.

## Concepts: the priority chain

The managed plugin list (`plugins.txt`) for a controller is built in two layers:

1. **Core set** — the pinned set from the matched [version profile](jenkins-versions.md) (or the operator's embedded baseline when no profile matches). These come first and always win: a later entry for the same `artifactId` is dropped.
2. **Non-core entries** — **either** the controller's `spec.pluginSpec.entries` (when non-empty) **or** the composed bundle's `plugins.yaml` — the controller spec **replaces** the bundle list entirely, it does not merge with it. Duplicates of core entries are dropped; duplicates within the list keep the first occurrence.

So the precedence to remember: **profile core set > pluginSpec (replaces bundle) > bundle plugins.yaml**. Catalog items contribute to the bundle layer (including [embedded plugin blocks](casc-catalog.md)).

A version-pin conflict — the bundle or spec pinning a *different version* of a plugin the core set locks — is surfaced as a warning; the core set's version ships.

## How to add a plugin via the bundle (the normal path)

Add it to your bundle repo's `plugins.yaml`:

```yaml
# plugins.yaml in the bundle source
plugins:
  - artifactId: sonar
    version: "2.17.3"
  - artifactId: timestamper
    version: "1.28"
```

Commit, let the bundle recompose, and the controller converges per its [reconciliation mode](../operations/reconciliation.md).

**Verify:** `kubectl get controller <name> -n <ns> -o jsonpath='{.status.conditions}'` stays clean, the roll completes, and the plugin appears in *Manage Jenkins → Plugins*.

## How to pin per controller

Use `spec.pluginSpec` when one controller needs a list different from its bundle siblings — remembering it **replaces** the bundle's `plugins.yaml`, so include everything non-core the controller needs:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata: { name: demo, namespace: teams-platform }
spec:
  namespace: teams-platform
  version: "2.555"
  composedBundleRef: { name: platform-baseline }
  pluginSpec:
    policy: pinned
    entries:
      - { artifactId: sonar, version: "2.17.3" }
      - { artifactId: timestamper, version: "1.28" }
```

**Verify:** after convergence, the installed plugin list contains the core set plus exactly these entries.

## Concepts: drift and rolls on a running controller

Plugin state is checked continuously, not just at provisioning:

- The mite's snapshots report installed plugins; the operator compares them with the desired managed lines in the **Connected** phase (the same core set governs both provisioning and drift checks).
- A change that requires re-baking the image/StatefulSet (plugin add/remove/version change) is a **plugin roll**. What happens next depends on the controller's [reconciliation mode](../operations/reconciliation.md):
  - `automatic` / roll allowed → the controller transitions `Connected → Provisioning` and rolls.
  - `manual` → the roll is parked as `status.pendingPluginRoll` with a human-readable diff (`+id:ver` / `-id:ver`) and waits for approval.
- The "plugins converged" signal clears only when the baked set equals the desired set — a roll that didn't take keeps the condition visible instead of lying.

## How to approve a pending plugin roll

```bash
kubectl get controller demo -n teams-platform -o jsonpath='{.status.pendingPluginRoll}' | jq .
# { "targetChecksum": "…", "since": "…", "changes": ["+sonar:2.17.3"] }
```

Approve from the dashboard (controller page → pending changes), or via the API's apply action:

```bash
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" \
  https://app.example.com/api/v1/clusters/core/controllers/teams-platform/demo/apply \
  -d '{"action": "plugin-roll"}'
```

**Verify:** the controller rolls (`Provisioning` → back to `Connected`) and `status.pendingPluginRoll` is empty.

## Concepts: the version-skew failure mode

The one way plugin pinning bites: a plugin set resolved against a **newer core** than the controller runs. Jenkins' plugin bootstrap then aborts the whole startup with `AggregatePluginPrerequisitesNotMetException` — the pod crash-loops before JCasC even runs. Varroa's guardrails: LTS-line profiles deploy their `resolveVersion` core, and lock generation (`hack/gen-plugin-lock.sh`) resolves against the exact target core. If you hand-edit plugin versions, you own this risk — see [Troubleshooting](../operations/troubleshooting.md) for the recovery steps.

## Concepts: the pin-vs-lock mismatch failure mode

A different failure mode from version-skew: a plugin pin (from a catalog item,
`spec.pluginSpec`, or a bundle git/OCI input) pins a **different version** of a plugin
than the active [JenkinsVersionProfile](jenkins-versions.md) / core lock requires.
When detected the controller is wedged at 0/0 in `Provisioning` with
`ConditionPluginConflict=True`.

**Observability surface:**

| Signal | Detail |
|---|---|
| Metric | `varroa_controller_plugin_lock_conflict{namespace,controller}` — gauge, 1 while conflicted |
| Activity event | `pluginConflict.detected` — warning-level, emitted when the condition flips True |
| Detail page | Red `PluginConflictBanner` above the fold, showing the condition message and reason |

See [Troubleshooting → Plugin lock conflict](../operations/troubleshooting.md#plugin-lock-conflict)
for the full runbook.

## Troubleshooting

- Plugin missing after adding to the bundle → the controller has a non-empty `spec.pluginSpec` that replaces the bundle list.
- Roll never happens → `manual` mode with a pending roll awaiting approval, or the wave gate is holding ([Rollout waves](../operations/rollout-waves.md)).
- Pod crash-loops right after a version/plugin change → version skew, above.
- Controller stuck in Provisioning with `ConditionPluginConflict=True` → pin-vs-lock mismatch, above.
- Requested version didn't ship → core-set pin wins; check the profile's locked version.

## Related pages

- [Jenkins versions](jenkins-versions.md) — profiles, locks, and the resolution chain
- [Composed bundles](composed-bundles.md) — where bundle plugins.yaml comes from
- [Reconciliation](../operations/reconciliation.md) — automatic vs manual roll behavior
- [The mite](../architecture/mite.md) — how installed-plugin state is observed
