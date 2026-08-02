# Reconciliation

<!-- sources: api/v1alpha1/types.go (ReconciliationPolicy, ReconciliationMode, PendingRestart), internal/controller/controller_controller.go (handleConnected), internal/api/handlers.go (apply actions) -->

How and when a controller converges on its desired state — and how much control humans keep over the moment configuration actually lands. Set per controller in `spec.reconciliationPolicy`, with brood defaults from `ProvisioningDefaults.defaultReconciliationPolicy`.

In the controller workspace, Configuration presents reconciliation in operational order: desired and applied hashes, SOURCE/COMPOSE/DESIRE/DELIVER/LIVE stages, validation and diff controls, guarded apply/retry actions, then newest-first apply history. Editing bundle, probes, policy, hibernation, version, or overlays requires `controllers:update`; pipeline restart/reload approvals require `controllers:approve-restart`, and reprovision requires `controllers:manage`.

## Concepts

In the `Connected` phase, the operator re-evaluates the controller every `interval` tick: it resolves the [composed bundle](../config/composed-bundles.md), applies the [version profile](../config/jenkins-versions.md), regenerates [RBAC](../security/jenkins-rbac.md), computes the desired-state hash, and — if the hash differs from what the mite last applied — decides what to do based on **mode**. A convergence short-circuit keeps steady-state ticks cheap: identical hashes push nothing.

```yaml
spec:
  reconciliationPolicy:
    mode: idle              # automatic | idle | manual
    interval: 60s           # min 10s; default 30s (or brood default)
    maxDeferSeconds: 1800   # idle mode: max wait for builds to finish (0 ⇒ 1800)
    drainTimeoutSeconds: 900 # quiet-down bound before restart-class changes (0 ⇒ disabled)
    rolloutWave: 0          # see Rollout waves
```

| Mode | On drift |
|---|---|
| `automatic` (default) | Push desired state immediately |
| `idle` | Defer while builds are running, up to `maxDeferSeconds`, then push anyway |
| `manual` | Park the change for human approval |

Two more dials interact with pushes:

- **`drainTimeoutSeconds`** — before a restart-class change (one that needs Jenkins to restart), the controller is quiet-downed and builds get up to this long to finish; `0` disables draining (restart immediately). Backoff state for a drain that can't complete is in `status.restartDrain`.
- **`interval`** — the steady-state tick. Longer intervals reduce operator load per controller ([Scaling](../architecture/scaling.md)) at the cost of drift-detection latency.

## How to run a controller in manual mode

```bash
kubectl patch controller demo -n teams-platform --type merge \
  -p '{"spec":{"reconciliationPolicy":{"mode":"manual"}}}'
```

From now on, drift parks instead of applying:

```bash
kubectl get controller demo -n teams-platform -o jsonpath='{.status.pendingRestart}' | jq .
# { "detectedAt": "…", "desiredStateHash": "…", "changes": ["config", "rbac"] }
```

`changes` names the drifted sections (`plugins`, `config`, `rbac`, `items`). Plugin-roll drift parks separately as `status.pendingPluginRoll` ([Plugin pinning](../config/plugin-pinning.md)).

Approve via the dashboard's controller page, or the API's apply endpoint:

```bash
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" \
  https://app.example.com/api/v1/clusters/core/controllers/teams-platform/demo/apply \
  -d '{"action": "approve"}'
```

Apply actions: `approve` (apply the parked change), `reload` (JCasC reload only), `restart` / `force-restart` (restart-class apply), `force` (apply now regardless of drain), `plugin-roll` (approve a parked plugin roll). Approval rights are the `approve-restart` verb in [Varroa RBAC](../security/varroa-rbac.md).

**Verify:** after approving, `status.pendingRestart` clears and the mite's apply results land clean (`GET .../diff` shows no pending sections).

## How to tune build-friendly convergence

For controllers where interrupting builds is expensive, `idle` mode plus a generous drain:

```yaml
spec:
  reconciliationPolicy:
    mode: idle
    maxDeferSeconds: 3600      # wait up to an hour for a quiet moment
    drainTimeoutSeconds: 1800  # then give builds 30 min to finish before restart
```

**Verify:** push a bundle change while a long build runs; the apply defers (visible in the controller's activity feed) and lands after the build — or at the deadline.

## Concepts: what needs a restart vs a reload

Not every change restarts Jenkins. JCasC-only changes are applied by the mite through a config reload; plugin set changes and image/version changes are restart-class (StatefulSet roll through `Provisioning`). The apply pipeline picks the cheapest sufficient action; the `changes` list on a parked approval tells you which class you're approving.

## Troubleshooting

- Change never applies in `idle` mode → builds never stop; `maxDeferSeconds` caps the wait — check the deferral in the activity feed.
- `manual` controller drifts forever → nobody approved it; `status.pendingRestart.detectedAt` shows how long.
- Applies happen but config reverts → something else is fighting (an overlay on a managed field, or hand-edits in Jenkins that reconciliation reverts — that's the design).
- Interval change has no effect below 10s → CRD validation floor.

## Related pages

- [Rollout waves](rollout-waves.md) — ordering applies across controllers
- [Plugin pinning](../config/plugin-pinning.md) — the plugin-roll approval path
- [Lifecycle](lifecycle.md) — manual restarts, reprovision, drains
- [The mite](../architecture/mite.md) — the agent that executes applies
