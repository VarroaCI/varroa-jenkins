# Controller Lifecycle Operations

Use the dashboard or `varroactl` for lifecycle actions. Controller phases are described in the [architecture overview](../architecture/overview.md).

## Stop, start, hibernate, and wake

```bash
varroactl power controller <namespace>/<name> stopped
varroactl power controller <namespace>/<name> running
varroactl hibernate controller <namespace>/<name>
varroactl wake controller <namespace>/<name>
```

`Stopped` scales Jenkins to zero while keeping its storage and configuration. It does not wake for traffic or webhooks. Starting returns it through provisioning.

Hibernation is separate from `spec.powerState`. Enable automatic hibernation with:

```yaml
spec:
  hibernation:
    enabled: true
    gracePeriodMinutes: 60
```

The grace period defaults to 60 minutes and cannot be below 5. Automatic hibernation needs a connected controller with no build, queue, or recent activity. On-demand hibernation can interrupt a build. Hibernate and wake fail while the controller is stopped.

Use Varroa's queue URL for supported SCM webhooks. Direct requests to sleeping Jenkins can be lost. A hibernated controller cannot run timer-triggered jobs.

On a dedicated controller, a GitLab delivery to the queue URL's `generic-webhook-trigger/invoke` path can carry the job token in the GitLab **Secret token** field (`X-Gitlab-Token`) or as `?token=`. The query value wins when both are present. A delivery with no token starts only jobs whose `genericTrigger` has no token. The tenant door requires `?token=<team>:<name>` and strips `X-Gitlab-Token`, so a tenant replay carries no GitLab token header.

A [headless](../security/headless-controllers.md) controller hibernates the same way. Webhooks still queue and replay once it wakes. The human wake redirect page is not served for it. It answers `404` instead. A headless controller has no URL to redirect the browser to.

Varroa usually wakes a controller within about 1 second of a wake request. Occasionally the operator drops the wake request because its event queue is full. The controller then wakes on the next periodic check. With the default 30-second reconciliation interval, that is up to about 36 seconds later. A configured `reconciliationPolicy.interval` raises the bound to about 120% of that interval. This delay is expected behavior, not a fault.

## Restart, reconcile, or reprovision

```bash
varroactl restart controller <namespace>/<name>
varroactl reconcile controller <namespace>/<name>
varroactl reprovision controller <namespace>/<name>
```

| Action | Use |
|---|---|
| Restart | Recreate the Jenkins pod. |
| Reconcile | Check desired state now. |
| Reprovision | Rebuild operator-managed resources and roll Jenkins. |

For approval and drain behavior, see [Reconciliation](reconciliation.md).

A CASC configuration change (JCasC content, the security realm, or Jenkins
RBAC) made while a controller is still provisioning rolls the Jenkins pod
automatically, even if the controller is crash-looping and never reaches
`Running`. Manual `Restart` is not needed to pick up the change during
provisioning.

This automatic roll assumes the StatefulSet's default `RollingUpdate`
strategy. A `resourceOverlay` that sets `spec.updateStrategy` to `OnDelete`
takes ownership of pod recycling. This is the same limitation as the
plugin-checksum roll. After such an overlay, deleting the Jenkins pod by hand
is required to pick up a CASC or plugin change.

## Resolve field conflicts

A conflicting field manager, such as a GitOps controller, can block a change. See [Manage field ownership](../agents/writing.md#manage-field-ownership) for the full procedure.

```bash
varroactl patch controller <namespace>/<name> \
  -p '{"spec":{"version":"<version>"}}' --force
```

`--force` transfers ownership. Use it as a recovery action, not routine workflow.

## Delete and verify

```bash
varroactl delete controller <namespace>/<name>
varroactl describe controller <namespace>/<name>
varroactl activity --controller <name> --follow
```

Deletion removes operator-owned workload resources. An externally supplied TLS Secret is kept. Storage follows the StorageClass reclaim policy, so back up required Jenkins data first. If progress stops, use [Troubleshooting](troubleshooting.md).
