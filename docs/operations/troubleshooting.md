# Troubleshooting

<!-- sources: CLAUDE.md (gotchas), api/v1alpha1/types.go (conditions/reasons), internal/mite/server.go (keepalive), cmd/mite/agent.go, internal/controller/controller_controller.go -->

Symptom-indexed runbook. Start from what you see; each section gives the diagnosis commands, the usual cause, and the fix. General tools first:

```bash
kubectl get controller <name> -n <ns> -o jsonpath='{.status.phase}'
kubectl get controller <name> -n <ns> -o jsonpath='{.status.conditions}' | jq .
kubectl logs sts/<name> -n <ns> -c mite --tail=100
kubectl logs sts/<name> -n <ns> -c jenkins --tail=100
kubectl get events -n <ns> --sort-by=.lastTimestamp | tail -20
```

Plus the [activity feed](observability.md) and the controller's `GET .../diff` and `.../events` API views.

## Symptom index

- [Controllers stuck after changing cluster.name](#controllers-stuck-after-changing-clustername)
- [Stuck in Pending](#stuck-in-pending)
- [Stuck in Provisioning](#stuck-in-provisioning)
- [Running but never Connected (mite disconnected)](#mite-disconnected)
- [Every controller orphaned after a control-plane reinstall](#control-plane-ca-regen)
- [Pod crash-loops after a version or plugin change](#plugin-prerequisite-abort)
- [Controller stuck with plugin lock conflict](#plugin-lock-conflict)
- [403s after an RBAC change](#403s-after-rbac-changes)
- [Bundle changes not taking effect](#bundle-changes-not-applying)
- [Rollout held (blocked / paused / awaiting approval)](#rollout-held)
- [Pod hangs in Terminating](#pod-hangs-terminating)
- [Status fields look stale or stuck](#stale-status)
- [Hibernated controller won't wake / webhooks not triggering builds](#hibernation-wake-failures)
- [Controller reports ConditionReconcileBlocked](#reconcile-blocked)

## Controllers stuck after changing cluster.name

Mismatched `VARROA_CLUSTER_NAME` between operator/gateway/BFF = structural non-rendezvous (mites connect, commands don't flow). The chart's single `cluster.name` value must feed all three deployments. Old-keyed `mite_desired` entries are abandoned garbage (optionally purge manually).

## Stuck in Pending

**Diagnose:** operator logs — `kubectl logs deploy/varroa-varroa-operator -n varroa | grep <name>`.
**Usual causes:** the operator isn't watching that namespace (`managedNamespaces` excludes it), no operator leader (all replicas down), or the CR failed admission on a recent edit.
**Fix:** add the namespace to `managedNamespaces` (or use a [Team](multi-tenancy.md) namespace), restore the operator, or `kubectl apply` a valid spec.

## Stuck in Provisioning

**Diagnose:** `kubectl get events -n <ns> --sort-by=.lastTimestamp`, `kubectl describe pod <name>-0 -n <ns>`.
**Usual causes:** unschedulable pod (resources, nodeSelector/tolerations from [podOverrides](../config/pod-customization.md)), PVC unbound (no default StorageClass), image pull failure (missing pull secret — in tenant namespaces see gap [#262](multi-tenancy.md)), or the bundle failed composition so provisioning is holding (check the bundle's `status.phase`/`errors` — [Composed bundles](../config/composed-bundles.md)).
**Fix:** address the event; provisioning resumes on the next tick, or [trigger a reconcile](lifecycle.md).

## Mite disconnected

Controller reaches `Running` but never `Connected`, or drops out of `Connected`.

**Diagnose:** mite container logs. The registration and stream mechanics are in [The mite](../architecture/mite.md).
**Usual causes, by log signature:**

| Log says | Cause | Fix |
|---|---|---|
| `bootstrap token already used` / permission-denied on Register | Token consumed (pod restarted mid-bootstrap; replay) | [Re-provision](lifecycle.md) to mint a fresh bootstrap Secret |
| token expired | Pod took longer than the token TTL to start | Re-provision; investigate slow scheduling/pull |
| connection refused / timeout to gateway | [Network policy](../install/network-policies.md) missing tenant label, or gateway down | Label the namespace / restore gateway replicas |
| TLS handshake errors | Mite cert expired while pod was suspended | Usually self-heals via renewal-with-existing-keypair; re-provision if not |
| `GOAWAY ENHANCE_YOUR_CALM` | Keepalive mismatch — client pinging faster than the server floor (custom builds) | Restore paired keepalive settings (client ≥ 30s vs server MinTime 15s) |
| mite gets 401/403 from Jenkins | Operator JWT not yet pushed (operator restart) or security-realm plugin not loaded | Wait one tick; check plugin init container ran; check jenkins logs |
| `x509: certificate signed by unknown authority` (mite) / `invalid or expired bootstrap token` (gateway) | Control-plane was reinstalled and the internal CA was regenerated | Self-heals — see [below](#control-plane-ca-regen) |

## Control-plane CA regen

Reinstalling a hive's `varroa-system` control plane regenerates the internal CA. Existing controllers self-recover — no manual surgery:

- The operator re-mints each controller's bootstrap token when the stored token no longer validates under the new CA's signing key (not just on expiry/format), replacing the old-CA token the gateway would otherwise reject.
- A controller whose mite stays disconnected for a few ticks is reset from `Running`/`Connected` back to `Pending`, which drives a full re-provision that rolls the pod with the new CA (`VARROA_CA_PEM`).
- On restart the mite discards its saved on-PVC identity (`cert.pem`/`key.pem`/`ca.pem`) when the leaf no longer chains to the current CA, then re-bootstraps against the fresh CA instead of looping on `unknown authority`.

Recovery takes a couple of reconcile ticks plus a pod roll. If a controller is still stuck after that, [re-provision](lifecycle.md) it to force the pass immediately.

## Plugin prerequisite abort

Jenkins container exits during startup; logs show `AggregatePluginPrerequisitesNotMetException` (often right after a version change or hand-edited plugin pins).

**Cause:** the plugin set was resolved against a **newer core** than the pod runs — plugins demand a core the image doesn't have. Background in [Plugin pinning](../config/plugin-pinning.md#concepts-the-version-skew-failure-mode).
**Fix:** align core and lock — use a [version profile](../config/jenkins-versions.md) whose `resolveVersion` matches (regenerate custom locks with `hack/gen-plugin-lock.sh` against the exact core), then re-provision.

## Plugin lock conflict

Controller is wedged at 0/0 in `Provisioning`, `ConditionPluginConflict=True`, the
`varroa_controller_plugin_lock_conflict` gauge is 1, and a `pluginConflict.detected`
warning activity event is emitted.

**Cause:** a plugin pin (from a catalog item, `spec.pluginSpec`, or a bundle git/OCI
input) pins a version that no longer matches the active
[JenkinsVersionProfile](../config/jenkins-versions.md) / core lock. This is a
*pin-vs-lock version mismatch*, distinct from the
[version-skew crash-loop](#plugin-prerequisite-abort) caused by
`AggregatePluginPrerequisitesNotMetException`.

**Diagnose:** check the condition message for which source produced the conflicting pin:

```bash
kubectl get controller <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="PluginConflict")]}' | jq .
```

Also check the metric and the [activity feed](observability.md) (not a Kubernetes Event — this
uses Varroa's own activity-bus mechanism, same as other operator-visible signals):

```bash
# Prometheus / OTel metric
varroa_controller_plugin_lock_conflict{namespace,controller} 1
```

Look for a `pluginConflict.detected` warning-severity entry on the controller's Activity tab or
`GET .../events` API view.

**Fix, by source:**

1. **Catalog item** — fix the pin in the source catalog repo/CRD. The `ComposedBundle`
   reconciliation already detects itemRef drift via content-hash comparison
   (`item.Status.ContentHash` vs `observedRevisions[inputKey]`) and automatically
   recomposes on the next tick; no manual deletion of any `<bundle>-content` ConfigMap
   is needed.
2. **`spec.pluginSpec`** — edit the Controller's `spec.pluginSpec.entries` directly.
3. **Bundle git/OCI input** — fix the pin in that git repo or OCI artifact; existing
   drift/digest invalidation picks it up (or trigger a reconcile).
4. Confirm `varroa_controller_plugin_lock_conflict` returns to 0 and the detail-page
   banner disappears; the controller proceeds out of `Provisioning` on its own.

## 403s after RBAC changes

You added/changed a [`JenkinsRoleBinding`](../security/jenkins-rbac.md) but users still get 403 in Jenkins.

**Cause:** known limitation [#218](https://github.com/varroaci/varroa-jenkins/issues/218) — Connected controllers don't apply regenerated role-strategy in place.
**Fix:** [re-provision](lifecycle.md) the affected controllers. For 403s on the **Varroa API** instead, check `/me` capabilities and binding claim values ([Varroa RBAC](../security/varroa-rbac.md)).

## Bundle changes not applying

You pushed a bundle change; the controller still runs the old config.

**Diagnose, in order:**

1. Did the bundle recompose? `kubectl get composedbundle <b> -n <ns> -o jsonpath='{.status.phase}{" "}{.status.resolvedHash}'` — if the hash didn't change: pinned `revision`/`pinnedContentHash`, wrong branch, or composition failed (`status.errors`).
2. Is rollout held? `status.rollout` on the controller — `paused` (annotation) or `blocked` + `waitingOn` (wave gate). See [Rollout waves](rollout-waves.md).
3. Is it parked for approval? `status.pendingRestart` / `status.pendingPluginRoll` (manual mode — [Reconciliation](reconciliation.md)).
4. Did the completeness gate hold it? Unresolved `${variables}` or missing sections keep the last good config; check bundle `status.warnings` and controller conditions.
5. Did the apply fail? The [activity feed](observability.md) and `GET .../diff` show apply results per section.

## Rollout held

`status.rollout.blocked: true` → the wave gate; `waitingOn` lists which lower-wave siblings haven't applied the target hash — go look at *those* controllers.
`status.rollout.paused: true` / condition `RolloutPaused` → the `varroa.dev/rollout-paused` annotation on the bundle; remove it to resume.
Version rolls can also be held by the version-roll gate (condition names the reason) — see [Jenkins versions](../config/jenkins-versions.md).

## Pod hangs Terminating

A controller pod sits in `Terminating` for many minutes during a roll or delete.

**Cause:** the Jenkins preStop hook waits for the mite's drain-done marker; if the mite couldn't complete the drain (or was killed first), the hook waits out its timeout. Long `drainTimeoutSeconds` also legitimately extends termination while builds finish.
**Fix:** for a stuck one-off, `kubectl delete pod <pod> -n <ns> --grace-period=0 --force` (build loss accepted); structurally, tune `drainTimeoutSeconds` ([Reconciliation](reconciliation.md)) and check mite logs for drain errors.

## Stale status

A status field (error, message) seems stuck long after the cause is fixed.

**Diagnose:** compare `status.observedGeneration` (on bundles) with `metadata.generation`; check the operator is the current leader and reconciling (operator logs).
**Fix:** trigger a reconcile; if a specific object's status is wedged, a no-op spec touch (`kubectl annotate controller <name> -n <ns> touch=$(date +%s)`) forces a fresh pass. Persistent stickiness is a bug — please file it with the object YAML.

## Reconcile blocked

**Condition:** `ConditionReconcileBlocked` with one of 19 distinct `Reason`
values; corresponding `status.lastReconcileError` and
`status.lastReconcileErrorAt` carry the latest block reason and timestamp.

This condition means the controller's last reconcile pass hit an error it
could not fully resolve in that single tick. **It does NOT imply**
`Phase=Failed` — most sites are transient and self-heal on the next reconcile
tick (the controller retries with its configured interval). Only 3 of the 19
underlying call sites (two in `handlePending`, one in `handleRunning`) also
flip `Phase=Failed`.

The same block also surfaces in the dashboard as a red banner above the fold
(`ReconcileBlockedBanner`) and in the `varroa_controller_reconcile_blocked`
Prometheus/OTel gauge (1 while blocked, 0 when cleared).

### Reason catalog (19 values)

| Reason | Meaning |
|---|---|
| `BundleRefMissing` | `spec.composedBundleRef` is nil — controller has no bundle to materialize |
| `BundleUnreadable` | The referenced ComposedBundle could not be fetched or its content ConfigMap is missing |
| `ClassResolutionFailed` | The ControllerClass named in `spec.className` was not found (CRD absent) |
| `ServiceReconcileFailed` | Creating or updating the controller's Service failed |
| `AgentRBACFailed` | Creating the agent ServiceAccount / RBAC failed |
| `BootstrapTokenFailed` | Minting or writing the mite bootstrap token Secret failed |
| `PluginsConfigMapFailed` | Writing the plugins ConfigMap (init container) failed |
| `InitConfigMapFailed` | Writing the init ConfigMap (groovy init scripts) failed |
| `CascConfigMapFailed` | Writing the JCasC ConfigMap failed |
| `StatefulSetCreateFailed` | Creating the StatefulSet failed |
| `IngressCreateFailed` | Creating the Ingress failed |
| `MiteConnectTimeout` | Mite did not connect within the provisioned timeout window |
| `WaveGateCheckFailed` | The rollout wave gate check failed (could not compare hashes with lower-wave siblings) |
| `DesiredStatePushFailed` | Pushing the desired state to the mite failed |
| `HibernateTransitionFailed` | Transitioning into Hibernated state failed |
| `FinalizeFailed` | Finalizer logic (on deletion) failed |
| `ScaleDownFailed` | Scaling the StatefulSet to zero (Stopped / Hibernated) failed |
| `UnknownPhase` | The controller's `status.phase` has a value not recognized by the reconciler |
| `PluginConflict` | A composed bundle pins a plugin version that conflicts with the `JenkinsVersionProfile` lock — also surfaced via the dedicated `PluginConflict` condition + a `pluginConflict.detected` activity event (see [Plugin lock conflict](#plugin-lock-conflict) above) |

**Diagnose:** check `status.conditions[?(@.type=='ReconcileBlocked')]`
and `status.lastReconcileError` for the exact message. Operator logs with
`grep <name>` give the full error.

**Fix:** address the underlying resource conflict or missing dependency;
the controller retries automatically. If the condition persists after the
cause is resolved, trigger a reconcile to force a fresh pass (the
automatic clear-on-success runs in the shared defer regardless of phase).

## Still stuck?

Open an issue with: controller YAML (`GET .../yaml`), `status.conditions`, mite + jenkins logs, and the relevant activity events. The [component logs](../architecture/overview.md) to attach for control-plane issues: operator (reconcile decisions), gateway (mite connections), bff (API/auth).

<a id="hibernation-wake-failures"></a>
## Hibernated controller won't wake / webhooks not triggering builds

See [Lifecycle → hibernate](lifecycle.md#how-to-hibernate-idle-controllers) for how the wake path works. Common causes:

- **Wrong or missing wake token.** The wake URL must carry the controller's current `status.wakeToken`. A mismatched token returns `401` with no effect. Re-read the token (`kubectl get controller <name> -n <ns> -o jsonpath='{.status.wakeToken}'`) and confirm the SCM webhook URL matches. If the token was rotated (the field cleared), update the webhook.
- **Webhook pointed at Jenkins directly.** A hit on the controller's own URL now auto-wakes it, which can make the webhook look successful, but the request that arrived while Jenkins was scaled to zero is still lost. Only the `/hibernation/.../queue/...` pipeline durably stores and replays the push event; repoint the SCM webhook there.
- **Raw ingress 503 instead of the wake page.** Navigation wake requires an EndpointSlice-aware ingress implementation (or ordinary ClusterIP routing). Consumers that read only legacy `Endpoints` do not see the operator wake endpoints. ingress-nginx additionally associates a slice only when its name is prefixed by the Service name, so the Varroa slice must remain `<service-name>-wake`; a differently named slice can work through ClusterIP/kube-proxy while ingress-nginx reports `Service does not have any active Endpoint`. Also verify the operator NetworkPolicy permits the ingress-controller namespace on `operator.wake.port`.
- **Wake page unavailable after a Service overlay.** The wake slice maps through the Service port named `http`. A `resourceOverlayService` that renames that port breaks navigation wake without breaking hibernation itself; preserve the port name.
- **Only one address family wakes.** A wake EndpointSlice is single-family, chosen from the first ready operator pod IP. On dual-stack Services the other family has no wake endpoint during hibernation.
- **A Connected controller still shows the interstitial.** This indicates a stale `<service-name>-wake` EndpointSlice, usually after an operator crash or transient delete failure at handoff. A request to the stale page nudges reconciliation, and the Running/Connected healing sweep retries deletion until confirmed. Check operator logs and `kubectl get endpointslice -n <ns> <service-name>-wake`; if it persists, restore EndpointSlice RBAC/API connectivity rather than deleting Jenkins resources.
- **Controller never reaches `Connected` within the hour.** Queued webhooks are retained for 1 hour; if the controller is wedged in `Provisioning`/`Failed`, they age out. Fix the provisioning problem (see above), then rely on the SCM provider's retry or push again.
- **Non-allowlisted webhook path.** Only crumb-exempt webhook endpoints (`github-webhook/`, `generic-webhook-trigger/invoke`) are accepted for replay; other paths return `404`. Use a supported trigger.
- **Never hibernating when you expect it to.** The operator only parks a `Connected` controller with no running builds, an empty queue, and no activity past the grace period; stale mite gauges (a disconnected mite) also block it. Check `status.phase`, `status.lastActivityAt`, and mite connectivity.

## Related pages

- [The mite](../architecture/mite.md) · [Reconciliation](reconciliation.md) · [Rollout waves](rollout-waves.md) · [Observability](observability.md) · [Brood operations](brood-operations.md) · [Lifecycle](lifecycle.md)
# Activity history

- **Disconnected** means retained rows may still be available but the live SSE continuation is not connected.
- **Cursor expired (410)** means stream retention removed the cursor boundary; clear the cursor and rerun the query.
- **Invalid range (400)** means start/end are incomplete, reversed, outside retention, or historical retention is off.
- Empty filtered pages can occur when authorization or sparse filters exclude scanned records; continue while **Load more history** is offered.

# Fleet Operations history

Fleet Operations lists active and completed runs first. Progress is completed targets over total;
outcome counts separate succeeded, failed, and skipped targets. Open a failed operation for its
per-cluster failure context. Timing displays creation time and only displays duration when both
existing start and completion endpoints are available.
