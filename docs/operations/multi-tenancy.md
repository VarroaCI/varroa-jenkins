# Multi-Tenancy

Use a Kubernetes namespace for each team or workload boundary. A cluster-scoped `Team` grants Varroa access to its namespaces and can create those namespaces.

## Onboard a team

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  displayName: Payments
  subjects:
    - kind: Group
      name: payments-team
  namespaces:
    - team-payments
  roleRef: developer
  provisionNamespaces: true
```

Apply the resource, then inspect its status:

```bash
kubectl apply -f team.yaml
kubectl get team payments -o yaml
```

`namespaces` must not be empty. `roleRef` defaults to `developer`. `members` names local users; `subjects` names identities from the configured provider.

## Control namespace access

Only configured deployable namespaces accept controller creation. Configure them through the installation values or a `Team`; then use [Varroa RBAC](../security/varroa-rbac.md) to grant API access and [Jenkins RBAC](../security/jenkins-rbac.md) to grant Jenkins permissions.

Teams own their namespace membership and bindings. Do not manually edit generated access objects. If a user gets `403`, verify their identity, Team membership, namespace scope, and role binding. If creation is rejected, verify that the namespace is deployable.
