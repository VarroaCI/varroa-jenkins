# executeGroovy

<!-- sources: internal/controller/broodoperation_controller.go, api/v1alpha1/types.go, internal/mite/jenkinstoken.go, internal/api/handlers_broodops.go -->

`executeGroovy` is a [brood operation](../operations/brood-operations.md#executegroovy-semantics) verb that runs a Groovy script against each target controller's script console. It is the most powerful thing Varroa can do to a Jenkins controller, so this page covers exactly what it does, who can invoke it, what is recorded, and how to switch it off.

Fleet-wide script execution is table stakes for this category — CloudBees CI ships the same capability. What follows is not a warning against using it; it is the information a security reviewer needs to sign off on it.

## What it does

The operator dispatches directly to the target's Jenkins at `/scriptText` over HTTP on port 8080. This is the **only verb that bypasses the mite**: everything else flows over the gRPC command stream, but the script console has no mite equivalent.

The script itself comes from one of two places, enforced as exactly-one-of by CRD validation:

- **`action.groovy.script`** — an inline script, stored verbatim in the `BroodOperation` object.
- **`action.groovy.itemRef`** — a `groovy`-typed [catalog item](../config/casc-catalog.md). It is resolved once per operation into a deterministically-named, owner-referenced ConfigMap (`<operation>-groovy-script`) and every target in the operation runs those exact bytes. A later edit to the catalog item cannot change what a running operation executes.

`groovy` catalog items are execution-only: the composer rejects them as `ComposedBundle` inputs, so a script that runs with Administer can never be smuggled into a controller's configuration.

## The identity it runs as

The operator mints a short-lived `system:varroa-operator` JWT carrying the `ROLE:varroa:system-operator` authority. That role holds **`hudson.model.Hudson.Administer`**.

That is not a design choice Varroa is free to reverse: Jenkins accepts no permission narrower than `Administer` for `/scriptText`. There is no least-privilege variant of the script console. The risk reduction comes from **credential lifecycle** rather than permission size — the token is minted per dispatch, is short-lived, is never written to disk, and the `VarroaSecurityRealm` plugin verifies it offline against the operator's public key. See [Jenkins RBAC](jenkins-rbac.md#varroa-system-operator-role-details).

Because a script runs as Administer, a caller authorized to invoke `executeGroovy` on a controller is effectively an administrator of that controller. Treat the permission to create brood operations in a namespace as equivalent to Jenkins admin over the controllers in it.

## Who can invoke it

Two independent gates, in this order:

1. **Varroa RBAC.** Creating a `BroodOperation` requires manage-controller permission in the operation's namespace, and operations in the operator's own namespace are admin-only. See [Varroa RBAC](varroa-rbac.md).
2. **Brood policy** (below), which can disable the verb outright or restrict it to named namespaces.

Both gates apply to `kubectl apply` and GitOps as well as the dashboard, because the policy gate lives in the operator. The BFF performs the same policy check before creating an object, but that is user experience, not enforcement: anything that can create a `BroodOperation` object directly never reaches the HTTP API.

## Disabling or restricting it

Set `broodPolicy` on the cluster-scoped `ProvisioningDefaults` singleton:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata:
  name: varroa-defaults
spec:
  broodPolicy:
    executeGroovy:
      enabled: false
```

Or allow it only in specific namespaces:

```yaml
spec:
  broodPolicy:
    executeGroovy:
      allowedNamespaces: [platform-ops]
```

Semantics, stated precisely because the defaults matter:

- **Omitting `broodPolicy`, `executeGroovy`, or `enabled` all mean enabled.** `ProvisioningDefaults` is an optional object, and a policy that only some clusters have must not change behaviour by its absence.
- **`allowedNamespaces` is matched against the `BroodOperation`'s own namespace, not its targets'.** The operation is the thing being authorized, and one operation may fan out to targets in many namespaces. Empty means every namespace.
- **`enabled: false` is the outer gate.** A namespace on the allow-list is still denied when the verb is switched off.
- **A failure to read `ProvisioningDefaults` allows the operation.** Absence and a transient API error are indistinguishable at this layer, and the verb is enabled by default — failing closed would turn an apiserver blip into a cluster-wide outage of a working feature. Varroa RBAC, which has already run, is the authorization boundary; this gate exists to enforce an operator's explicit decision.
- **The policy is re-evaluated on every reconcile, not only at admission.** Disabling the verb while an operation is running stops further dispatch and marks the remaining targets skipped. Targets already dispatched cannot be recalled — their HTTP call is in flight — so expect up to `maxParallel` more executions after the switch.
- **Each cluster enforces its own policy.** In a multi-cluster installation a fan-out creates one `BroodOperation` per target cluster, and each cluster's operator reads its own `ProvisioningDefaults`. Disabling the verb on the control-plane cluster does not disable it on the others; set the policy on every cluster you want it off. The dashboard's pre-check only runs for local-only operations, because it can read only the local policy — a fan-out's per-cluster outcomes appear in the create response.

A denied operation ends immediately in phase `Failed` with `status.reason` naming the policy. `BroodOperationStatus` carries no conditions, so phase and reason are the whole channel.

## What is audited

Every target emits a `broodop.target.finished` [activity event](../operations/observability.md) carrying:

| Field | Value |
|---|---|
| `actor` | the operation's `startedBy` — the authenticated caller |
| `verb` | `executeGroovy` |
| `scriptSource` | `inline` or `catalog` |
| `scriptSha256`, `scriptBytes` | for inline scripts: digest and length |
| `scriptItemRef` | for catalog scripts: `<namespace>/<name>` of the resolved item |
| `scriptSnapshotRef` | for catalog scripts: `<namespace>/<configmap>` holding the exact bytes that ran |

**The event never carries the script body.** Activity is retained for up to 90 days and is readable by anyone who can read activity; Groovy scripts routinely contain credentials.

Failure reasons are filtered for the same reason. Jenkins embeds the response body in a non-2xx error, and a script-console compilation failure echoes the submitted source back in it — so for `executeGroovy` the activity event carries only the HTTP status. The full reason stays on `status.targets[].reason`, which lives and dies with the `BroodOperation` object and is gated by that object's RBAC.

**The snapshot pointer outlives what it points at.** The ConfigMap is owner-referenced by the operation and is deleted with it after `ttlSecondsAfterFinished` — 7 days by default. Activity can be retained for 90. So an event older than the operation's TTL names a ConfigMap that no longer exists. Treat `scriptSnapshotRef` as a correlation key that resolves during the retention window and identifies the run afterwards, not as durable storage. If you need the script text to survive the operation, raise `ttlSecondsAfterFinished`, keep the script in the catalog (where the `CatalogItem` persists independently), or ship the events to a system that captures the ConfigMap alongside them.

For an inline script the digest is the audit record, and it has no such expiry. It proves two runs executed identical scripts, and it matches a run against a script held elsewhere, without publishing the script into a long-retention stream.

Provenance is attached to `target.finished` rather than `run.started` because script resolution happens after the run-started event fires; putting it there would record an empty value.

## Network path

The operator needs egress to target Jenkins pods on TCP/8080. When [network policies](../install/network-policies.md#executegroovy-egress) are enforced, that rule is bimodal on `managedNamespaces`: with the list populated, controllers in unlisted namespaces cannot be reached by `executeGroovy` at all. That is a third, independent control if you want script execution confined to particular namespaces at the network layer as well as the policy layer.

## See also

- [Brood operations](../operations/brood-operations.md#executegroovy-semantics) — the verb's dispatch, timeout, and result semantics
- [Jenkins RBAC](jenkins-rbac.md#varroa-system-operator-role-details) — the operator's in-Jenkins identity
- [CasC catalog](../config/casc-catalog.md) — publishing reusable `groovy` items
