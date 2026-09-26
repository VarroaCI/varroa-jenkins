# Tenant Slots

A pipelines tenant can run on several cells. Each such cell is a **member**. Read
[Cells and pipelines tenants](cells.md) first.

## Slots and standbys

`Team.spec.pipelines.members` is the tenant's active slot count, not `Team.spec.members`, which
lists local usernames.

```yaml
spec:
  pipelines:
    profile: jenkins-lts
    agentNamespace: payments-agents
    members: 2      # active slots, 1 to 16, default 1
    standby: 1      # warm members, 0 to 8, default 0
```

An **active** member holds one slot index, dense from `0` to `members - 1`. A **standby** holds
the tenant folder, no slot, and runs no build.

A cell holds at most one member of a tenant, so a tenant asks its pool for `members + standby`
cells. Every member consumes one unit of `maxTenantsPerCell`: a standby holds a real folder and
heap.

The placement pass never creates a cell. A pool that cannot meet the demand reports `SlotPending`;
raise its `maxCells`. A tenant that holds every slot but not its standby reports `StandbyPending`,
and one whose members all report applied reports `MembersApplied`.

Read the members:

```bash
kubectl get team payments -o jsonpath='{.status.placement.members}'
```

## Every job has one home slot

The recorder maps each job path to one slot, as its only writer. A job runs only on its home
slot's holder; a claim from any other member answers `NOT_HOME`. One writer for each job keeps
`disableConcurrentBuilds`, `previousBuild`, workspace reuse, and throttling true: all observe one
Jenkins.

The map uses a consistent hash. A growth from `N` to `N + 1` slots moves about one job in `N + 1`,
onto the new slot. A job the recorder has never seen resolves to slot 0 until the next round.

A change to `members` binds or releases cells, then the recorder recomputes the map. A job whose
home changed takes its history to the new holder: the recorder refreshes its records and raises
its build number. A scale moves no running build. A scale-down releases the highest indexes, so
they stay dense. No cell refreshes a released slot's agent pods, so the operator deletes them within
about 16 minutes of their last refresh.

## Jobs that trigger each other

Varroa forwards a cross-slot trigger through the ledger.

- A `build job:` step with `wait: false` reaches a job on another slot.
- A `build job:` step that waits fails at once, and names `varroa.build`, which waits across slots.
- A native `upstream` trigger fires across slots.
- A cross-slot `build job:` shows no downstream link. `varroa.build` returns a handle instead.
- A forwarded build carries text parameters only; a file parameter does not cross a slot.

Two members that fire one cron inside one minute build the job once. A fire that crosses a minute
boundary builds it twice.

## A standby takes a slot whose holder dies

When the operator confirms a slot holder's pod gone, it promotes a standby into that slot: the one
with the newest applied time, ties broken by name.

The promoted cell already holds the tenant folder and its history, so the slot stays warm. It
keeps its slot index and job set, so no job's home changes. The agent Service
`varroa-agents-<team>-<slot>` keeps its name, and its selector moves to the promoted cell. The
promoted cell deletes the old holder's agent pods within about six minutes of their last refresh.

Every other slot keeps building. A claim carrying the generation from before the promotion
answers `NOT_READY`, a retry and never a cancel.

## Two surfaces reach slot 0 only

An [external runner](runners.md) attaches to slot 0. A job homed on another slot that asks for
that runner passes the fence, then waits forever for an executor. Give such a job a label the
tenant's own cloud serves.

A [webhook](../tutorials/pipelines.md) replay reaches slot 0. A job homed on another slot needs
its own trigger.

A tenant with one active slot, the default, meets neither.
