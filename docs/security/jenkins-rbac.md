# Configure Jenkins RBAC

`JenkinsRole` defines Jenkins permissions. `JenkinsRoleBinding` assigns a role
to users or groups on selected controllers and Jenkins objects.

Varroa owns the managed controllers' role-strategy configuration. Configure it
with these resources. JCasC `authorizationStrategy` entries and manual Jenkins
role edits are not retained.

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

Connected controllers receive role changes without reprovisioning.

## Select scopes

| Field | Values | Effect |
|---|---|---|
| `roleType` | `Global`, `Item`, `Agent` | Permission category, default `Global` |
| `controllerScope.namespaces` | Namespace names | Limits selected controllers |
| `controllerScope.controllerSelector` | Label selector | Limits selected controllers |
| `jenkinsScope.type` | `Global`, `Folder`, `Pattern` | Limits Jenkins objects |
| `jenkinsScope.propagate` | `None`, `Children`, `Subtree` | Controls folder inheritance |

Pattern scope accepts a regular expression over full item names:

```yaml
jenkinsScope:
  type: Pattern
  pattern: "payments/.*-production"
```

Prefer folder scope when a folder is the security boundary.

## Link Varroa and Jenkins access

A `VarroaRole` can set `jenkinsRoleRef` to a Global `JenkinsRole`. Its bindings
then grant both control-plane permissions and that Jenkins role within the same
controller scope. Use a separate `JenkinsRoleBinding` for Item or Agent roles.

Varroa reconciles the built-in Jenkins role resources `varroa-admin`,
`varroa-operator`, `varroa-developer`, `varroa-viewer`,
`varroa-system-mite`, and `varroa-system-operator`. Create custom roles instead
of editing built-ins. The privileged operator role is covered by
[executeGroovy security](execute-groovy.md).

## Troubleshoot

| Symptom | Check |
|---|---|
| Jenkins returns `403` | Subject, scope, role, and permission |
| JCasC authorization disappears | Use Jenkins RBAC resources |
| Manual role edit reverts | Edit the Varroa resource |
| Job action fails | Add the required Item grant |
