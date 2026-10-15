# Cell views

The dashboard has three operator surfaces for cells: a kind column on the controllers table, a Cell tab on a cell's controller page, and a fleet Cells page. Each renders reported values, and none computes a capacity number. Read [Cells](cells.md), [Cell pools](cell-pools.md), and [Cell usage](cell-usage.md) first.

## The kind column

The controllers table has a Kind column between Phase and Version. Each row reads `Cell` or `Stateful`. A cell row also names its pool and states its fill as `tenants / cap`, and a cell with no tenant carries a **warm** badge.

A kind filter sits below the phase chips, with a chip and a count for each kind. Two chips off lists every kind. The column renders in the brood picker too.

## The Cell tab

A cell's controller page shows a **Cell** tab. No other controller does.

The active tab lives in the `tab` query parameter, so a link opens it directly:

```
https://<varroa-host>/controllers/<cluster>/<namespace>/<cell>?tab=cell
```

An unknown value falls back to the Overview tab, and so does `cell` on a controller that is not a cell. The tab has four panels, and its only action is Restart.

### Topology

The panel names the pool, the profile, the running pod instance, the previous one, the unready-since time, and the tenant warnings. A caller holding cluster-wide `cellpools:read` also sees the pool's `maxTenantsPerCell`, its `maxRunsPerTenant` run cap, and its `PoolReady` verdict. Every other caller sees neither, and the panel states the reason.

A tenant table lists one row for each tenant, naming the tenant, its folder, and its placement state. A tenant is `placed` when the applied tenant set names it, and `rehydrating` otherwise.

Each row also names that tenant's **Role** here: `slot <n>` for an active member, and `standby` for a standby. A standby holds the tenant folder and runs no build. The value comes from the cell usage read. Read [Tenant slots](tenant-slots.md).

Two more columns, the agent namespace and the placement generation, also come from the cell usage read, so a member cell shows them too.

A tenant links to its pipelines page only for a caller who may read that tenant. Every other caller sees the name as text.

### Capacity and usage

The panel renders the cell usage read: the tenant cap, the free slots, the queue length, and the per-tenant counts with their totals. The counts come from the build records, while the page header reports the Jenkins instance, so the two can differ.

### Permissions

The panel lists one block for each tenant, with the subjects, the pipelines verbs, and the job globs of each grant, plus the tenant's generated Jenkins item role and its folder pattern. It edits no binding, and a link opens the Access page. A tenant that no binding and no Team membership reaches says so.

### Script approvals

The panel lists the script signatures that the sandbox refused in this cell and that no operator approved. Each entry names up to three tenant jobs that requested it, and links to the build when the caller may read that tenant. The panel is read-only. An operator approves a signature in the pool declaration. See [Script approvals in a cell](script-approvals.md).

## The Cells page

Open `/cells`. The page and its navigation row need cluster-wide `cellpools:read`. A namespace-scoped grant admits neither.

The page renders one block for each cell pool of each cluster. The block head names the cluster
and the pool, for example `east / lts-2-570`. Each block states:

| Value | Meaning |
|---|---|
| Warm cells | Cells that hold no tenant |
| Placed cells | Cells that hold at least one tenant |
| Unready cells | Cells whose `CellReady` condition is not True |
| Free slots on placed cells | The spare capacity of the placed cells only |
| Max cells | The cap on the pool's cells, placed included |
| Pool ready | The `PoolReady` condition and its reason |

The warm-cell count and the free-slot count measure different things. Do not add them.

A table under each block lists that pool's cells: the cells of the block's cluster whose pool label names the block's pool. Each row links to that cell's Cell tab. Cell, Phase, Age, and Unready since come from the controllers list. Tenants over cap, Queue, Runs 24 h, and Failures 24 h come from the batch cell usage read, one read for each cluster. A cell name is unique inside one cluster only, so the page joins on the cluster, the namespace, and the name. A cell whose cluster answered no read shows **not available**, which is not a count of zero. A pool that owns no cell states the reason.

## The scale control

Each pool block carries an inline scale control for a caller who holds `cellpools:update`. Every other caller sees no control at all.

The control edits `warm` and `maxCells`. Press **Apply** to write them. The write goes to the pool on the block's own cluster, and the operator of that cluster applies it. The write carries only the dial you changed, so another operator's value is kept, and an Apply with no change sends nothing.

A new `warm` below the reported warm-cell count drains free cells, and the control asks for a confirm that names both numbers. A drain never touches a cell that holds a tenant.

A `409` means another writer changed the pool. The control states the conflict, reads the pool again, keeps your values, and retries nothing. A refused write, such as a `maxCells` below `warm`, states the API message, and the next Apply clears it. The control changes the pool's size only: create and delete a pool with `kubectl` or GitOps.

## Two caveats

**A cell in a remote cluster reports no queue length.** The mite gauges cover the local cluster only, so the Queue column and the Capacity panel name the reason. Neither renders zero, because zero means an empty queue.

**A pool cap and a reported cap can differ.** The cap resolves with one first-match chain: the cell's own `spec.cell.maxTenants` when it is not zero, then the pool's `maxTenantsPerCell`, then the chart value. It is never a minimum. The two differ only when the cell's own value wins, and the tab then states both.

## Evacuate is operator-written state

The operator sets `status.cell.evacuate` when a cell stays unready past twice its grace period, and clears it on recovery. No API accepts a write to it, and no page carries an evacuate action. The Cell tab renders the flag as state.

## Degraded modes

| Missing | Result |
|---|---|
| Cluster-wide `cellpools:read` | The `/cells` route and its navigation row do not render |
| `controllers:read` on a cell | That cell has no row, and the table says it lists readable cells only |
| The build record store | The batch read answers `503`, and the table drops its four usage columns |
| An answer from a member cluster | The page names that cluster and the error, and shows no block for its pools |
| Cluster-wide `rolebindings:read` | The Permissions panel names the missing verb and lists no grant |

The page never states how many cells a caller cannot see.

## The grants read

The Permissions panel reads one path:

```
GET /api/v1/cells/{namespace}/{name}/grants?cluster=<cluster>
```

The read applies the controller read check first, then cluster-wide `rolebindings:read`, because the body names subjects.

| Code | Condition |
|---|---|
| `200` | Both gates pass |
| `403` | The controller check passes and the caller holds no `rolebindings:read` |
| `404` | No such controller, a controller that is not a cell, or a refused controller check |

One code for the three `404` conditions confirms no cell's existence.

The RBAC resolver backs the read, so it answers no `503`. It returns at most 50 grants for each tenant, narrowing grants first, and flags a cut list as `truncated`.

## Refresh

Every cell surface polls. The pool and usage reads refresh every 15 seconds. The grants read holds a 60-second stale time and no interval. The Cell tab's reads run only while that tab is open, and no cell surface opens an event stream.
