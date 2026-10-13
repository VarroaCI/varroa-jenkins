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

Subject names match authenticated claims exactly and case-sensitively. A local account beside OIDC is
`local:<name>`. A group binding also matches the Varroa `Group` of that name.
That is the break-glass path.

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

## Read catalog and bundle objects

A caller reads a `CatalogItem`, `CatalogSource`, or `ComposedBundle` in a
namespace where its binding grants `read`. A caller with `read` in any scope
also reads the operator namespace, because an unqualified `itemRef` resolves
there. A cluster-wide binding reads every namespace. A list leaves out the
objects that the caller cannot read. A single read outside that set answers
`403`. MCP and search apply the same rule. A binding scoped only by
`controllerSelector` reads the operator namespace only.

Each cluster uses its own operator namespace. A member cluster reports that
namespace in its heartbeat. The BFF applies the value of the target cluster.
A member that reports no namespace grants no operator-namespace read.

## Grant pipelines access

The `pipelines` resource controls Jenkins job and run access through Varroa.
It has three verbs.

| Verb | Grants |
|---|---|
| `read` | Read a tenant's jobs, runs, stages, logs, artifacts, tests, and changes |
| `write` | Start, cancel, and rerun a tenant's builds |
| `admin` | Write a tenant's `Team.spec.pipelines.exports` |

A verb does not imply another verb. `write` does not grant `read`. List
every verb a role needs.

Built-in roles carry this `pipelines` matrix:

| Role | `pipelines` verbs |
|---|---|
| `admin` | All verbs, through the wildcard rule |
| `operator` | `read`, `write`, `admin` |
| `developer` | `read`, `write` |
| `viewer` | `read` |

## Read and scale a cell pool

`cellpools` carries two verbs. `read` lists and gets a [cell pool](../operations/cells.md);
`update` scales one. There is no create verb and no delete verb: an operator creates and deletes a
pool with `kubectl` or through GitOps.

| Role | `cellpools` verbs |
|---|---|
| `admin` | Both verbs, through the wildcard rule |
| `operator` | `read`, `update` |
| `developer` | None |
| `viewer` | None |

A pool's `status.tenants` lists every tenant on the pool, and the Teams API is admin-only. A read
verb for a developer or a viewer would therefore disclose the tenant list to a caller who may not
list Teams. `CellPool` is cluster-scoped, so a namespace-scoped binding satisfies neither verb.

## Read the fleet locks

`fleetlocks` carries two verbs. `read` lists every fleet lock on the `/locks` page and through
`GET /locks`. `release` force-releases a `releasing` lock through `POST /locks/release`. See
[Fleet locks](../tutorials/run-operations.md#fleet-locks).

| Role | `fleetlocks` verbs |
|---|---|
| `admin` | Both, through the wildcard rule |
| `operator` | `read`, `release` |
| `developer` | None |
| `viewer` | None |

A fleet-wide lock list names every tenant's lock names and job paths at once. Both verbs
therefore stay with the operator role, as `cellpools` does. A tenant sees its own locks on the
team home with no verb.

## Grant break-glass access

`break-glass` is a `controllers` verb. It lets a caller request a temporary admin session on a
[headless controller](headless-controllers.md).

Only the built-in `admin` role holds `break-glass`, through its wildcard rule. No other
built-in role holds it. Grant it to a custom role the same way as any other verb:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRole
metadata:
  name: incident-responder
spec:
  apiRules:
    - resources: [controllers]
      verbs: [break-glass]
```

Holding `break-glass` grants a token. It does not grant Jenkins access on its own. The caller
also needs the Jenkins permission `hudson.model.Hudson.Administer` on the target controller.
Otherwise Jenkins denies the session. See [Headless controllers](headless-controllers.md) for
the full runbook.

## Scope a binding to a pipelines tenant

A pipelines tenant is a `Team`. Scope a `VarroaRoleBinding` to one or more
tenants:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: checkout-developers
spec:
  roleRef: developer
  subjects:
    - kind: Group
      name: checkout-team
  scope:
    tenants:
      - name: checkout-team
        jobs:
          - "release/*"
```

`tenants[].name` names a `Team`. `tenants[].jobs` is an optional list of
folder-relative glob patterns. An absent `jobs` list covers every job of the
tenant. A pattern uses `*` for one path segment, `**` to cross a `/`, and
`?` for one character.

A scope names one dimension. The API server rejects a scope that sets
`tenants` together with `namespaces` or `controllerSelector`. A
`controllerSelector`-scoped binding grants no `pipelines` access. Scope the
binding to `tenants` to grant `pipelines` access.

### Author the same scope in the dashboard

Open **Access**, then **Role Bindings**, then the binding. Expand **Scope**
and use the **Pipelines Tenants** editor.

1. Select **+ Add tenant** and name the `Team`. The list offers the teams you
   can read. A team that does not exist yet is accepted.
2. Select **+ Narrow to job patterns** to add a `jobs` list. Leave the list
   off to cover every job of the tenant.
3. Select **+ Add pattern** and enter a folder-relative pattern. The editor
   shows the grammar beside the list, and refuses an empty pattern, a
   pattern starting with `/`, and an empty list.

The editor shows a live count in the form `matches 3 of 10 jobs you can see`.
The count reads the team's job list with **your** grants, not the subject's,
so a narrower view of your own produces a smaller count. When the job list
cannot be read the editor states so, and still accepts every pattern.

The editor disables the **Namespaces** and **Controller Selector Labels**
controls while a tenant entry exists, and disables the tenant controls while
either of those holds a value. Each disabled control states the reason.

### A jobs glob is a Varroa rule

A `jobs` glob is a Varroa rule. It narrows what Varroa shows and what Varroa
starts. It changes nothing inside Jenkins. Jenkins keeps one item role per
tenant, which covers the tenant's whole folder. Varroa defines no
folder-level Jenkins role.

## Reach a tenant through Team membership

A `Team` member reaches the team's pipelines tenant through the `Team`. The
member needs no separate `VarroaRoleBinding` for that tenant.

## Jenkins role bindings carry no tenant scope

A `JenkinsRoleBinding` accepts no tenant scope. Its `controllerScope` field
rejects a `tenants` list. A `pipelines` verb produces no Jenkins
role-strategy entry.

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
