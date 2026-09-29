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

### Read the federation status

Each core `VarroaRoleBinding` reports its result on each hive in
`status.federation`. The BFF writes one entry per hive cluster. The core
cluster has no entry. A hive that leaves the fleet loses its entry on the next
pass.

```bash
kubectl get varroarolebinding team-admins -o jsonpath='{.status.federation}'
```

| `state` | `reason` | Meaning |
|---|---|---|
| `federated` | `Federated` | The hive holds the `JenkinsRole` and the `JenkinsRoleBinding`. |
| `skipped` | `NoSubjects` | The binding has no subjects. |
| `skipped` | `TenantScope` | The scope names `tenants`. A tenant grant stays on the core cluster. |
| `skipped` | `RoleNotFound` | The `roleRef` names no `VarroaRole`. |
| `skipped` | `NoJenkinsProjection` | The role sets no `jenkinsRoleRef` and no `jenkinsPermissions`. |
| `skipped` | `JenkinsRoleNotFound` | The role's `jenkinsRoleRef` names no core `JenkinsRole`. |
| `skipped` | `NonGlobalJenkinsRole` | The referenced `JenkinsRole` is not Global. |
| `skipped` | `ReservedRole` | The role projects onto a reserved `system-*` role. |
| `failed` | `HiveUnavailable` | The BFF cannot list the RBAC objects on the hive. |
| `failed` | `JenkinsRoleCollision` | A `JenkinsRole` that Varroa does not own has the same name. |
| `failed` | `JenkinsRoleWriteFailed` | The hive refused the `JenkinsRole` write. |
| `failed` | `JenkinsRoleBindingCollision` | A `JenkinsRoleBinding` that Varroa does not own has the same name. |
| `failed` | `JenkinsRoleBindingWriteFailed` | The hive refused the `JenkinsRoleBinding` write. |
| `failed` | `StaleJenkinsRoleBindingDeleteFailed` | The hive refused the delete of an earlier `JenkinsRoleBinding` copy. The earlier copy still grants its subjects. |

A skip applies to every hive. `message` gives the detail, such as the hive
error text. `lastTransitionTime` changes only when the state, reason, or message
changes. `status.observedGeneration` names the spec generation the entries
describe. The BFF retries a failed hive on each pass, and the entry returns to
`federated` when the fault clears. To clear a collision, rename or delete the
object on the hive that Varroa does not own.

A change to the subjects or the scope of a binding gives its copy a new name.
The BFF creates the new copy and deletes the earlier copy. Each copy carries
the annotation `varroa.dev/federated-bindings`, which names the core bindings
that own it. When the hive refuses the delete, each owner reports `failed` with
reason `StaleJenkinsRoleBindingDeleteFailed` on that hive. This state replaces
a skip, because the earlier copy still grants access. A binding that loses its
last subject is an example. A copy with no annotation reports on every projected
core binding whose `roleRef` equals the copy's `varroa.dev/federated-from` label. The
BFF retries the delete on each pass. The entry changes when the delete
succeeds.

When no owner of the earlier copy exists on the core, no status changes. The
BFF log shows `rbac federation warning` with the text
`cluster <name>: delete orphaned JenkinsRoleBinding <copy>`.

The dashboard Role Bindings page shows a count of hives per state. The edit
page of a binding shows one row per hive. The MCP `get_varroa_role_binding`
tool returns the status, and `list_varroa_role_bindings` returns the
`hivesFederated`, `hivesSkipped`, and `hivesFailed` counts.

### Read a stale role copy status

The BFF deletes a federated `JenkinsRole` copy on a hive when no binding needs
it. A role copy becomes stale in three cases:

- The `jenkinsRoleRef` of a `VarroaRole` changes.
- The `VarroaRole` is deleted.
- Every binding of the role goes away, loses its last subject, or gets a
  `tenants` scope.

Each role copy carries the annotation `varroa.dev/federated-roles`. It names
each core `VarroaRole` that projects onto the copy through a projected
binding. When the hive refuses the delete, each owner reports the fault in its
own `status.federation`. `status.observedGeneration` is set only when the BFF
writes the role status, so it is not a health signal:

```bash
kubectl get varroarole team-admin -o jsonpath='{.status.federation}'
```

| `state` | `reason` | Meaning |
|---|---|---|
| `failed` | `StaleJenkinsRoleDeleteFailed` | The hive refused the delete of an earlier `JenkinsRole` copy. |

A role status holds an entry only for a hive with a fault. A healthy role has
no entries. The message names the copy and the hive error. A copy with no
annotation reports on the `VarroaRole` that its `varroa.dev/federated-from`
label names. The BFF retries the delete on each pass. The BFF removes the entry
when the delete succeeds. A hive that the BFF cannot list keeps its entry. A
hive that leaves the fleet loses its entry.

A stale role copy grants nothing while no `JenkinsRoleBinding` names it. But a
later binding that names the same role gets the earlier permission set. When
the fault remains, delete the copy on the hive.

When the `VarroaRole` is deleted, no status changes. The BFF log shows
`rbac federation warning` with the text
`cluster <name>: delete orphaned JenkinsRole <copy>`.

The dashboard Roles page shows a count of failed hives next to a role with
entries.

## Troubleshoot

| Symptom | Check |
|---|---|
| Jenkins returns `403` | Subject, scope, role, and permission |
