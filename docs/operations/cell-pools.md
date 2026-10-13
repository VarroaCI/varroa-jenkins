# Cell Pools

A `CellPool` is a cluster-scoped custom resource. It holds the size, the capacity, and the
template of one pool of cells that serve the pipelines tenants of a single version profile. The
operator sizes each pool from its own spec on a 60-second ticker, and reports the result in
`status`.

Read [Cells and pipelines tenants](cells.md) first.

## Spec fields

| Field | Type | Meaning |
|---|---|---|
| `profileRef` | string, required, immutable | the `JenkinsVersionProfile` the pool's cells run |
| `warm` | int32, default 0 | target count of free cells the pool keeps ready |
| `maxCells` | int32, default 0 | cap on the pool's cells, placed cells included. 0 means unlimited |
| `maxTenantsPerCell` | int32, minimum 1, default 10 | capacity a pool cell uses when its own `spec.cell.maxTenants` is zero |
| `maxRunsPerTenant` | int32, 0 to 10000, default 0 | builds one tenant runs at once on one cell; zero means no cap. A build over the cap waits. Set it on the object; the scale API does not change it |
| `unreadyGracePeriod` | duration, optional | how long a pool cell may stay unready before the operator rolls its pod |
| `cellTemplate` | object, optional | `resources`, `miteResources`, and `podOverrides` for the cells the pool creates |
| `libraries` | list, optional | trusted shared libraries that every cell of the pool serves. See [Pool shared libraries](cell-libraries.md) |
| `approvedSignatures` | list of strings, optional | script-security signatures that every cell of the pool approves. See [Script approvals](script-approvals.md) |
| `mail` | object, optional | SMTP server for the `mail` and `emailext` steps of every cell of the pool. See [Slack and email](notifications.md#slack-and-email) |
| `giteaServers` | list, optional, max 16 | Gitea and Forgejo servers every cell of the pool declares. See [Commit status](commit-status.md#forgejo-and-gitea) |

`profileRef` is immutable. A pool's cells carry the profile label they were created with, and a
change would orphan every one of them. To serve another profile, create another pool.

## Status fields

| Field | Meaning |
|---|---|
| `warmCells` | owned cells that hold no tenant |
| `placedCells` | owned cells that hold at least one tenant |
| `unreadyCells` | owned cells whose `CellReady` condition is not `True` |
| `freeTenantSlots` | spare capacity of the placed cells only |
| `tenants` | every tenant on the pool, sorted |

`PoolReady` reasons:

| Reason | Status | Holds when | Action |
|---|---|---|---|
| `ProfileMissing` | `False` | `profileRef` names no `JenkinsVersionProfile` | Create the `JenkinsVersionProfile`, or fix `profileRef`. |
| `RecordStoreUnsupported` | `False` | `recordStore.enabled` is false | Enable the record store. |
| `BlobTierUnsupported` | `False` | `recordStore.blobs.mode` is not `s3` | Set the blob tier to `s3`. |
| `ArtifactSecretMissing` | `False` | no artifact credential is configured, or `<artifactSecret>-<pool>` (STS mode: the issuer Secret) does not exist with data | Create the Secret the message names. |
| `ArtifactPluginMissing` | `False` | the plugin set a cell of the pool's version resolves does not pin `artifact-manager-s3` | Pin `artifact-manager-s3` in the version profile. |
| `SecretsMissing` | `False` | the pool holds no usable confidential store | Read [Pool secrets](#pool-secrets) below. |
| `CellCapReached` | `False` | `maxCells` is above zero and the pool owns that many cells | Raise `maxCells`. |
| `Ready` | `True` | the profile exists, the pool is below its cap, and no fill precondition refused a fill | None. |

The first four precondition reasons report only on a pass that wanted a new cell. A pool at its
target therefore reports `CellCapReached` or `Ready`, even with a missing artifact Secret.
`SecretsMissing` is the exception: the store is a property of the pool, not of a fill.

Reasons rank from the top row down. The status reports the highest that applies.

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

Each value seeds a new pool only. A Helm upgrade never rewrites the spec of an existing pool.

The chart refuses a `cells.maxTenantsPerCell` or `cells.warmPool.perProfile` below 1.

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

Creation and deletion are `kubectl` or GitOps operations. The API and the CLI only read and scale
a pool.

## Default template

`cells.cellTemplate` seeds the `cellTemplate` of each pool the operator creates:

| Key | Default |
|---|---|
| `resources` | requests `cpu: 1`, `memory: 3Gi`; limit `memory: 6Gi` |
| `miteResources` | requests `cpu: 10m`, `memory: 32Mi`; limit `memory: 128Mi` |
| `podOverrides.jvmOpts` | `-XX:MaxRAMPercentage=50` |
| `podOverrides.affinity` | preferred pod anti-affinity on `kubernetes.io/hostname` |

The heap takes half the memory limit. The anti-affinity spreads the pods of all pools across
nodes, and a small cluster still schedules them. Values merge into the default. Set a key to
`null` to remove it:

```yaml
cells:
  cellTemplate:
    resources:
      requests: {cpu: "2", memory: 6Gi}
      limits: {memory: 12Gi}
    podOverrides:
      affinity: null
```

Set `cells.cellTemplate: null` for pools with no template. A pool that exists keeps its own
template. Edit the pool to change it, as [Template edits](#template-edits) describes.

## Scale a pool

Each cluster of the brood holds its own pools. A pool name is unique inside one cluster only.

```bash
varroactl get cellpools
varroactl describe cellpool jenkins-lts
varroactl scale cellpool jenkins-lts --warm 3 --max-cells 8
varroactl scale cellpool lts-2-570 --cluster east --warm 2
```

`get cellpools` lists every cluster, with a `CLUSTER` column. A command on one pool uses the core
cluster when you give no `--cluster`.

`GET /api/v1/cellpools` adds a `cluster` field to each pool and a `clusters` array with one read
status for each cluster. A cluster that does not answer has `ok: false`. `GET` and `PATCH
/api/v1/cellpools/{name}` take `?cluster=<name>`, and an unreachable member gives `502`. A member
operator on an older image does not answer until you upgrade it.

The same edit through `kubectl`:

```bash
kubectl patch cellpool jenkins-lts --type=merge -p '{"spec":{"warm":3}}'
```

A watch wakes the operator, so a scale edit takes effect within seconds rather than after the
60-second backstop.

Lowering `warm` drains free cells only. The pool never deletes a cell that holds a tenant, nor one
a tenant is being placed on. Such a cell leaves the pool one above its target until the next pass.
Deleting a pool drains its free cells. A placed cell keeps every label and lives on as an ordinary
cell, resolving its capacity from its own `spec.cell.maxTenants` or from `cells.maxTenantsPerCell`.

### Scale up a pool

A fill needs the install-wide gates of the `PoolReady` table. In static mode it also needs
`<artifactSecret>-<pool name>`. In STS mode it needs no Secret per pool. See
[Artifact credentials](artifact-credentials.md).

## Cold start

A pool with `warm: 0` holds no cell until a tenant waits for placement, and the pass then creates
one. The tenant reports `Placed=False` with reason `AwaitingCell` while that cell boots, usually
one to three minutes. Set `warm` to 1 or more to remove that wait.

## Template edits

The pool re-stamps `spec.version`, `spec.resources`, `spec.podOverrides`, and
`spec.miteSpec.resources` on each free cell every tick. It keeps the other `spec.miteSpec` fields,
and never edits a cell that holds a tenant. A `cellTemplate` edit therefore rolls the free cells
at once, and reaches a placed cell only after it drains. To grow a placed tenant, move
it to a pool with a larger template.

## Pool secrets

A cell keeps `$JENKINS_HOME` on an `emptyDir`, so each instance would mint its own `secrets/`
directory and reject the last one's agents. Each pool therefore owns one Jenkins confidential
store: the Secret `varroa-cell-secrets-<pool>` in the operator namespace, labelled
`varroa.dev/cell-pool: <pool>`. Every cell of the pool starts with it, and each key name is also
the file name Jenkins reads.

| Key | Protects |
|---|---|
| `master.key` | every other key of the store |
| `jenkins.slaves.JnlpSlaveAgentProtocol.secret` | each inbound agent's own secret |
| `hudson.util.Secret` | every `Secret` value in a config file |
| `org.jenkinsci.main.modules.instance_identity.InstanceIdentity.KEY` | the instance identity |

**The operator mints the store once for each pool, and never rotates it.** It never overwrites a
key that holds a value, and builds a missing key under the existing `master.key`. A repaired key
reaches only the cells that start after the repair, so deleting a key is never a rotation path.

Two states never repair themselves: a store with no `master.key`, and a key that does not decrypt
under it. The pass then writes nothing and reports `PoolReady=False` with `SecretsMissing`. Delete
the Secret and roll the pool to recover. A pool cell that finds no usable store does not start:
its reconcile blocks with `ReconcileBlocked=True` and the reason `CellSecretsFailed`.

**Blast radius.** Every cell of a pool accepts every agent secret that pool minted, which is the
point of the shared store. Set `maxTenantsPerCell: 1` to narrow the radius to one tenant. The
operator holds each pool's master key, beside every bootstrap Secret and the mite CA.

**Rotate a pool store by hand.**

1. Delete the Secret: `kubectl delete secret varroa-cell-secrets-<pool> -n varroa-system`.
2. Wait for the next pool tick, and confirm `PoolReady` reports `Ready`.
3. Roll every cell: `kubectl delete pod -n varroa-system -l varroa.dev/cell-pool=<pool>`.

The roll drops every running agent. Every value encrypted under the old `hudson.util.Secret`
becomes unreadable, so re-apply those credentials.

## Cell labels

A cell carries three labels. `varroa.dev/cell-pool` names its pool, `varroa.dev/cell-profile`
names its profile, and `varroa.dev/warm` records that a pool created it. A pool owns a cell only
while the pool its label names still exists.

A pool cell's pod also carries `varroa.dev/cell-pool`, set when the operator builds the pod. A
running cell gets it at its next roll. A `podOverrides.podLabels` entry cannot change it.

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
