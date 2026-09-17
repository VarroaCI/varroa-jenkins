# Tenant Slots

A pipelines tenant can run on more than one cell. Each such cell is a **member**. Read
[Cells and pipelines tenants](cells.md) first.

## Two fields are named members

`Team.spec.members` lists local usernames. `Team.spec.pipelines.members` is the tenant's active
slot count. This page is about the second one.

## Slots and standbys

```yaml
spec:
  pipelines:
    profile: jenkins-lts
    agentNamespace: payments-agents
    members: 2      # active slots, 1 to 16, default 1
    standby: 1      # warm members, 0 to 8, default 0
```

An **active** member holds one slot index, dense from `0` to `members - 1`. A **standby** holds
the tenant folder and no slot, and it runs no build.

A cell holds at most one member of a tenant, so a tenant asks its pool for `members + standby`
cells. Every member consumes one unit of the cell's `maxTenantsPerCell`: a standby holds a real
folder, a real cloud, and real heap on that Jenkins process.

The placement pass never creates a cell. A pool that cannot meet the demand reports `SlotPending`.
Raise the pool's `maxCells` to clear it. A tenant whose every slot holds a cell and whose standby
does not reports `StandbyPending`. One whose every member cell reports it applied reports
`MembersApplied`.

Read the member list:

```bash
kubectl get team payments -o jsonpath='{.status.placement.members}'
```

## Every job has one home slot

The recorder maps each job path to one slot, and it is the only writer of that map. A job runs
only on its home slot's holder, and a claim from any other member answers `NOT_HOME`. One writer
for each job keeps the per-process rules true: `disableConcurrentBuilds`, `previousBuild`,
workspace reuse, and throttling all observe one Jenkins.

The map uses a consistent hash. A growth from `N` to `N + 1` slots moves about one job in `N + 1`,
and every job that moves lands on the new slot. A job the recorder has never seen resolves to slot
0 until the next round stores its real home.

A change to `members` binds or releases cells, then the recorder recomputes the map. A job whose
home changed takes its history to the new holder: the recorder refreshes that job's records and
raises its next build number. A scale moves no running build, and a scale-down releases the
highest slot indexes, so the surviving indexes stay dense.

## A standby takes a slot whose holder dies

When the operator confirms a slot holder's pod gone, it promotes a standby into that slot. It
chooses the standby with the newest applied time, and breaks a tie by cell name.

The promoted cell already holds the tenant folder and its build history, so it takes the slot
warm. The slot keeps its index and its job set, so no job's home changes. The agent Service
`varroa-agents-<team>-<slot>` keeps its name, and its selector moves to the promoted cell.

Every other slot keeps building. A claim carrying the generation from before the promotion answers
`NOT_READY`, which is a retry and never a cancelled build.

## Two surfaces reach slot 0 only

An [external runner](runners.md) attaches to slot 0. A job homed on another slot that asks for
that runner passes the claim fence, then waits for an executor its own cell never gains. The build
is not cancelled: it waits. Give such a job a label the tenant's own cloud serves.

A [webhook](../tutorials/pipelines.md) replay reaches slot 0. A job homed on another slot needs
its own trigger until a later release forwards the replay.

A tenant with one active slot, the default, meets neither limit.
