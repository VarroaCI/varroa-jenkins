# Varroa RBAC (control-plane roles)

<!-- sources: api/v1alpha1/types.go (VarroaRole, VarroaRoleBinding, APIRule, SubjectRef, RBACSpec), internal/controller/role_controller.go (builtinRoles), internal/api/authorizer.go -->

Who may do what in **Varroa itself** — the dashboard and REST API. This is the control-plane half of authorization; what users can do *inside* a Jenkins is [Jenkins RBAC](jenkins-rbac.md). The two are linked: a `VarroaRole` can reference a `JenkinsRole` so one binding grants both.

## Dashboard navigation

Authorization-aware pages are grouped under **Access**: Users, Groups, Built-in Roles,
Varroa Roles and Bindings, Jenkins Roles and Bindings, and Teams. Provisioning,
Versions, and Identity are under **Administration**. Entries are hidden when their
permission check fails; direct access renders an in-shell access-denied page without
disclosing resource details.

Provisioning and Versions require global `provisioningdefaults:update`. Users,
Groups, Built-in Roles, and Identity require the `varroa:admin` wildcard grant.
Other Access pages retain their corresponding global resource `read` gate; create and
edit routes require `create` or `update`. Jenkins pages retain their cluster selector
and query-backed cluster context.

## Concepts

- **`VarroaRole`** (cluster-scoped) — a named set of `apiRules`: `resources` × `verbs` over the API's resource types. `spec.jenkinsRoleRef` optionally links a Global [`JenkinsRole`](jenkins-rbac.md) so holders also get in-Jenkins permissions. (`spec.jenkinsPermissions` is the deprecated inline form — prefer the ref.)
- **`VarroaRoleBinding`** (cluster-scoped) — grants a role to `subjects` (`kind: User` or `kind: Group`, matched against the login claims from [Authentication](authentication.md)), optionally **scoped** to namespaces and/or a controller label selector.
- **Built-in roles** are reconciled continuously by the operator (label `varroa.dev/builtin: "true"`) — edits to them are reverted; create your own role instead.

### Built-in roles

| Role | API capabilities | Linked JenkinsRole |
|---|---|---|
| `admin` | everything (`*` / `*`) | `varroa-admin` |
| `operator` | full controller lifecycle incl. `approve-restart`, `approve-deletion`, `manage`; manage catalog sources and composed bundles; read roles/templates/defaults | `varroa-operator` |
| `developer` | read + `approve-restart` on controllers; read catalog items; read/create/update/delete catalog sources; read/create/update composed bundles | `varroa-developer` |
| `viewer` | read everything | `varroa-viewer` |

(A fifth JenkinsRole, `varroa-system-mite`, exists for the [mite](../architecture/mite.md)'s minimal data-plane permissions — it has no Varroa API role.)

### Publishing catalog content from your own namespace

A `varroa:developer` binding scoped to namespace(s) N can create, update, delete, and sync
`CatalogSource`s in N — including `CatalogSource`s composed automatically for a
[`Team`](../operations/multi-tenancy.md)'s `spec.namespaces` — but cannot act on a `CatalogSource`
outside N. Reading catalog sources and catalog items stays global regardless of scope: any caller
with a `catalogsources:read`/`catalogitems:read` grant can browse content published by any team,
so catalog discovery works across namespace boundaries even though publishing does not.

Promoting a `CatalogSource` to the shared/global tier (the operator namespace, `varroa-system` by
default) is a manual step: an operator with cluster-wide `catalogsources` capability creates or
copies it there directly. There is no automated promotion API.

## How to bind a group to a role

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: platform-operators
spec:
  roleRef: operator
  subjects:
    - kind: Group
      name: "acme:platform-team"   # the group claim value from your IdP
    - kind: User
      name: jdoe                   # a specific user subject
```

```bash
kubectl apply -f binding.yaml
```

**Verify:** log in as a member and confirm effective access:

```bash
curl -sf https://app.example.com/api/v1/me -H "Cookie: varroa_token=<value>" | jq .capabilities
```

## How to scope a binding

Limit a grant to certain namespaces or controllers:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: payments-devs
spec:
  roleRef: developer
  subjects:
    - { kind: Group, name: payments-team }
  scope:
    namespaces: [teams-payments]             # only controllers in this namespace
    controllerSelector:                      # AND: only controllers with this label
      matchLabels:
        env: nonprod
```

**Verify:** a member sees and can act on matching controllers only; others are absent from their dashboard and return 403 via the API.

## How to create a custom role

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
  jenkinsRoleRef: varroa-operator     # optional: in-Jenkins permissions too (must be a Global JenkinsRole)
```

Then bind it as above. Verbs you'll use: `read`, `create`, `update`, `delete`, plus controller actions `approve-restart`, `approve-deletion`, `manage`, and the update-center action `upload`. Resources match the API surface: `controllers`, `composedbundles`, `catalogsources`, `catalogitems`, `templates`, `provisioningdefaults`, `version-profiles`, `roles`, `rolebindings`, `jenkinsroles`, `jenkinsrolebindings`, `updatecenter`.

`updatecenter` is cluster-scoped (the `UpdateCenter` CR is a cluster singleton), so a binding's namespace scope does not narrow it. Its only verb is `upload`, which gates pushing a plugin artifact into the update-center store — see [Uploading a plugin](../operations/update-center.md#uploading-a-plugin). `varroa:admin` holds it through `*`/`*` and `varroa:operator` is granted it explicitly; `varroa:developer` and `varroa:viewer` are not.

**Verify:** `kubectl get varroarole release-manager` exists; a bound user's `/me` capabilities reflect exactly these rules.

## Multi-cluster Jenkins permissions

`VarroaRole` and `VarroaRoleBinding` remain core-local because they authorize the core BFF. When a bound role also grants Jenkins permissions through `jenkinsRoleRef` or legacy `jenkinsPermissions`, the core BFF projects that Jenkins-facing part to every hive as ordinary `JenkinsRole` and `JenkinsRoleBinding` CRs labeled `varroa.dev/federated-from`. Hive controllers then consume those CRs through the normal Jenkins RBAC generator, so the generated Jenkins role name stays identical to the core (`varroa:<roleRef>`).

Federation never overwrites hand-authored RBAC on a hive. If a same-named `JenkinsRole` or federated binding object already exists without the federation label, Varroa skips it and logs a warning.

## Concepts: the per-controller shorthand

For the common "this OIDC group runs this controller" case, skip separate bindings and use the controller's own `rbacSpec`:

```yaml
spec:
  rbacSpec:
    groups:
      - { name: payments-team, role: developer }   # admin | operator | developer | viewer
      - { name: payments-leads, role: operator }
```

This maps groups to **built-in** roles for that controller only. Use full `VarroaRoleBinding`s when you need custom roles or multi-controller scopes.

## Troubleshooting

- 403 on an API call you expect to work → check `/me` capabilities; bindings match claim values **exactly** (case-sensitive).
- Edits to a built-in role vanish → by design (continuous reconciliation); clone into a custom role.
- Group binding has no effect → the IdP isn't emitting that group in the claim; see [Authentication](authentication.md) troubleshooting.

## Related pages

- [Jenkins RBAC](jenkins-rbac.md) — the data-plane half
- [Authentication](authentication.md) — where subjects' claims come from
- [API keys](api-keys.md) — keys inherit the owner's roles
