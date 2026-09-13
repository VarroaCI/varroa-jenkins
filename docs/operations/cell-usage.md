# Cell Usage

A cell hosts many tenants in one Jenkins process. Three reads report how busy that process is:
two for an operator, and one for a team user.

Read [Cells and pipelines tenants](cells.md) first. That page covers what a cell is and how a
`Team` becomes a tenant.

Every read needs the [build record store](build-records.md). Without it each one answers `503`.

## Operator reads

```bash
# Every cell in the local cluster the caller may read.
curl -H "Authorization: Bearer $TOKEN" https://<dashboard>/api/v1/cells/usage

# One cell.
curl -H "Authorization: Bearer $TOKEN" \
  https://<dashboard>/api/v1/cells/varroa-system/cell-jenkins-lts/usage
```

Both reads accept an optional `cluster` parameter, which defaults to the local cluster. The list
read also accepts an optional `namespace` parameter.

Both reads apply the `controllers:read` check on each cell. The list read drops each cell the
check refuses. The single read answers `404` for an unknown controller, for a controller that is
not a cell, and for a caller the check refuses. One code for all three keeps the surface from
confirming that a cell exists.

The MCP tool `get_cell_usage` answers the same value as the single read. See
[Agent tools](../agents/tools.md).

## The four counts

Each read reports four counts for each tenant, and the same four summed over the cell.

| Count | Meaning |
|---|---|
| `runningNow` | runs that hold the status `RUNNING`. No time bound applies |
| `runsLast24h` | runs created inside the last 24 hours, whatever their status |
| `failedLast24h` | runs in the same window that finished and did not succeed |
| `jobs` | distinct job paths the cell reports for the tenant |

`runningNow` carries no time bound on purpose. A build can run longer than a day, and a long
build must stay visible.

The window is a fixed 24 hours. No caller chooses another window. Use the run list for a longer
history.

A run whose result is `TRIGGER_FAILED` counts in no value. A failed trigger started no build, so
it is no part of a load number.

`jobs` counts distinct job paths, never schedule rows. One job holds one row for each trigger
type. A job with the trigger type `none` counts before it ever builds, because it costs the
Jenkins process a folder.

A tenant that left the cell leaves the cell's totals. The read reports the load of the current
placement, not the whole history of the cell. The run list keeps the full history.

## The tenant cap

Three tiers can cap the tenants of one cell:

1. the cell's own `spec.cell.maxTenants`, when that value is not zero
2. the pool's `maxTenantsPerCell`
3. the chart value `cells.maxTenantsPerCell`

The rule is a first match. It stops at the first tier that holds a value, and it is never a
minimum over the tiers.

**This read resolves all three tiers.** It reports the cell value when that value is not zero,
else the `maxTenantsPerCell` of the pool the cell's `varroa.dev/cell-pool` label names, else the
chart value. A label that names no existing pool skips the pool tier. The dashboard names the pool
value beside this one when the cell's own value wins.

`freeSlots` is the reported `maxTenants` minus the placed tenant count, and never below zero.

## A remote cell reports no queue length

`queueLength` comes from the mite heartbeat, which the dashboard back end holds for the local
cluster only. A cell in a remote cluster reports no `queueLength` and no `queueLengthAt`. Both
fields are absent, never zero, because zero means an empty queue. The Jenkins health of a remote
cell comes from the stored mite status on the `Controller`, not from the live snapshot.

## Your cell

A team user reads its own tenant's view of the cell:

```bash
curl -H "Authorization: Bearer $TOKEN" https://<dashboard>/api/v1/pipelines/payments/cell
```

The read applies the same tenant read check the team page applies. It carries no other tenant's
name, job path, run, or count. It carries `otherTenantCount`, a count of the other tenants on the
cell, and never a list of them.

The `state` field holds one of five values.

| State | Meaning |
|---|---|
| `placed` | the tenant is bound to a cell and its folder is built |
| `moving` | the placement pass is moving the tenant to another cell |
| `rehydrating` | the tenant is bound, and its folder is not built on the cell yet |
| `unplaced` | the team is a pipelines tenant and waits for its first cell |
| `notCellHosted` | the team is a single controller, not a cell tenant |

`reason` carries the `Placed` condition's reason. For `unplaced` the body holds `state` and
`reason` alone. For `notCellHosted` it holds `state` alone.

`canOpenCell` is a hint. It is true only when the caller also holds `controllers:read` on the
cell. The hint tells the dashboard whether to render a link to the cell. It gates no field of the
body: a caller without that grant still receives every field.

The dashboard renders this read as the **Your cell** card on the team page. See
[Read and Run Pipelines](../tutorials/pipelines.md#your-cell-card).
