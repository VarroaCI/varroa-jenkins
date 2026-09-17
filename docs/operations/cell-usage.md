# Cell Usage

A cell hosts many tenants in one Jenkins process. Three reads report how busy that process is:
two for an operator, and one for a team user. Read [Cells and pipelines tenants](cells.md) first.
Every read needs the [build record store](build-records.md), and answers `503` without it.

## Operator reads

```bash
# Every cell in the local cluster the caller may read.
curl -H "Authorization: Bearer $TOKEN" https://<dashboard>/api/v1/cells/usage

# One cell.
curl -H "Authorization: Bearer $TOKEN" \
  https://<dashboard>/api/v1/cells/varroa-system/cell-jenkins-lts/usage
```

Both take an optional `cluster` parameter, which defaults to the local cluster. The list read also
takes an optional `namespace` parameter.

Both apply the `controllers:read` check on each cell. The list read drops each refused cell. The
single read answers `404` for an unknown controller, for one that is not a cell, and for a refused
caller. The MCP tool `get_cell_usage` answers the same value. See
[Agent tools](../agents/tools.md).

## The four counts

Each read reports four counts for each tenant, and the same four for the cell.

| Count | Meaning |
|---|---|
| `runningNow` | runs in the status `RUNNING`, with no time bound |
| `runsLast24h` | runs created in the last 24 hours, whatever their status |
| `failedLast24h` | runs in that window that finished and did not succeed |
| `jobs` | distinct job paths the cell reports for the tenant |

Each tenant row also carries `role`: `slot <n>` for an active member, and `standby` for a standby.
A standby holds the tenant folder and runs no build, so its running count is zero. Read
[Tenant slots](tenant-slots.md).

The window is a fixed 24 hours, and no caller chooses another. `runningNow` carries no window. A
`TRIGGER_FAILED` run counts in no value, because it started no build. A tenant that left the cell
leaves its totals. Use the run list for the full history.

## The tenant cap

`maxTenants` resolves the first-match cap chain [Cell views](cell-views.md) states. `freeSlots` is
that cap minus the placed tenant count, and never below zero. A member of any role counts as one
tenant unit.

## A remote cell reports no queue length

`queueLength` comes from the mite heartbeat, which the dashboard back end holds for the local
cluster only. A remote cell reports no `queueLength` and no `queueLengthAt`: both are absent,
never zero, because zero means an empty queue. Its Jenkins health comes from the stored mite
status on the `Controller`.

## Your cell

A team user reads its own view of the cell:

```bash
curl -H "Authorization: Bearer $TOKEN" https://<dashboard>/api/v1/pipelines/payments/cell
```

The read applies the same tenant read check the team page applies, and it carries no other
tenant's name, job path, run, or count. `otherTenantCount` is a count, never a list.

`members` carries one entry for each member of the tenant, with that member's cell, its role, and
its own four counts. Every other field describes the tenant's slot 0.

The `state` field holds one of five values.

| State | Meaning |
|---|---|
| `placed` | every active slot holds an applied cell |
| `moving` | the pass is moving the tenant to another cell |
| `rehydrating` | a member's folder is not built yet |
| `unplaced` | the tenant waits for its first cell |
| `notCellHosted` | the team is a single controller |

`reason` carries the `Placed` condition's reason. For `unplaced` the body holds `state` and
`reason` alone, and for `notCellHosted` it holds `state` alone.

`canOpenCell` is true only when the caller also holds `controllers:read` on the cell. It tells the
dashboard whether to render a link, and it gates no field.

The dashboard renders it as the **Your cell** card. See
[Read and Run Pipelines](../tutorials/pipelines.md#your-cell-card).
