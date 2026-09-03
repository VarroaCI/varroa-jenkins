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
- Operator, gateway, or BFF logs `nats disconnected` followed by repeated `nats async error` with `authorization violation`, and the operator pod goes NotReady: the NATS password rotated. Expect this to clear on its own. The components re-read the mounted Secret on every reconnect, so they reconnect once kubelet has synced it. That takes about a minute. Confirm the operator returns to Ready and that `varroa.bus.connected` returns to 1.
- Those same authorization errors continue well past a minute, and the operator stays NotReady: the component cannot see the rotated password. Its pod spec supplies the bus password as an environment variable instead of `-bus-pass-file`, which freezes the value at pod start. A component in this state retries the stale credential indefinitely, so its log lines are the same as the recoverable case above. The recovery window is the only thing that separates them. Upgrade the release, then run `kubectl rollout restart deployment/<release>-varroa-operator deployment/<release>-varroa-gateway deployment/<release>-varroa-bff`. Restarting during a rotation window is safe: a component that starts before its Secret has synced waits for the credential and stays NotReady, rather than exiting into a crash loop.
- Controller sits in `Provisioning` and the pill shows Boot failed: read `kubectl logs <pod> -c jenkins -n <namespace>`. A JCasC boot error such as `Cloud must have a unique non-empty name` exits with code 5. Fix the bundle. The operator rolls the pod as soon as the rendered CasC content changes.
- Pill shows Blocked with `plugin X requested at A conflicts with profile lock B`: every plugin pinned in a bundle must equal the `JenkinsVersionProfile` lock for the controller's Jenkins version. Read the lock with `kubectl get configmap jenkins-version-<v>-pluginset-content -n <operator-namespace> -o jsonpath='{.data.plugins\.yaml}'`, then pin that plugin to `B` where the pin lives. The message names the source: `spec.pluginSpec` means the controller's own spec, and `the bundle` means the catalog or bundle entry. A bundle composed from catalog items does not recompose on a catalog change by itself. After the catalog re-syncs, delete the bundle's `<name>-content` ConfigMap so the operator re-materializes it. The controller reconciles through on the next tick.

For a support case, provide the controller YAML with secrets removed, `varroactl describe` output, relevant Kubernetes events, and the time range of the failed action. Do not share API keys, passwords, or private bundle credentials.
