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

The Team form edits both counts: **Active slots** sets `members` and **Standby members** sets `standby`.

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

The recorder maps each job path to one slot. A job runs only on its home
slot's holder; a claim from any other member answers `NOT_HOME`. One writer for each job keeps
`disableConcurrentBuilds`, `previousBuild`, and throttling true: all observe one Jenkins.

The map uses a consistent hash. A growth from `N` to `N + 1` slots moves about one job in `N + 1`,
onto the new slot. A new job takes its slot at once.

A change to `members` binds or releases cells, then the recorder recomputes the map. A job whose
home changed takes its history and its trigger rows to the new holder, and its build number rises.
A scale moves no running build. A scale-down releases the highest indexes.
A released slot's agent pods go as [Agent pod lifetime](tenant-pod-templates.md#agent-pod-lifetime) states.

## Jobs that trigger each other

Varroa forwards a cross-slot trigger through the ledger.

- A `build job:` step with `wait: false` reaches a job on another slot.
- A `build job:` step that waits fails at once, and names `varroa.build`, which waits across slots.
- A native `upstream` trigger, a `varroaUpstream` trigger, a backstop cron fire, and an automatic retry reach the job's home slot.
- A cross-slot `build job:` shows no downstream link. `varroa.build` returns a handle instead.
- A forwarded build keeps its run id and text parameters; a file parameter does not cross a slot.

Two members that fire one cron inside one minute build the job once. A fire that crosses a minute
boundary builds it twice.

## A standby takes a slot whose holder dies

When the operator confirms a slot holder's pod gone, it promotes a standby into that slot: the one
with the newest applied time, ties broken by name.

The promoted cell already holds the tenant folder and its history, so the slot stays warm. It
keeps its slot index and job set, so no job's home changes. The agent Service
`varroa-agents-<team>-<slot>` keeps its name, and its selector moves to the promoted cell. The
old holder's agent pods go as [Agent pod lifetime](tenant-pod-templates.md#agent-pod-lifetime) states.

Every other slot keeps building. A claim carrying the generation from before the promotion
answers `NOT_READY`, a retry and never a cancel.

## Build state across slots

Each slot holder is its own Jenkins. A cell writes artifacts and stashes under its own object
prefix, and computes every key from that prefix. A **move** below is a resume or a job home on
another cell: a promoted standby, a slot scale, or a new cell for the slot. A new pod of the same
cell is not a move.

| State | Kept on a new pod of the same cell | Kept after a move |
|---|---|---|
| Workspace of a running `node` block | No | No |
| Workspace of an earlier build | No, unless an idle pod kept it | No |
| Stash of the running build | Yes | No |
| Preserved stash, for a restart | Yes | No, the `unstash` of a restarted stage fails |
| Archived artifact, read through Varroa | Yes | Yes |
| Archived artifact, read by Jenkins | Yes | No |

### Workspace

A cell has no executors. By default, each build runs on a new agent pod. Each build starts with an
empty workspace, also on one slot. `ws`, `dir`, and `customWorkspace` select a path in that pod.
Check out the source in each build. Keep caches in an external store.

A pod template with `idleMinutes` can keep a pod for a later build on the same cell. A restart of
the cell ends that pod. A move ends it too. An idle pod keeps a workspace only on an uninterrupted
cell.

A resume does not keep a running `node` block today. The resumed build waits five minutes for its
agent, then ends `ABORTED`. Rerun the build.

### Stash

A later stage can `unstash` a stash from an earlier stage, on any agent pod. After a move, an
`unstash` of a stash from before the move fails with `No such saved stash`. Run the build again.

`preserveStashes` keeps stashes only for a [restart from a stage](../tutorials/run-operations.md).
A preserved stash survives a pod roll of the same cell. A move of the job to another cell ends it.
A restart keeps its restart copy for 7 days by default. A restart after a move does not restore
a stash from the old cell.

### Artifacts

The record store keeps the exact key of each archived artifact. The build page and
`GET /runs/{run}/artifacts` read that key, so the artifacts of every run stay readable after a
move. Jenkins itself lists only the artifacts of builds that the current cell wrote.

The cell does not pin the Copy Artifact plugin. A `copyArtifacts` step fails with `No such DSL
method`. To copy an artifact from another job of the same tenant, use a run handle. Use it inside
a `node` block, on any slot. Give the job name relative to the tenant folder. `lastSuccessful`
returns `null` when the job never succeeded:

```groovy
def last = varroa.lastSuccessful(job: 'build-app')
if (last != null) {
    last.copyArtifact(path: 'dist/app.tar.gz', target: 'app.tar.gz')
}
```

A call inside one tenant needs no export. Each call copies one path. See
[Cross-tenant pipelines](../config/cross-tenant-pipelines.md).

## Two surfaces reach slot 0 only

An [external runner](runners.md) attaches to slot 0. A job homed on another slot that asks for
that runner passes the fence, then waits forever for an executor. Give such a job a label the
tenant's own cloud serves.

A [webhook](../tutorials/pipelines.md) replay reaches slot 0. A job homed on another slot needs
its own trigger.

A tenant with one active slot, the default, meets neither.
