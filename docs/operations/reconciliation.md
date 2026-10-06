# Reconciliation

Reconciliation compares a controller's desired configuration with its running Jenkins instance. The policy controls when disruptive changes apply.

## Choose a mode

| Mode | Behavior |
|---|---|
| `automatic` | Apply desired state when drift is detected. |
| `idle` | Defer an apply while builds run, up to the configured maximum. |
| `manual` | Hold content changes until approval; placement and rehydration data apply. |

Configure the policy in `ProvisioningDefaults` or the controller specification. `interval` sets the check interval, `drainTimeoutSeconds` limits the drain before a restart, and `maxDeferSeconds` limits idle-mode delay.

## Approve or investigate a change

```bash
varroactl describe controller <namespace>/<name>
varroactl approve controller <namespace>/<name>
varroactl reconcile controller <namespace>/<name>
```

Approval applies the pending action when safety checks permit. A running build, rollout wave, or maintenance window can delay it. A busy reconcile can fail an approval after 20 seconds; retry it. An approval refuses content that changed after detection and names the new sections; review them and approve again. A client that omits `expectedContentHash` approves the current pending change. Then check the status, activity feed, and [Troubleshooting](troubleshooting.md).
