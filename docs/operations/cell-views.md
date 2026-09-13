# Cell views

The dashboard has three operator surfaces for cells: a kind column on the controllers table, a Cell tab on a cell's controller page, and a fleet Cells page. Each surface renders reported values. No surface computes a capacity number.

Read [Cells](cells.md) first for what a cell is. Read [Cell pools](cell-pools.md) for what a pool sizes, and [Cell usage](cell-usage.md) for the load numbers.

## The kind column

The controllers table has a Kind column between Phase and Version. Each row reads `Cell` or `Stateful`.

A cell row also names its pool and states its fill as `tenants / cap`. A cell that holds no tenant carries a **warm** badge.

A kind filter sits below the phase chips. It has two chips, `Cells` and `Stateful`, each with its count. The group has no `All` chip: a chip toggles off, and two chips off lists every kind. The kind filter and the phase filter narrow the same list.

The column renders in the brood picker too. A brood operation on a cell is a different act from one on a stateful controller.

## The Cell tab

Open a cell's controller page. The page shows a **Cell** tab. A controller that is not a cell shows no Cell tab.

The active tab lives in the `tab` query parameter, so a link opens the tab directly:

```
https://<varroa-host>/controllers/<cluster>/<namespace>/<cell>?tab=cell
```

An unknown value falls back to the Overview tab. The value `cell` on a controller that is not a cell falls back too.

The tab has three panels. Its only action is Restart.

### Topology

The panel names the pool, the profile, the running pod instance, the previous pod instance, the unready-since time, and the tenant warnings.

A caller who holds the cluster-wide `cellpools:read` verb also sees the pool's `maxTenantsPerCell` and its `PoolReady` verdict. Every other caller sees neither value, and the panel states the reason.

A tenant table lists one row for each tenant. Each row names the tenant, its folder, and its placement state. A tenant is `placed` when the applied tenant set names it, and `rehydrating` otherwise.

Two more columns, the agent namespace and the placement generation, come from the Teams read. That read needs the administrator grant, not a `teams` verb: no role carries such a verb. For every other caller the two columns are absent, and the table states the reason. A refused Teams read drops the same two columns and reports no error.

A tenant links to its pipelines page only when the caller may read that tenant's pipelines. Every other caller sees the tenant name as text.

### Capacity and usage

The panel renders the cell usage read: the tenant cap, the free slots, the queue length, and the per-tenant counts with their totals.

The counts come from the build records. The page header reports the Jenkins instance instead, so the two numbers can differ. The panel labels its counts and states the start of the 24-hour window.

### Permissions

The panel lists one block for each tenant. Each block names the grants that reach the tenant, with the subjects, the pipelines verbs, and the job globs of each grant. Each block also names the tenant's generated Jenkins item role and its folder pattern.

The panel reads. It edits no binding. A link opens the Access page, where a binding is edited.

A tenant that no binding and no Team membership reaches says so. An empty block would read as a display fault.

## The Cells page

Open `/cells`. The page and its navigation row need the cluster-wide `cellpools:read` verb. A namespace-scoped grant admits neither.

The page renders one block for each cell pool. Each block states:

| Value | Meaning |
|---|---|
| Warm cells | Cells of the pool that hold no tenant |
| Placed cells | Cells of the pool that hold at least one tenant |
| Unready cells | Cells whose `CellReady` condition is not True |
| Free slots on placed cells | The spare capacity of the placed cells only |
| Max cells | The cap on the pool's cells, placed cells included |
| Pool ready | The `PoolReady` condition and its reason |

The warm-cell count and the free-slot count measure different things. Do not add them.

A table under each block lists that pool's cells:

| Column | Source |
|---|---|
| Cell | The controllers list |
| Phase | The controllers list |
| Tenants over cap | The batch cell usage read |
| Queue | The batch cell usage read |
| Runs 24 h | The batch cell usage read |
| Failures 24 h | The batch cell usage read |
| Age | The controllers list |
| Unready since | The controllers list |

Each row links to that cell's Cell tab.

The four usage columns come from one read for each cluster the list names. A cell name is unique inside one cluster only, so the page joins on the cluster, the namespace, and the name together. A cell whose cluster the usage read did not answer for shows **not available** in those four columns. That state is not a count of zero.

A pool that owns no cell states the reason. Either `warm` is zero and no tenant waits, or no Team requests the pool's profile.

## The scale control

Each pool block carries an inline scale control for a caller who holds the `cellpools:update` verb. Every other caller sees no control, enabled or disabled.

The control edits `warm` and `maxCells`. Press **Apply** to write them. The write carries only the dial you changed, so a value another operator changed at the same time is kept. An Apply with no change sends nothing.

A new `warm` below the reported warm-cell count drains free cells. The control then asks for a confirm. The confirm names the reported count and the value you typed. A drain never touches a cell that holds a tenant. Every other edit applies with no confirm.

The write can answer `409` when another writer changes the pool at the same time. The control states the conflict, reads the pool again, and keeps the values you typed. Press Apply again to write them. The control retries nothing on its own.

The write can also be refused. A `maxCells` below `warm` breaks a rule of the CRD, for example. The control states the message the API returned, and the next Apply clears it.

The control changes the pool's size only. Create a pool and delete a pool with `kubectl` or GitOps.

## Two caveats

**A cell in a remote cluster reports no queue length.** The mite gauges cover the local cluster only. The Queue column and the Capacity panel then render an em dash and name the reason. Neither surface renders zero, because zero means an empty queue.

**A pool cap and a reported cap can differ.** The cell usage read and placement resolve the cap with one first-match chain: the cell's own `spec.cell.maxTenants` when that value is not zero, then the pool's `maxTenantsPerCell`, then the chart value. The chain is never a minimum over the tiers. The two numbers differ only when the cell's own value wins. The Cell tab and the pool block then state both. No surface computes either one.

## Evacuate is operator-written state

`status.cell.evacuate` is set by the operator when a cell stays unready past twice its grace period. The operator clears it on recovery.

No API accepts a write to that field, and no page carries an evacuate action. The Cell tab renders the flag as state.

## Degraded modes

| Missing | Result |
|---|---|
| The cluster-wide `cellpools:read` verb | The `/cells` route and the navigation row do not render |
| The `controllers:read` verb on a cell | That cell has no table row, and the table states that it lists readable cells only |
| The build record store | The batch usage read answers `503`, the table drops its four usage columns, and the page states why |
| The cluster-wide `rolebindings:read` verb | The Permissions panel names the missing verb and lists no grant |

The page never states how many cells a caller cannot see. The pool counts already come from the pool status.

## The grants read

The Permissions panel reads one path:

```
GET /api/v1/cells/{namespace}/{name}/grants?cluster=<cluster>
```

The read applies two gates. It applies the controller read check on the cell first. It then applies the cluster-wide `rolebindings:read` verb, because the body names subjects.

| Code | Condition |
|---|---|
| `200` | Both gates pass |
| `403` | The controller check passes and the caller holds no `rolebindings:read` |
| `404` | No controller of that name, a controller that is not a cell, or a refused controller check |

One code for the three `404` conditions keeps the surface from confirming that a cell exists. The `403` hides nothing: that caller already knows the cell exists.

The read is backed by the RBAC resolver. It reads no build records, so it answers no `503`. It returns at most 50 grants for each tenant and reports a cut list with a `truncated` flag. The narrowing grants come first.

## Refresh

Every cell surface polls. The pool reads and the usage reads refresh every 15 seconds, the cadence the controllers list uses. The grants read holds a 60-second stale time and no interval, because a role binding changes rarely. The Cell tab's own reads run only while that tab is open. No cell surface opens a server-sent event stream.
