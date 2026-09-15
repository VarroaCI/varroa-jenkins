# Cell Pools

A `CellPool` is a cluster-scoped custom resource. It holds the size, the capacity, and the
template of one pool of cells that serve the pipelines tenants of a single version profile. The
operator sizes each pool from its own spec on a 60-second ticker, and reports the result in
`status`.

Read [Cells and pipelines tenants](cells.md) first. That page covers what a cell is and how a
`Team` becomes a tenant.

## Spec fields

| Field | Type | Meaning |
|---|---|---|
| `profileRef` | string, required, immutable | the `JenkinsVersionProfile` the pool's cells run |
| `warm` | int32, default 0 | target count of free cells the pool keeps ready |
| `maxCells` | int32, default 0 | cap on the pool's cells, placed cells included. 0 means unlimited |
| `maxTenantsPerCell` | int32, minimum 1, default 10 | capacity a pool cell uses when its own `spec.cell.maxTenants` is zero |
| `unreadyGracePeriod` | duration, optional | how long a pool cell may stay unready before the operator rolls its pod |
| `cellTemplate` | object, optional | `resources` and `podOverrides` for the cells the pool creates |

`profileRef` is immutable. A pool's cells carry the profile label they were created with, and the
operator never restamps that label. A changed `profileRef` would orphan every cell of the pool. To
serve another profile, create another pool.

## Status fields

| Field | Meaning |
|---|---|
| `warmCells` | owned cells that hold no tenant |
| `placedCells` | owned cells that hold at least one tenant |
| `unreadyCells` | owned cells whose `CellReady` condition is not `True` |
| `freeTenantSlots` | spare capacity of the placed cells only |
| `tenants` | every tenant on the pool, sorted |

`freeTenantSlots` counts placed cells alone. `warmCells` already reports the empty ones.

The `PoolReady` condition carries seven reasons.

| Reason | Status | Holds when | Action |
|---|---|---|---|
| `Ready` | `True` | the profile exists, the pool is below its cap, and no fill precondition refused a fill | None. |
| `CellCapReached` | `False` | `maxCells` is above zero and the pool owns that many cells | Raise `maxCells`, or leave it: the pool is working as configured. |
| `ProfileMissing` | `False` | `profileRef` names no `JenkinsVersionProfile` | Create the `JenkinsVersionProfile`, or fix `profileRef`. |
| `RecordStoreUnsupported` | `False` | `recordStore.enabled` is false | Enable the record store. |
| `BlobTierUnsupported` | `False` | `recordStore.blobs.mode` is not `s3` | Set the blob tier to `s3`. |
| `ArtifactSecretMissing` | `False` | `recordStore.blobs.s3.artifactSecret` is unset, or `<artifactSecret>-<pool>` does not exist with non-empty data | Set `recordStore.blobs.s3.artifactSecret`, and create `<artifactSecret>-<pool>`. |
| `ArtifactPluginMissing` | `False` | the plugin set a cell of the pool's version resolves does not pin `artifact-manager-s3` | Pin `artifact-manager-s3` in the version profile. |

The four precondition reasons report only when the pass wanted to create at least one cell. A
pool whose free count already meets its target reports `CellCapReached` or `Ready`, even with a
missing artifact Secret: the pass never tests a precondition it does not need. When more than one
precondition would refuse, the reasons rank `ProfileMissing`, then `RecordStoreUnsupported`, then
`BlobTierUnsupported`, then `ArtifactSecretMissing`, then `ArtifactPluginMissing`, then
`CellCapReached`, then `Ready`; the status reports the highest that applies.

## Create a pool

The operator creates `CellPool/<profile>` for each profile a pipelines `Team` requests, when
`cells.warmPool.enabled` is true. The chart values seed the new pool:

```yaml
cells:
  warmPool:
    enabled: true
    perProfile: 1
  maxTenantsPerCell: 10
  unreadyGracePeriod: 5m
```

Each value is the default of a pool the operator creates. It configures no live pool, and a Helm
upgrade never rewrites the spec of a pool that already exists.

The chart refuses a `cells.maxTenantsPerCell` below 1 and a `cells.warmPool.perProfile` below 1 at
render time. The first floor matches the CellPool CRD. The second matches the operator, which seeds
at least one warm cell so placement keeps the last free cell. Each of the two must be a whole
number that fits a 32-bit integer.

To create a second pool, apply one:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CellPool
metadata:
  name: solo
spec:
  profileRef: jenkins-lts
  warm: 1
  maxTenantsPerCell: 1
```

Creation and deletion are `kubectl` or GitOps operations. The API and the CLI read and scale a
pool. Neither creates nor deletes one.

## Scale a pool

```bash
varroactl get cellpools
varroactl describe cellpool jenkins-lts
varroactl scale cellpool jenkins-lts --warm 3 --max-cells 8
```

The same edit through `kubectl`:

```bash
kubectl patch cellpool jenkins-lts --type=merge -p '{"spec":{"warm":3}}'
```

A watch wakes the operator, so a scale edit takes effect within seconds rather than after the
60-second backstop.

Lowering `warm` drains free cells only. The pool never deletes a cell that holds a tenant. It also
keeps a cell that a tenant is being placed on, although that cell holds no tenant yet. Such a cell
leaves the pool one cell above its target until the next pass, which is expected.
Deleting a pool drains its free cells. A placed cell keeps every label and lives on as an ordinary
cell, resolving its capacity from its own `spec.cell.maxTenants` or from `cells.maxTenantsPerCell`.

### Scale up a pool

A pool fill needs four install-wide gates to pass: the record store enabled, the `s3` blob tier,
`recordStore.blobs.s3.artifactSecret` set to a base name, and the resolved plugin set pinning
`artifact-manager-s3`. Once those hold fleet-wide, one Secret per pool,
`<artifactSecret>-<pool name>` in the release namespace, is the only remaining credential
prerequisite for a scale-up. See
[The artifact Secret](build-logs-and-artifacts.md#the-artifact-secret) for its keys and the
candidate lookup order.

An install that predates pool-scoped credentials, and holds only per-cell Secrets, needs the pool
Secret created once per pool before a scale-up mints new cells. Existing cells keep running on
their per-cell Secret; only a newly minted cell needs the pool one.

## Cold start

A pool with `warm: 0` holds no cell until a tenant of that pool waits for placement. The pass then
creates one cell. The tenant reports `Placed=False` with reason `AwaitingCell` while that cell
boots. A cell boots in the time a Jenkins `StatefulSet` needs to reach `Connected`, usually one to
three minutes. Set `warm` to 1 or more to remove that wait.

## Template edits

The pool re-stamps `spec.version`, `spec.resources`, and `spec.podOverrides` on each free cell
every tick. It never edits a cell that holds a tenant. A `cellTemplate` edit therefore rolls the
free cells at once, and reaches a placed cell only after that cell drains. To grow a placed
tenant, move it to a pool whose template is larger.

## Cell labels

A cell carries three labels. `varroa.dev/cell-pool` names its pool, `varroa.dev/cell-profile`
names its profile, and `varroa.dev/warm` records that a pool created it. A pool owns a cell only
while the pool its label names still exists.

A pool whose name equals its own `profileRef` is that profile's default pool. It adopts every cell
of its profile that carries no pool label. A pool with any other name adopts nothing.

## Permissions

The `cellpools` resource carries two verbs. `read` lists and gets a pool; `update` scales one. The
built-in `varroa:operator` role holds both, and `varroa:admin` holds both through its wildcard.
`varroa:developer` and `varroa:viewer` hold neither. See [Varroa RBAC](../security/varroa-rbac.md).

**Verify:**

```bash
kubectl get cellpool
kubectl get controller -n varroa-system -l varroa.dev/warm=true
```

Expect one `Controller` per `warm` count for each pool, each reporting `WarmPool=True`.
