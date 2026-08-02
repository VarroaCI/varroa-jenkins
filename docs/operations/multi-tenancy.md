# Multi-Tenancy

<!-- sources: api/v1alpha1/types.go (Team, TeamSpec, TeamStatus, ProvisioningDefaultsSpec.Namespaces/DefaultNamespace), internal/controller/team_controller.go, charts/varroa/values.yaml (networkPolicy.tenantNamespaceSelector) -->

Run many teams on one Varroa installation with a **team-per-namespace** model: each team gets its own namespace(s), scoped RBAC, and network isolation, while the platform team owns the control plane, catalogs, and version governance.

## Concepts

- A **`Team`** (cluster-scoped) declares: who's on it (`members` for local users and/or `subjects` for IdP groups/users), which namespaces it owns (`namespaces`, required), and what it may do there (`roleRef`, default `developer`).
- The team controller materializes that into an owned **`Group`** (`status.groupRef`) and a **scoped [`VarroaRoleBinding`](../security/varroa-rbac.md)** (`status.bindingRef`) limited to the team's namespaces.
- With `provisionNamespaces: true`, missing team namespaces are **created and managed**; per-namespace state is reported in `status.namespaceStates`.
- **Deployable namespaces**: which namespaces the creation wizard offers comes from the target cluster's `ProvisioningDefaults` — `defaultNamespace` (preselected; `varroa` when empty) and `namespaces` (additional provisionable targets), intersected with the caller's RBAC create scopes. The RBAC scopes are cluster-agnostic (a `team-a` grant deploys to `team-a` in any cluster). When the target operator is unreachable, the picker degrades: unrestricted callers see a freeform text input while restricted callers see their explicit scopes; a degraded banner warns the user.
- **Isolation**: RBAC scoping bounds the API/dashboard; the chart's [network policies](../install/network-policies.md) (`tenantNamespaceSelector`) bound the network, so only labeled tenant namespaces reach the gateway.

## How to onboard a team

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  displayName: Payments
  subjects:
    - { kind: Group, name: payments-team }     # IdP group claim
  members: [jdoe]                              # local users, if any
  namespaces: [teams-payments]
  roleRef: developer                           # VarroaRole to bind, scoped to the namespaces
  provisionNamespaces: true
```

```bash
kubectl apply -f team.yaml
```

If you run network policies, label the namespace to match your `tenantNamespaceSelector`:

```bash
kubectl label namespace teams-payments varroa.dev/tenant=true
```

**Verify:**

```bash
kubectl get team payments -o jsonpath='{.status}' | jq '{group: .groupRef, binding: .bindingRef, ns: .namespaceStates}'
# group + binding exist; namespaceStates show the namespace Ready/managed
```

Then log in as a team member: the dashboard shows only `teams-payments` targets in the creation wizard, and controllers in other namespaces are invisible.

> [!NOTE]
> **Auto-sync**: Varroa automatically copies each entry from `ProvisioningDefaults.spec.imagePullSecrets` from the operator namespace into every provisioned Team namespace every ~30 seconds (self-healing, idempotently converged). The *source* Secret must still be seeded once in the operator namespace by an admin (e.g. `kubectl create secret docker-registry registry-creds -n varroa ...`); the operator handles the rest.

## How to expose namespaces in the wizard

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata: { name: varroa-defaults }
spec:
  defaultNamespace: teams-payments        # what the wizard preselects
  namespaces: [teams-payments, teams-web] # additional provisionable targets
```

**Verify:** the wizard's namespace dropdown offers exactly these (filtered further by the user's own RBAC scope).

## Concepts: what tenants share vs own

| Shared (platform-owned) | Per-team |
|---|---|
| Control plane, NATS, dashboard | Namespaces + controllers in them |
| [Version profiles](../config/jenkins-versions.md) and plugin governance | [Composed bundles](../config/composed-bundles.md) and bundle repos |
| Operator-namespace [catalog](../config/casc-catalog.md) (teams can shadow locally) | Local catalog items, if any; catalog sources published in the team's namespaces |
| Built-in roles, cluster RBAC objects | Team `Group` + scoped binding |

Teams consume the platform catalog by `itemRef` (local-first resolution lets them override); the platform team stages brood-wide changes with [rollout waves](rollout-waves.md). A Team's scoped binding (on the default `developer` role) also lets members publish their own `CatalogSource`s into the team's namespaces — see [namespace-scoped catalog publishing](../security/varroa-rbac.md#publishing-catalog-content-from-your-own-namespace). Promoting a team's content into the shared platform catalog is a manual operator action, not automatic.

## Troubleshooting

- Team member sees nothing in the dashboard → binding scope vs the namespaces they expect; check `kubectl get varroarolebinding $(kubectl get team payments -o jsonpath='{.status.bindingRef}') -o yaml`.
- Controller pods `ImagePullBackOff` in a new team namespace → check that the source image pull Secret exists in the operator namespace (the auto-sync note above describes how the operator keeps tenant copies in sync once the source exists).
- Controller in a tenant namespace never connects → namespace missing the tenant label while network policies are on.

## Related pages

- [Varroa RBAC](../security/varroa-rbac.md) — the binding the team materializes
- [Network policies](../install/network-policies.md) — the isolation half
- [CasC catalog](../config/casc-catalog.md) — platform/team content sharing
