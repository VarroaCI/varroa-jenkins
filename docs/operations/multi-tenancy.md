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

## Onboard a pipelines tenant

A `Team` can also become a pipelines tenant: it shares a cell controller with other teams instead
of running its own controller.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  namespaces: ["team-payments"]
  pipelines:
    profile: jenkins-lts
    agentNamespace: team-payments
    composedBundleRef:
      name: payments-pipelines
      namespace: team-payments
```

`pipelines.agentNamespace` must be one of `spec.namespaces`. A `pipelines` block requires `profile` and `agentNamespace`. The API refuses a block without either field with `400`, and the error names the field. A placement reconciler binds the
`Team` to a cell that matches `pipelines.profile`, and the `Team`'s `Placed` condition reports
the result. See [Cells and pipelines tenants](cells.md) for the cell prerequisites, what a tenant
gets, and how to troubleshoot a placement or a cell that will not accept it.

A pipelines tenant can also register machines it owns as [external runners](runners.md), agents
that connect over SSH or WebSocket instead of running as Kubernetes pods.

`spec.pipelines` also carries seven settings for build-history rehydration, interrupted runs,
job concurrency, and the backstop scheduler:

| Setting | Default | Range |
|---|---|---|
| `rehydrateBuilds` | `10` | `0` to `100` |
| `rehydrateDeadline` | `30m` | `1m` to `24h` |
| `interruptedRetry` | `manual` | `manual` or `automatic` |
| `concurrency` | `allow` | `allow` or `cancelInProgress` |
| `cronBackstop` | `true` | `true` or `false` |
| `cronCatchUp` | `one` | `one` or `none` |
| `cronGrace` | `5m` | `1m` to `60m` |

See [Cells and pipelines tenants](cells.md#rehydration) for what each setting controls.

A tenant can also keep its runs for a shorter time than the store. The section below describes the rules.

## Tenant and job retention windows

A pipelines tenant can keep its runs for less time than the store-wide `runs` window. An operator
sets the window on the `Team`:

```yaml
spec:
  pipelines:
    retention:
      runs: 30d
      jobs:
        - job: "pr-*"
          runs: 7d
        - job: "release/**"
          runs: 365d
```

The effective window of one run follows these rules:

- A job rule whose glob matches the job path sets the window. The first matching rule wins.
- Without a matching rule, the tenant `runs` window applies.
- Without a tenant window, the store-wide `runs` window applies.
- The store-wide `runs` window caps every result. A longer window never keeps a run past the store
  limit.

Job globs use the export grammar. `*` matches within one folder level, `**` crosses folders, and
`?` matches one character. The glob names the job without its tenant folder.

The tenant and job windows delete single runs. A deleted run takes its stages, tests, test cases,
artifacts, log, and run state with it. Logs and artifacts never outlive their run. The hourly
retention round deletes at most 1000 runs for each job and 10000 runs in total. A large backlog
drains over several rounds. The build-number watermark and the claim ledger stay, so no build
number is reused.

The team home page shows the effective window for the team and for each job rule. Only an
operator can change `retention`. A pipelines administrator of the tenant cannot.

On a cell, `buildDiscarder` on a job does not change the record store. Use `retention` instead.


## Cap concurrent builds across jobs

A pipelines tenant can limit how many builds of a set of jobs run at one time. A pipelines
administrator of the tenant sets the limits in `concurrencyLimits`:

```yaml
spec:
  pipelines:
    concurrencyLimits:
      - name: deploys
        jobs: ["deploy/*", "release/**"]
        max: 1
      - name: tests
        jobs: ["test/*"]
        max: 4
```

Each limit names a set of jobs and a maximum. A build over the maximum waits in the queue. The
job globs use the same grammar as the retention rules. The name must be a DNS label, unique in
the list.

A build that waits holds no build number. When a running build ends, the oldest waiting build
of that limit starts first. A build from source control or from a cron schedule waits in the
queue with no run record. It shows only in the **Waiting** list of its job page. The **Waiting** list updates live.

The cell pool can cap the builds of one tenant on one cell. See [Cell pools](cell-pools.md).

## Narrow a tenant grant to some of its jobs

A `VarroaRoleBinding` scoped to a tenant accepts an optional `jobs` list of
folder-relative globs. See
[Varroa RBAC](../security/varroa-rbac.md#scope-a-binding-to-a-pipelines-tenant) for the grammar
and the dashboard editor.

A `jobs` glob is a Varroa rule. It narrows what Varroa shows and what Varroa starts. It changes
nothing inside Jenkins. Jenkins keeps one item role per tenant, which covers the tenant's whole
folder. Varroa defines no folder-level Jenkins role.

## Pipeline templates on a shared cell

A cell delivers each placed tenant's [pipeline templates](../config/pipeline-templates.md) with
that tenant's folder name attached. A job in one tenant's folder resolves only that tenant's
templates and the operator's own fleet-wide templates, whose folder is empty. A job cannot resolve
another tenant's template.

A tenant's template naming a `spec.scmDefinition.credentialsId` resolves that credential in the
tenant's own folder credential store only. A fleet-wide template, whose folder is empty, may
resolve a system-store credential. This keeps a shared cell's per-tenant isolation: no tenant's
template can reach another tenant's build, and no tenant's template can reach a credential the
tenant does not own.

## Export a job or a lock to another tenant

A pipelines tenant grants another tenant access to one of its jobs or its fleet locks on its own
`Team`, under `spec.pipelines.exports`:

```yaml
spec:
  pipelines:
    profile: jenkins-lts
    agentNamespace: team-platform
    exports:
      - job: "release/*"
        callers: ["payments", "search"]
      - locks: ["deploy-prod"]
        callers: ["payments"]
```

Each entry sets exactly one of `job` or `locks`, plus `callers`, the list of Team names allowed
to use it. An edit to `exports` takes one reconcile tick to reach the record store that enforces
the grant. See [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the caller-side
`varroa` steps and the fleet lock.
