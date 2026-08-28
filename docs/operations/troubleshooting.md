# Troubleshooting

Start with the controller status, recent activity, and operator logs.

```bash
varroactl describe controller <namespace>/<name>
varroactl activity --controller <name>
varroactl logs <namespace>/<name> --follow
kubectl get events -n <namespace> --sort-by=.lastTimestamp
```

- `Pending` or `Provisioning`: check namespace eligibility, image pull, storage, and ingress events. Correct the missing dependency, then reconcile.
- Jenkins does not connect: check Pod readiness, gateway reachability, TLS, and network policy.
- Bundle change is absent: inspect composed bundle status and any rollout or reconciliation hold. Correct the bundle, then approve or reconcile.
- `403` or `401`: run `varroactl whoami`, then review [Varroa RBAC](../security/varroa-rbac.md).
- Lifecycle action waits: inspect pending status for a build, power state, hibernation, or approval. See [Reconciliation](reconciliation.md).
- Plugins are unavailable: check the version profile and [Update Center](update-center.md) readiness.

For a support case, provide the controller YAML with secrets removed, `varroactl describe` output, relevant Kubernetes events, and the time range of the failed action. Do not share API keys, passwords, or private bundle credentials.
