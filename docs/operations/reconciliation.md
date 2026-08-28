# Reconciliation

Reconciliation compares a controller's desired configuration with its running Jenkins instance. Use the configured policy to control when disruptive changes apply.

## Choose a mode

| Mode | Behavior |
|---|---|
| `automatic` | Apply desired state when drift is detected. |
| `idle` | Defer an apply while builds run, up to the configured maximum. |
| `manual` | Hold changes until an authorized user approves them. |

Configure the policy in `ProvisioningDefaults` or the controller specification. `interval` sets the steady-state check interval, `drainTimeoutSeconds` controls the Jenkins drain before restart-class changes, and `maxDeferSeconds` bounds idle-mode delay. The controller status reports pending changes and their reason.

## Approve or investigate a change

```bash
varroactl describe controller <namespace>/<name>
varroactl approve controller <namespace>/<name>
varroactl reconcile controller <namespace>/<name>
```

Approval applies the pending action when safety checks permit it. A running build, rollout-wave gate, or maintenance window can delay work. Inspect the controller status and activity feed, then use [Troubleshooting](troubleshooting.md) if the reason does not clear.
