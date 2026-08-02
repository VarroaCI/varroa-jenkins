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
- **Federation** (`federation.go`) — projects a core-cluster
  `VarroaRoleBinding` for the `system-mite` `VarroaRole` onto a non-core
  (hive) cluster as a generated `JenkinsRole`/`JenkinsRoleBinding` pair
  (`fedBindingName`, content hash via `sha256`/hex for change detection) so
  multi-cluster mites get consistent Jenkins RBAC without hand-authored
  per-cluster CRs. Federation silently accepts an existing labeled builtin
  JenkinsRole only when its name and spec match the canonical builtin.
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
