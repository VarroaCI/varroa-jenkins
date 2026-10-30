# Multi-Tenancy

Use a Kubernetes namespace for each tenant or workload boundary. A cluster-scoped `Tenant` grants Varroa access to its namespaces and can create those namespaces.

## Onboard a tenant

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Tenant
metadata:
  name: payments
spec:
  displayName: Payments
  subjects:
    - kind: Group
      name: payments-tenant
  namespaces:
    - tenant-payments
  roleRef: developer
  provisionNamespaces: true
```

Apply the resource, then inspect its status:

```bash
kubectl apply -f tenant.yaml
kubectl get tenant payments -o yaml
```

`namespaces` must not be empty. `roleRef` defaults to `developer`. `members` names local users; `subjects` names identities from the configured provider.

The names `runs` and `overview` are reserved. The pipelines API uses them as routes.

## Control namespace access

Only configured deployable namespaces accept controller creation. Configure them through the installation values or a `Tenant`; then use [Varroa RBAC](../security/varroa-rbac.md) to grant API access and [Jenkins RBAC](../security/jenkins-rbac.md) to grant Jenkins permissions.

Tenants own their namespace membership and bindings. Do not manually edit generated access objects. If a user gets `403`, verify their identity, Tenant membership, namespace scope, and role binding. If creation is rejected, verify that the namespace is deployable.

## Onboard a pipelines tenant

A `Tenant` can also become a pipelines tenant: it shares a cell controller with other tenants instead
of running its own controller.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Tenant
metadata:
  name: payments
spec:
  namespaces: ["tenant-payments"]
  pipelines:
    profile: jenkins-lts
    agentNamespace: tenant-payments
    composedBundleRef:
      name: payments-pipelines
      namespace: tenant-payments
```

`pipelines.agentNamespace` must be one of `spec.namespaces`. A `pipelines` block requires `profile` and `agentNamespace`. The API refuses a block without either field with `400`, and the error names the field. A placement reconciler binds the
`Tenant` to a cell that matches `pipelines.profile`, and the `Tenant`'s `Placed` condition reports
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
sets the window on the `Tenant`:

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

The tenant home page shows the effective window for the tenant and for each job rule. Only an
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
`Tenant`, under `spec.pipelines.exports`:

```yaml
spec:
  pipelines:
    profile: jenkins-lts
    agentNamespace: tenant-platform
    exports:
      - job: "release/*"
        callers: ["payments", "search"]
      - locks: ["deploy-prod"]
        callers: ["payments"]
```

Each entry sets exactly one of `job` or `locks`, plus `callers`, the list of Tenant names allowed
to use it. An edit to `exports` takes one reconcile tick to reach the record store that enforces
the grant. See [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the caller-side
`varroa` steps and the fleet lock.

## Anonymous build badges

A pipelines tenant can serve a status badge for each of its jobs without a session. The badge is
off by default. Turn it on under `spec.pipelines.badge`:

```yaml
spec:
  pipelines:
    badge:
      enabled: true
```

The badge URL holds the tenant's badge token in its path, so the URL is the credential. A caller
with the URL reads the job name and its last result. The operator mints the token once. To rotate
it, clear the token. The next reconcile tick mints a new one, and the old URLs answer 404. A tenant on a member cluster
takes the new token on the next operator tick after the clear, so its old URLs answer 404 a little
later:

```sh
kubectl patch tenant payments --subresource=status --type=json \
  -p '[{"op":"remove","path":"/status/pipelines/badgeToken"}]'
```

## Start a run from a pull-request comment or a check re-run

A pipelines tenant can start a run from a forge event. A pipelines administrator of the tenant
sets the rules in `triggers`:

```yaml
spec:
  pipelines:
    triggers:
      commentPattern: "^/retest$"
      checksRerun: true
```

- `commentPattern` is an RE2 pattern of at most 256 characters. A new pull-request comment that
  matches it starts a run of the pull-request job. The pattern is matched against the trimmed
  comment body. An empty pattern turns comment triggers off. The default is off.
- `checksRerun` lets a GitHub check re-run request start a run. The default is on.

A run that a comment starts names the commenter as its actor. The run shows the comment as its
cause. Only a writer of the repository starts a run. The
[webhook page](webhooks.md#re-run-from-github) lists the checks that the door applies.
One event starts at most eight jobs.
