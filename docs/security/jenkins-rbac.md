# Jenkins RBAC (data-plane roles)

<!-- sources: api/v1alpha1/types.go (JenkinsRole, JenkinsRoleBinding, JenkinsScope), internal/rbac/, internal/controller/role_controller.go (builtinJenkinsRoles) -->

Who may do what **inside each Jenkins** — build, configure jobs, administer — declared as cluster-scoped CRDs and generated into every controller's role-strategy configuration. You never configure Jenkins authorization by hand; Varroa owns it.

## Concepts

- **`JenkinsRole`** (cluster-scoped) — a pure Jenkins permission set: `roleType` `Global` | `Item` | `Agent` (default `Global`) plus a list of Jenkins permission IDs (`hudson.model.Item.Build`, `hudson.model.Item.Configure`, …).
- **`JenkinsRoleBinding`** (cluster-scoped) — grants a role to subjects (`kind: User`/`Group`, same claim values as [Varroa RBAC](varroa-rbac.md)), restricted two ways:
  - `controllerScope` — **which controllers** (namespaces allow-list and/or controller label selector);
  - `jenkinsScope` — **where inside Jenkins**: `Global`, `Folder` (a path plus `propagate: None|Children|Subtree`), or `Pattern` (raw regex over item names).
- **This page owns the RBAC-generation flow**: `JenkinsRole` + `JenkinsRoleBinding` objects — together with `VarroaRole.jenkinsRoleRef` links and legacy `VarroaRole.jenkinsPermissions` — are compiled per controller into role-strategy YAML. **Varroa owns the authorization strategy**: any `authorizationStrategy` keys in your [bundle](../config/composed-bundles.md) JCasC are stripped, and the built-in roles (`varroa-admin`, `varroa-operator`, `varroa-developer`, `varroa-viewer`, `varroa-system-mite`, `varroa-system-operator`) are reconciled continuously — hand-edits in the Jenkins UI don't survive.

In the dashboard, Jenkins Roles are shown as a responsive policy table with separate Global, Item, and Agent permission columns. A role's permissions appear only in its `roleType` column. Jenkins Role Bindings separately identify role, typed subjects, and scope. Both lists retain the selected cluster in filters and create/edit links, and actions remain hidden when the user lacks the corresponding permission.
- Bundles can also carry `rbac.yaml` role definitions ([bundle sources](../config/bundle-sources.md)), and items can attach `groups`/`filteredRoles` per item ([items](../config/items.md)); all of it feeds the same generator.

## How to create a role and bind it

A read-plus-build role, granted to a team on their folder only, across their controllers:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRole
metadata:
  name: payments-builder
spec:
  roleType: Item
  description: Build and read payments jobs
  permissions:
    - hudson.model.Item.Read
    - hudson.model.Item.Build
    - hudson.model.Item.Cancel
    - hudson.model.Item.Workspace
---
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRoleBinding
metadata:
  name: payments-team-builders
spec:
  roleRef: payments-builder
  subjects:
    - { kind: Group, name: payments-team }
  controllerScope:
    namespaces: [teams-payments]
  jenkinsScope:
    type: Folder
    folder: payments
    propagate: Subtree
```

```bash
kubectl apply -f payments-rbac.yaml
```

**Verify** in a live Jenkins on one of the scoped controllers:

1. *Manage Jenkins → Manage and Assign Roles* shows an item role matching `payments-builder` with the folder pattern, and `payments-team` assigned.
2. A member can trigger a build under `payments/` and gets 403 configuring a job outside it:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $USER_TOKEN" \
  https://<controller-host>/job/payments/job/deploy/build      # 201
```

> [!NOTE]
> **Known limitation ([#218](https://github.com/varroaci/varroa-jenkins/issues/218))**: role/binding changes regenerate each controller's RBAC config, but a controller that is already `Connected` does not currently apply the regenerated strategy in place — it lands on the next provisioning cycle. Until that issue closes, follow an RBAC change that must take effect immediately with a re-provision ([Lifecycle](../operations/lifecycle.md)).

## How to grant via a pattern scope

```yaml
  jenkinsScope:
    type: Pattern
    pattern: "payments/.*-prod"
```

Pattern scopes are raw regexes over full item names — quote carefully and prefer `Folder` scopes when a folder boundary exists.

## Concepts: the bridge from Varroa roles

A [`VarroaRole`](varroa-rbac.md) may set `jenkinsRoleRef` to a **Global** `JenkinsRole` — everyone bound to the Varroa role also gets that Jenkins role on controllers in the binding's scope. The built-ins are pre-linked (`admin`→`varroa-admin`, etc.), which is why a Varroa `admin` can administer every Jenkins without extra bindings. Keep custom Item/Agent-scoped grants in `JenkinsRoleBinding`s; the ref bridge is Global-only by validation.

In multi-cluster installs, `VarroaRole` and `VarroaRoleBinding` objects stay on the core. The core BFF federates only their Jenkins projection to hives over the existing RBAC authoring path:

- For `jenkinsRoleRef`, the hive receives a copied Global `JenkinsRole` with the same unprefixed name as the core role, plus a `JenkinsRoleBinding` whose `roleRef` is that same name and whose `controllerScope` is copied from the `VarroaRoleBinding` scope.
- For legacy `jenkinsPermissions`, the hive receives a synthesized Global `JenkinsRole` named after the `VarroaRole`, plus the matching binding.
- Federated CRs carry `varroa.dev/federated-from`; only the binding object name uses the reserved `varroa-fed-` prefix. Role names and `roleRef` values are never prefixed, so the generated Jenkins role remains `varroa:<name>` on core and hives.

If a hive already has a same-named `JenkinsRole` or same object-name `JenkinsRoleBinding` without the federation label, the core skips that object and warns rather than overwriting hand-authored RBAC.

## Troubleshooting

- 403 in Jenkins after adding a binding → the controller is `Connected` and hasn't re-provisioned (limitation #218 above); or the subject claim doesn't match exactly.
- Your JCasC `authorizationStrategy` "disappears" → stripped by design; declare roles here instead.
- A role you edited in the Jenkins UI reverted → built-ins and generated strategy are reconciled; make the change in CRDs.
- Which permission ID do I need? → the role-strategy section of *Manage Roles* lists them; common ones: `hudson.model.Item.{Read,Build,Configure,Create,Delete,Cancel,Workspace}`, `hudson.model.Run.{Replay,Update}`, `hudson.model.View.{Read,Configure}`.

## `varroa-system-operator` role details

The operator's own direct-to-Jenkins identity for `executeGroovy` dispatch. This role carries **`hudson.model.Hudson.Administer`** — the only permission Jenkins accepts for `/scriptText` (the script-console endpoint). There is no narrower permission that authorizes it; the risk reduction comes from **credential lifecycle**, not permission size.

- **How it is granted**: the operator synthesizes a `varroa:system-operator` role-strategy assignment targeting the GROUP authority `ROLE:varroa:system-operator` — exactly like the mite's `ROLE:varroa:system-mite` mechanism, but without a lockout-guard minimal fallback (operator access is not recovery-critical).
- **Credential lifecycle**: a fresh `system:varroa-operator` JWT is minted per dispatch with a ~2-minute TTL, RS256-**signed with the operator's private key** (the same keypair the mite tokens use). The token is held only in operator process memory, never written to a Secret, never sent to a mite or Jenkins pod — the target Jenkins **verifies** it offline with the corresponding public key (`VARROA_MITE_PUBKEY_PEM`). If the operator process or its private signing key is compromised, the attacker can mint unlimited `system:varroa-operator` tokens with **fleet-wide Administer** (every Jenkins that trusts that public key). Protect the private signing key as the root credential it is.
- **Distinction from the mite**: the `varroa-system-mite` role intentionally lacks `Hudson.Administer` — the mite must never hold `/scriptText` access. The operator's separate `system:varroa-operator` identity is the only path that can run arbitrary Groovy.

## Related pages

- [Varroa RBAC](varroa-rbac.md) — control-plane roles and the ref bridge
- [Items](../config/items.md) — per-item `groups`/`filteredRoles`
- [Lifecycle](../operations/lifecycle.md) — re-provisioning to force RBAC application
- [The mite](../architecture/mite.md) — why `varroa-system-mite` exists
- [Brood Operations `executeGroovy`](../operations/brood-operations.md#executegroovy-semantics) — the `varroa-system-operator` role in action
- [Network Policies](../install/network-policies.md) — egress TCP/8080 for operator→Jenkins dispatch
- [CasC Catalog — `groovy` type](../config/casc-catalog.md) — execution-only catalog items
