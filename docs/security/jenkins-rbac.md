# Configure Jenkins RBAC

`JenkinsRole` defines Jenkins permissions. `JenkinsRoleBinding` assigns a role
to users or groups on selected controllers and Jenkins objects.

Varroa owns the role-strategy configuration of managed controllers. It drops
JCasC `authorizationStrategy` entries and manual role edits.

## Grant folder build access

```yaml
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRole
metadata:
  name: payments-builder
spec:
  roleType: Item
  permissions:
    - hudson.model.Item.Read
    - hudson.model.Item.Build
    - hudson.model.Item.Cancel
---
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRoleBinding
metadata:
  name: payments-builders
spec:
  roleRef: payments-builder
  subjects:
    - kind: Group
      name: payments-team
  controllerScope:
    namespaces:
      - teams-payments
  jenkinsScope:
    type: Folder
    folder: payments
    propagate: Subtree
```

```bash
kubectl apply -f jenkins-rbac.yaml
```

Connected controllers get role changes without reprovisioning.

## Select scopes

| Field | Values | Effect |
|---|---|---|
| `roleType` | `Global`, `Item`, `Agent` | Permission category, default `Global` |
| `controllerScope.namespaces` | Namespace names | Limits selected controllers |
| `controllerScope.controllerSelector` | Label selector | Limits selected controllers |
| `jenkinsScope.type` | `Global`, `Folder`, `Pattern` | Limits Jenkins objects |
| `jenkinsScope.propagate` | `None`, `Children`, `Subtree` | Controls folder inheritance |

Pattern scope matches item names with a regular expression:

```yaml
jenkinsScope:
  type: Pattern
  pattern: "payments/.*-production"
```

## Link Varroa and Jenkins access

A `VarroaRole` can set `jenkinsRoleRef` to a Global `JenkinsRole`. Its bindings
grant control-plane permissions and that Jenkins role within the same
controller scope. Use a separate `JenkinsRoleBinding` for Item or Agent roles.

Varroa reconciles the built-in Jenkins roles `varroa-admin`,
`varroa-operator`, `varroa-developer`, `varroa-viewer`,
`varroa-system-mite`, and `varroa-system-operator`. Create custom roles instead
of editing built-ins. See [executeGroovy security](execute-groovy.md) for the
operator role.

## Federate roles to hive clusters

Varroa copies each core `VarroaRoleBinding` that grants a Global Jenkins role
to every hive as a `varroa-fed-*` `JenkinsRoleBinding` and `JenkinsRole`. It
skips a binding with no subjects, a `tenants` scope, or a reserved `system-*`
role, and deletes its earlier copy. The BFF logs a failed copy as
`rbac federation warning`.

## Troubleshoot

| Symptom | Check |
|---|---|
| Jenkins returns `403` | Subject, scope, role, and permission |
