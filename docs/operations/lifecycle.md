# Controller Lifecycle Operations

<!-- sources: api/v1alpha1/types.go (PowerState, BackupSpec, PendingDeletion, RestartDrainStatus), internal/api/handlers.go (reconcile/reprovision/restart routes), internal/controller/controller_controller.go -->

Day-2 operations on a controller: stop/start, restarts, re-provisioning, on-demand reconciles, deletion, and backups. The phase model behind these is in the [Architecture overview](../architecture/overview.md); the API base for the calls below is `https://app.example.com/api/v1`.

The controller workspace keeps lifecycle state and actions in its persistent header. Overview summarizes readiness, bundle health, probes, and executor activity; Configuration owns editable desired state (including the three-tier Spec Editor — see [Pod customization](../config/pod-customization.md#the-spec-editor-dashboard-ui)); Observability owns metrics; and Diagnostics contains connection conditions, Activity, and live Logs. Power, restart, and reprovision controls require `controllers:manage`, while Reconcile remains available to existing controller viewers.

## How to stop and start (power state)

`spec.powerState` scales the StatefulSet: `Running` (default/empty) keeps one replica, `Stopped` scales to zero — the PVC and all configuration stay.

```bash
kubectl patch controller demo -n teams-platform --type merge -p '{"spec":{"powerState":"Stopped"}}'
```

**Verify:** `kubectl get sts demo -n teams-platform -o jsonpath='{.spec.replicas}'` → `0`; `status.phase` → `Stopped`. Set it back to `Running` and the controller returns through `Provisioning → Running → Connected`.

Use `Stopped` as a big red switch during incident response, or for controllers you want held down until a human brings them back. For automatic cost savings on merely-idle controllers, use **hibernation** (below) instead — it wakes itself on incoming webhooks.

## How to hibernate idle controllers

Hibernation scales an idle controller to zero like `Stopped`, but the controller **wakes automatically** when an SCM webhook or a user request arrives. Enable it per controller:

```bash
kubectl patch controller demo -n teams-platform --type merge -p '{"spec":{"hibernation":{"enabled":true,"gracePeriodMinutes":60}}}'
```

`spec.hibernation` fields:

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Turn auto-hibernation on for this controller. |
| `gracePeriodMinutes` | `60` | Idle time before the operator parks the controller (minimum `5`). |
| `activityIgnoreRegex` | — | Optional regex of request paths that do **not** count as activity (appended to the built-in exclusions for mite/probe/static traffic). **Changing this rolls the pod.** |

The operator hibernates a controller only when it is `Connected`, has **no running builds and an empty queue**, and has seen no activity — HTTP requests, build events, or a fresh connection — for longer than the grace period. It then sets `spec.powerState: Hibernated`; the StatefulSet scales to zero through the same graceful [mite drain](../architecture/mite.md) as a stop.

**Verify:** `status.phase` → `Hibernated`; `kubectl get sts demo -n teams-platform -o jsonpath='{.spec.replicas}'` → `0`.

Opening a hibernated controller's normal Jenkins URL now wakes it automatically. The
operator serves a **Waking Controller** interstitial, changes the controller back to
`Running`, scales Jenkins up, and redirects the browser to the originally requested
deep link once Jenkins is ready. `Stopped` is deliberately different: its URL still
returns `503` and traffic never starts it.

Navigation wake is token-less because a request can wake only the controller whose own
URL it reached. That also means **any HTTP hit** on the controller URL wakes it. Bots and
scanners can therefore keep a publicly exposed controller awake and defeat hibernation;
restrict host exposure or set `operator.wake.enabled=false` if that trade-off is not
acceptable.

### Keeping webhooks working

Point your SCM provider's webhook at the Varroa **wake URL** instead of the controller directly. Each controller has a secret wake token in its status:

```bash
kubectl get controller demo -n teams-platform -o jsonpath='{.status.wakeToken}'
```

Register this URL (GitHub example) — it works whether the controller is awake or hibernated:

```
https://<dashboard-host>/hibernation/<wakeToken>/clusters/<cluster>/ns/teams-platform/queue/demo/github-webhook/
```

When the controller is hibernated, Varroa **durably queues** the webhook, wakes the controller, and replays the request once it reaches `Connected` (the queue is bounded to a 1-hour retention; a controller that fails to wake within the hour drops those payloads and relies on the provider's own retries). When the controller is already awake, the request is delivered promptly. A request with a wrong or missing token is rejected with `401` and has no effect.

For humans, the dashboard shows a **Wake** affordance on a hibernated controller; you can also open the `redirect` form of the URL to get a "waking up…" page that forwards to Jenkins once it is ready.

### Timer/cron triggers

A hibernated controller cannot fire its own `TimerTrigger` (cron) builds while parked. When the operator hibernates a controller that has timer-triggered jobs, it raises a non-blocking **`HibernationCronTriggersSkipped`** condition (surfaced as a warning badge in the dashboard). If a controller must run scheduled builds on its own clock, do not enable hibernation for it — or drive those builds from an external scheduler that hits the wake URL.

## How to restart

Via the API (or the controller page in the dashboard):

```bash
# Restart: delete the Jenkins pod; the StatefulSet recreates it
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" $API/clusters/core/controllers/teams-platform/demo/restart
```

The restart endpoint deletes the pod. Whatever build draining happens is the pod termination path's job ([mite shutdown coordination](../architecture/mite.md)), bounded by the pod's grace period — the endpoint itself does not quiet Jenkins down first. (Earlier releases exposed separate `safe-restart`/`hard-restart` endpoints that behaved identically; they were collapsed — see issue #275.)

The drain-aware restart with `status.restartDrain` backoff tracking is the **approval** path: approving a pending config change with action `restart` pushes the desired state with idle gating and restarts once builds allow, bounded by the controller's `drainTimeoutSeconds` ([Reconciliation](reconciliation.md)).

**Verify:** pod restarts; controller returns to `Connected`.

## How to trigger a reconcile or re-provision

```bash
# Nudge the reconciler now instead of waiting for the interval tick:
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" $API/controllers/teams-platform/demo/reconcile

# Full re-provision: rebuild init config, StatefulSet, RBAC, ingress from scratch
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" $API/controllers/teams-platform/demo/reprovision
```

Re-provision is the heavy tool: it transitions the controller back to `Provisioning`, re-renders everything the operator generates, and rolls the pod. Use it after RBAC changes that must apply immediately (limitation [#218](../security/jenkins-rbac.md)), or when a controller's generated resources have been damaged by hand.

**Verify:** `kubectl get controller demo -n teams-platform -w` shows the phase walk back to `Connected`.

## How to configure backups

```yaml
spec:
  backupSpec:
    enabled: true
    schedule: "0 3 * * *"        # cron, cluster time
    retentionDays: 14
```

**Verify:** `kubectl get controller demo -n teams-platform -o jsonpath='{.spec.backupSpec}'` reflects the config, and backup runs appear on the schedule (see the controller's events/activity feed).

> [!NOTE]
> Backups cover `$JENKINS_HOME` state (job history, build records). Your *configuration* shouldn't need restoring — it's declarative: bundle + CRDs recreate it. Treat backup/restore as build-history protection, not config management.

## How to delete a controller

```bash
kubectl delete controller demo -n teams-platform
```

The operator tears down the owned resources (StatefulSet, Services, Ingress, ConfigMaps, the bootstrap Secret). A Secret referenced via `spec.ingressSpec.tlsSecretName` — an existing TLS secret or cert-manager-created — is **not** deleted; you or cert-manager own its cleanup. The PVC follows your StorageClass reclaim policy — check before assuming the data is gone or kept.

Deletion of *items inside* a running controller (jobs removed from a bundle) is a different, guarded flow — `status.pendingDeletions` plus approval — documented in [Items](../config/items.md#concepts-removing-items).

**Verify:** `kubectl get all -n teams-platform -l app=demo` returns nothing.

## Reference: inspection endpoints

| Call | Returns |
|---|---|
| `GET $API/controllers/{ns}/{name}` | full detail DTO (spec + status + computed) |
| `GET .../yaml` | the CR as YAML |
| `GET .../logs` | Jenkins container logs |
| `GET .../events` | Kubernetes events for the controller's resources |
| `GET .../diff` | pending desired-vs-live config diff |
| `GET .../mite/stream` | live mite stream state |

## Troubleshooting

- Stuck in `Provisioning` after reprovision → check `kubectl get events -n <ns>` for scheduling/PVC errors; see [Troubleshooting](troubleshooting.md).
- Approved (drain-aware) restart never completes → a build refuses to finish; `status.restartDrain.lastReason` says so — raise `drainTimeoutSeconds` or use the restart endpoint deliberately.
- Deleted controller left a PVC → your StorageClass reclaim policy is `Retain`; delete the PVC explicitly if intended.

## Related pages

- [Reconciliation](reconciliation.md) — modes, intervals, and approvals behind these actions
- [The mite](../architecture/mite.md) — drain and restart mechanics
- [Items](../config/items.md) — guarded item deletion
- [Scaling](../architecture/scaling.md) — resources and presets
- [Brood operations](brood-operations.md) — bulk operations across multiple controllers
