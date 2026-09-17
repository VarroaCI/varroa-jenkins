# Agent Services

A pipelines tenant gets a Kubernetes Service for its build agents, and its kubernetes cloud sets
`jenkinsUrl` to it. An agent pod outlives the Jenkins JVM that launched it, so its URL must name
an endpoint the operator can repoint. Read [Cells and pipelines tenants](cells.md) first.

## The Service

The name is `varroa-agents-<team>-<slot>`. A tenant holds one Service for each active slot, and
none for a standby: a standby takes the Service of the slot it is promoted into. Read
[Tenant slots](tenant-slots.md).

The Service is a `ClusterIP` in the slot holder's namespace, carrying `http` on 8080 and `agent`
on 50000. Its selector is `app: <pod label of the slot holder>`, which the cell's StatefulSet
stamps.

A Service name is an RFC 1035 label, so the `Team` CRD refuses `spec.pipelines` on a name over 47
characters or carrying a dot.

## The selector follows the placement

`TeamReconciler` converges every slot's Service on each tick, so a deleted Service and a drifted
selector both repair themselves. A move repoints the selector, and the name and ports stay. A
scale-down deletes the Service of each slot it removes.

A move across namespaces leaves a Service of the same name behind. The operator lists each such
namespace in `status.staleAgentServiceNamespaces`, and drops one only after its delete succeeds.
Teardown adds its own namespace, and the `Team` holds its placement finalizer until the list
empties, so no Service outlives its tenant.

A failed write or delete sets `Placed=False` with `AgentServiceFailed`, and retries. A `Team` left
in `Terminating` with that reason awaits one delete: read the condition message.

## Agent traffic never crosses the BFF

An agent dials the Service directly, inside the cluster. The [external runner door](runners.md)
is the one exception.

**Verify:**

```bash
kubectl get svc -n varroa-system varroa-agents-<team>-0 -o yaml
```

Expect two ports and a selector naming the tenant's cell.
