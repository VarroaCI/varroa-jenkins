# Rollout Waves

Use rollout waves to apply a bundle change in stages. Set an integer `rolloutWave` in each controller's reconciliation policy. Lower values apply first; `0` is the first wave.

```yaml
spec:
  reconciliationPolicy:
    rolloutWave: 1
```

Each controller waits until earlier-wave controllers using the same bundle have applied the target successfully. Only Connected earlier-wave controllers gate later waves; a controller in any other phase is skipped and does not block them.

A Connected earlier-wave controller clears the gate only when all of the following hold: its applied bundle hash matches the target, its last apply succeeded, and its mite is connected with a heartbeat newer than the stale threshold. A controller whose apply succeeded but whose mite has gone quiet still blocks the wave, so check heartbeat freshness before assuming a stalled rollout is an apply failure.

## Operate a rollout

Inspect the controller and rollout status. Fix the failed controller, then approve or reconcile it as required by the configured reconciliation policy. To halt a rollout, pause the affected composed bundle; resume only after validating the corrected configuration.

Use [Composed bundles](../config/composed-bundles.md) for bundle lifecycle and [Reconciliation](reconciliation.md) for approval behavior.
