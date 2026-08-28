# Brood Schedules

Use a namespaced `BroodSchedule` to create recurring [brood operations](brood-operations.md).

```bash
varroactl broodschedule create daily-reconcile \
  --cron '0 6 * * *' --verb reconcile --names controller-a,controller-b \
  --namespace team-a
```

Use `--selector` for a changing target set. The supported verbs are `restart`, `reprovision`, `reconcile`, `stop`, and `start`. Use `varroactl broodschedule create --help` for target, retention, and deadline options.

## Prevent overlap

| Setting | Default | Effect |
|---|---:|---|
| `concurrencyPolicy` | `Forbid` | Allow, forbid, or replace overlapping trigger Jobs. |
| `waitForCompletion` | `true` | Wait for the operation result. |
| `successfulJobsHistoryLimit` | `3` | Retained successful trigger Jobs. |
| `failedJobsHistoryLimit` | `1` | Retained failed trigger Jobs. |

With `waitForCompletion: true`, `Forbid` covers the full operation duration. With it disabled, the schedule succeeds after the API accepts the operation, so later operations can overlap. Inspect the created brood operation for target outcomes.

## Operate and diagnose

```bash
varroactl broodschedule get
varroactl broodschedule describe <namespace>/<name>
varroactl broodschedule suspend <namespace>/<name>
varroactl broodschedule suspend <namespace>/<name> --resume
varroactl broodschedule delete <namespace>/<name>
```

Team-namespace schedules can target only their namespace and one cluster. Put cross-namespace or multi-cluster schedules in the operator namespace. If no run appears, inspect `status.reason`; `TenancyViolation` means the target is outside that scope. A trigger that cannot start usually lacks an image pull Secret in its namespace.
