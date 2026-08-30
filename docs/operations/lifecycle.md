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
