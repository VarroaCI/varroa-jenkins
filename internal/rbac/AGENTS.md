# internal/rbac

## Purpose

The single authoritative interpreter of Varroa's role model. Resolves
`VarroaRole`/`VarroaRoleBinding` (control-plane API authz) and
`JenkinsRole`/`JenkinsRoleBinding` (per-controller Jenkins authz) CRDs into
concrete permission grants, and renders the Jenkins-facing grants as
role-strategy-plugin JCasC YAML. Varroa owns the Jenkins authorization
strategy outright — no bundle-supplied `authorizationStrategy` is ever merged
with what this package generates.

## Ownership

- Owns: role/binding resolution (`resolver.go`), role-strategy YAML
  rendering (`generator.go`), cross-cluster role federation
  (`federation.go`), the fixed `system-mite` permission set
  (`mite_permissions.go`).
- Does NOT own: built-in role CR reconciliation (`varroa:admin`,
  `varroa:operator`, `varroa:developer`, `varroa:viewer`,
  `varroa:system-mite` — reconciled by `internal/controller/role_controller.go`),
  injecting the generated YAML into the composed bundle or stripping a
  bundle's own `authorizationStrategy` block (`internal/controller/controller_controller.go`),
  or the `VarroaRole`/`JenkinsRole` CRD schemas themselves (`api/v1alpha1/types.go`).
  This package is a pure resolve/render library consumed by `internal/controller`
  and `internal/api`.

## Local Contracts

- **`Resolver`** (`resolver.go:86`, built via `NewResolver`) — indexes
  `VarroaRoleBinding`/`JenkinsRoleBinding` by subject via `SubjectIndexFunc`
  / `JenkinsSubjectIndexFunc` (`cache.Indexer` index funcs, key format
  `subjectIndexKey(kind, name)`).
  - `EffectiveAPICapabilities` / `EffectiveClusterAPICapabilities` →
    `APICapabilitySet` (`map[string]map[string]bool`, resource→verb) drive
    BFF-side authz checks (`internal/auth`/`internal/api`).
  - `EffectivePermissionScopes` → `PermissionScopes` (`resolver.go:73`) feeds
    `/me/permissions` and namespace-scoped UI gating.
  - `CreateScopeNamespaces` resolves which namespaces a subject may create
    controllers in (`roleGrantsControllersCreate` gate).
  - `JenkinsAssignments` (legacy path, `VarroaRole.JenkinsPermissions` /
    `VarroaRole.JenkinsRoleRef`) and `JenkinsRoleAssignments` (current path,
    `JenkinsRole`+`JenkinsRoleBinding` CRDs with optional
    `JenkinsRoleBindingSpec` scope) both return `[]RoleAssignment`
    (`resolver.go:539`) for a given `*v1alpha1.Controller` — the generator
    input. `collectFromJenkinsRoleBindings`/`collectFromVarroaRoleBindings`
    dedupe/merge into an `accKey`-keyed map via `upsertAssignment`.
  - `TenantGrantSources` / `TenantGrantSourcesTruncated`
    (`tenant_grants.go`) walk the same three tenant paths
    `EffectiveTenantCapabilities` walks, with the subject filter removed:
    they answer **which grants reach a tenant**, so they take no claims and
    decide nothing. The order is `teamMembership`, `tenantScopedBinding`,
    `clusterWideBinding`, then binding name, and the list is capped at
    `MaxTenantGrants` (50). The exported `TenantGrantSources` returns the
    capped list; a caller that must tell a full list from a cut list calls
    the `Truncated` sibling, because a list of exactly 50 is not proof of a
    cut. `TenantItemRoleName` / `TenantItemRolePattern` are the single
    source of the generated per-tenant Jenkins item role name and its folder
    pattern; `JenkinsRoleAssignments` calls them, so the BFF's cell grants
    read never opens a `JenkinsRole` object.
  - Member role: `JenkinsRoleAssignments` also emits, per tenant on a cell,
    a `varroa:tenant-member:<team>` Item role for every grant with pipelines
    `write`. It carries the person checks of the acting-user routes (scan,
    run, replay, restart, input answers). A grant with no `scope.tenants`
    job list gets one role on the whole folder subtree. A tenant-scoped
    grant gets `varroa:tenant-member:<team>:<binding>` with the job globs
    (`TenantMemberNarrowedPattern`). The member walk
    (`allTenantGrantSources`) is uncapped, because a cut list drops a
    Jenkins subject silently. `Resolver.TenantMemberRoles` is the single
    source of these roles. `JenkinsRoleAssignments` and the BFF cell grants
    read both call it. No generated name may equal
    `varroa:tenant:<team>`: the plugin looks that exact name up.
    `User` subjects of the role also resolve through the idp-managed `User`
    objects of the operator namespace (`SetUserDirectory`, `tenant_member_users.go`):
    each person's principal (preferred username, email, subject) joins the grant.
    Alias claims are the configured claim names, including custom claims stored in
    `varroa.dev/oidc-user-claims` at login.
    Only the current claim values count; `varroa.dev/oidc-former-identifiers` is for deprovisioning only.
    The operator builds its resolver with a Team informer and a User informer, so the
    Team-membership path reaches the member role.
    Tenant-member copies (`federatedTenantMemberSources`) reach the member role of their
    tenant. A copy annotated `"*"` (`FederatedAllTenants`) reaches the member role of every
    tenant the cell hosts; it never narrows, and a copy with a jobs annotation fails closed. A copy is a `JenkinsRoleBinding` labelled `varroa.dev/federated-tenant-member`
    and annotated `varroa.dev/federated-tenant`, with an optional `varroa.dev/federated-tenant-jobs`
    JSON list. A copy with an unreadable job list grants nothing. The generic
    `collectFromJenkinsRoleBindings` skips every copy. The narrowed role name uses the copy name.
    `DesiredFederatedCRs` (`federation.go`) writes the copies. Its `expandSubjects` argument
    comes from `TenantMemberSubjectExpander`.
  - Scope matching (`scopeMatches`, `controllerScopeMatches`, `lowerScope`,
    `scopeSuffix`) implements namespace/controller-name/label scoping shared
    by both binding kinds.
- **Permission model** — three buckets per `RoleAssignment`: Global
  (`hudson.model.Hudson.*`), Item (`hudson.model.Item.*`, `hudson.scm.SCM.*`,
  credentials), Agent (`hudson.model.Computer.*`). `generator.go:47`'s
  `permissionGroupTitles` maps Jenkins internal permission-group class names
  to role-strategy-plugin's expected title strings (`"Overall"`, `"Job"`,
  `"Agent"`, `"SCM"`, `"Credentials"`); `toRoleStrategyPermission`
  (`generator.go:61`) converts a dotted permission id (e.g.
  `hudson.model.Item.Read`) to the plugin's `Job/Read` form.
- **`Generator`** (`generator.go:15`, `NewGenerator(resolver)`,
  `.WithBackend(backend)`) — `Generate(controller)` /
  `GenerateWithAdminCheck(controller)` return the
  `authorizationStrategy.roleBased` JCasC YAML block (via
  `RoleStrategyBackend.Render`, `generator.go:86`) plus (in the `WithAdminCheck`
  variant) whether a human admin is present. `HasHumanAdmin`
  (`generator.go:176`) / `containsAdminister` (`generator.go:199`) guard
  against generating a bundle with no human `Administer` grant — callers use
  this to block lockout-risk applies.
- **`system-mite` role** (`mite_permissions.go`) — fixed permission list
  granted to the mite operator-JWT identity: `Hudson.Read`, `Hudson.SystemRead`
  (opt-in via `-Djenkins.security.SystemReadPermission=true`, else inert per
  issue #189, see `internal/controller/clientset_client.go`), `Item.Read`,
  `Item.Discover`, `Item.Configure`, `Item.Create`, `Item.Delete`,
  `View.Read`, `View.Configure`, `View.Create`. Deliberately excludes
  `Administer` — mite never calls admin-only endpoints (see project memory
  "Mite minimal permissions").
- **Federation** (`federation.go`) — `DesiredFederatedCRs` projects every
  core-cluster `VarroaRoleBinding` whose `VarroaRole` has a Jenkins
  projection (a Global `jenkinsRoleRef` or legacy `jenkinsPermissions`) onto
  each non-core (hive) cluster as a generated `JenkinsRole` plus a
  `varroa-fed-<role>-<hash>` `JenkinsRoleBinding` (`fedBindingName`, a
  `sha256` of the scope and subjects). A binding with a `tenants` scope projects
  tenant copies instead (`varroa-fed-tenant-*`). A cluster-wide binding (nil scope)
  whose role grants pipelines `write` also projects one all-tenants copy
  (`varroa-fed-tenant-all-*`, annotation `"*"`, `RoleRef` `varroa:tenant-member:*`),
  even when the role has no Jenkins projection. It skips these bindings: a projection
  onto the reserved `system-mite` or `system-operator` role (with a warning), a
  binding with no Jenkins projection (`NoJenkinsProjection`) unless it is a
  cluster-wide pipelines write, and a
  binding with no subjects (silent, because a Team with no members is a
  normal state). The empty-subjects skip comes first, before every check that
  warns and before the role is recorded, so a role that only empty bindings
  reference is not federated. A
  projected binding always carries a non-nil `subjects` list (`copySubjects`),
  because the CRD rejects a `null` list. Federation silently accepts an
  existing labeled builtin JenkinsRole only when its name and spec match the
  canonical builtin. The fourth return value, `outcomes`, maps each named
  source binding to a `BindingOutcome`: a `Skip*` reason code and message, or
  the desired `JenkinsRole` and `JenkinsRoleBinding` names. Every skip branch
  sets a reason, including the silent ones (`NoSubjects`, `RoleNotFound`,
  `NoJenkinsProjection`). Bindings that hash to one child name share one
  projected child. Each projected `JenkinsRoleBinding` carries the annotation
  `varroa.dev/federated-bindings` (`AnnotationFederatedBindings`): the sorted,
  comma-separated names of its source bindings. It is an annotation because
  one child has several owners and a label value holds 63 characters. The BFF
  reads it to report a failed delete of a stale child. Each projected
  `JenkinsRole` carries the annotation `varroa.dev/federated-roles`
  (`AnnotationFederatedRoles`): the sorted, comma-separated names of every
  `VarroaRole` with at least one projected binding onto it. The label names
  only the first of them. The BFF reads it to report a failed delete of a
  stale role copy on each owner `VarroaRole`. The BFF status write reads these outcomes, so a new skip
  branch needs a new reason code.
- **`Backend` interface** (`resolver.go:79`) — abstraction over YAML
  rendering; `RoleStrategyBackend` is the only implementation today. Swap
  points here if a second Jenkins authorization plugin is ever supported.

## Work Guidance

- Any new permission surfaced to `JenkinsRole` must be added to both
  `permissionGroupTitles` (if it's a new group) and validated against
  role-strategy-plugin's actual permission ID strings — a typo renders inert
  YAML with no apply-time error.
- Changes to `RoleAssignment` scope-matching semantics affect both the
  legacy (`VarroaRole.JenkinsPermissions`/`JenkinsRoleRef`) and current
  (`JenkinsRole`/`JenkinsRoleBinding`) paths — update both collector
  functions in lockstep, and check `internal/controller/role_controller.go`
  / `rbacspec.go` for built-in-role assumptions before changing shapes.

## Verification

```bash
go test -race -count=1 ./internal/rbac/...
make lint
```
