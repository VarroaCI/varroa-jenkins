# Configure Varroa RBAC

Varroa RBAC controls the dashboard, REST API, CLI, and MCP. Configure Jenkins
permissions separately with [Jenkins RBAC](jenkins-rbac.md).

## Use a built-in role

| Role | Intended access |
|---|---|
| `admin` | All Varroa resources and actions |
| `operator` | Controller lifecycle, configuration, approvals, and update-center upload |
| `developer` | Controller and configuration work within the binding scope |
| `viewer` | Read-only access |

Built-in roles are reconciled. Create a custom role when their rules do not fit.

Bind a user or group:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: platform-operators
spec:
  roleRef: operator
  subjects:
    - kind: Group
      name: platform-team
    - kind: User
      name: release-user
```

Subject names match authenticated claims exactly and case-sensitively.

## Restrict the binding

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: payments-developers
spec:
  roleRef: developer
  subjects:
    - kind: Group
      name: payments-team
  scope:
    namespaces:
      - teams-payments
    controllerSelector:
      matchLabels:
        environment: nonproduction
```

When both scope fields are set, a controller must match both. Namespace scope
does not narrow cluster-scoped resources.

## Create a custom role

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRole
metadata:
  name: release-manager
spec:
  apiRules:
    - resources: [controllers]
      verbs: [read, approve-restart, manage]
    - resources: [composedbundles]
      verbs: [read, update]
  jenkinsRoleRef: varroa-operator
```

`apiRules` contain resource and verb lists. Use the exact resource and verb
names exposed by the API. `jenkinsRoleRef` may name only a Global Jenkins role.

For one controller and a built-in role, use `Controller.spec.rbacSpec`:

```yaml
spec:
  rbacSpec:
    groups:
      - name: payments-team
        role: developer
```

The shorthand accepts `admin`, `operator`, `developer`, and `viewer`. Use role
bindings for custom or cross-controller scope.

## Verify access

```bash
curl -sf https://app.example.com/api/v1/me/permissions \
  -H "Authorization: Bearer $VARROA_API_KEY"
```

| Symptom | Check |
|---|---|
| API returns `403` | Effective permissions, subject spelling, and scope |
| Group grant is absent | Resolved groups from `/api/v1/me` |
| Built-in edits disappear | Create a custom role |
| Varroa succeeds but Jenkins denies | Add a Jenkins role assignment |
