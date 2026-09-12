# Cells and Pipelines Tenants

A cell is a shared Jenkins controller that runs many teams' pipelines as tenants, instead of one
controller per team. Each tenant gets its own folder, its own build identity, and its own
Kubernetes agents.

## What a cell is

A cell is a `Controller` with `spec.cell.enabled: true`. Its Jenkins home is an `emptyDir`. A pod
roll loses the Jenkins local state and the mite's identity. The operator mints a fresh
bootstrap token per pod. [Rehydration](#rehydration) restores build history. `spec.cell.enabled` cannot change after creation. `spec.cell.maxTenants` can change; it
only caps how many tenants the cell may host.

A cell hosts each tenant as a top-level folder named after its `Team`. It accepts no ingress host.
Tenant traffic reaches it through the [tenant webhook door](#the-tenant-webhook-door), or through
builds the plugin authenticates internally.

Six limitations apply:

- A cell never auto-hibernates.
- `powerState: Stopped` on a cell destroys every in-flight build.
- Every branch source on a cell sees each delivered webhook. Only the job whose SCM matches
  the payload reacts.
- A rehydrated run shows no stage view, culprit list, or test detail inside Jenkins. The thin
  record holds core fields, parameters, and changesets only.
- The role-strategy admin screen reports a tenant's service identity as unknown until its
  first build runs.
- An ad-hoc job made in the Jenkins UI or script console does not survive a pod roll. Only a
  job in the tenant's [`items.yaml`](../config/items.md) bundle does.

## Prerequisites

A cell needs:

- The [build record store](build-records.md) enabled (`recordStore.enabled: true`).
- The `s3` blob tier (`recordStore.blobs.mode: s3`). A cell refuses the `postgres` tier.
- External artifact storage. Pin `artifact-manager-s3` in the cell's `JenkinsVersionProfile` and
  add a per-cell artifact `Secret`. See
  [The per-cell artifact Secret](build-logs-and-artifacts.md#the-per-cell-artifact-secret).

The operator reports `CellReady=False` until all three hold.

## Enable a cell

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: cell-jenkins-lts
  namespace: varroa-system
spec:
  version: "2.479.1"
  cell:
    enabled: true
    maxTenants: 10
```

Apply the resource and let it reach `Connected`.

**Verify:**

```bash
kubectl get controller cell-jenkins-lts -n varroa-system \
  -o jsonpath='{.status.conditions[?(@.type=="CellReady")]}'
```

Expect `"status":"True","reason":"Connected"`.

## Configure the warm pool

The warm pool pre-creates empty cells so a new tenant does not wait for a `Controller` to
provision:

```yaml
cells:
  warmPool:
    enabled: true
    perProfile: 1
  maxTenantsPerCell: 10
  unreadyGracePeriod: 5m
```

`warmPool.perProfile` sets how many empty, ready cells the operator keeps per profile that a
pipelines tenant names.
`maxTenantsPerCell` caps a cell's tenants when `spec.cell.maxTenants` is `0`.
`unreadyGracePeriod` sets how long a cell may stay unready before a roll.
See [Placement lifecycle](#placement-lifecycle).

A pool member is named `cell-<profile>-<ordinal>` and carries the labels
`varroa.dev/cell-profile` and `varroa.dev/warm`.

**Verify:**

```bash
kubectl get controller -n varroa-system -l varroa.dev/warm=true
```

Expect one `Controller` per configured `perProfile` count for each requested profile, each
reporting `WarmPool=True`.

## Make a Team a pipelines tenant

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  namespaces: ["team-payments"]
  pipelines:
    profile: jenkins-lts
    agentNamespace: team-payments
    composedBundleRef:
      name: payments-pipelines
      namespace: team-payments
```

`pipelines.agentNamespace` must be one of `spec.namespaces`, and Varroa must manage it.
`pipelines.profile` must name an existing `JenkinsVersionProfile`. `pipelines.composedBundleRef`
is optional; an unset reference places the tenant with an empty folder. Set
`pipelines.dedicated: true` to require a cell that carries no other tenant.

**Verify:**

```bash
kubectl get team payments -o jsonpath='{.status.conditions[?(@.type=="Placed")]}'
```

Expect `"status":"True","reason":"TenantPlaced"`.

## What composes a tenant

- **Folder.** The tenant's `items.yaml` is wrapped under one top-level folder named after the
  `Team`, then merged with every other tenant's folder into the cell's single manifest. The
  operator ignores `jenkins.yaml`, `plugins.yaml`, and `rbac.yaml` in a tenant's bundle: a tenant
  cannot change controller configuration. A job the bundle no longer declares leaves the folder,
  and its next trigger fails with `JOB_NOT_FOUND`. See [Build records](build-records.md).
- **Item role.** The operator synthesizes a role scoped to the folder's subtree, granting build,
  read, and replay permissions, bound to the tenant's own service identity.
- **Service identity.** Every build in the folder runs as `svc-<team>`. The plugin synthesizes
  this identity; it holds no Jenkins user record.
- **Cloud and agent namespace.** The tenant gets one restricted Kubernetes cloud pointed at
  `spec.pipelines.agentNamespace`. The folder's own permitted-clouds property names only that
  cloud. A pipeline selects it with `agent { label '<team>-default' }` or `cloud: '<team>'`;
  `agent any` lands on the cell's default cloud, which the agent fence refuses.
- **Projected credentials.** A `Secret` labelled `varroa.dev/tenant-credential: "true"` in the
  agent namespace becomes a folder-scoped Jenkins credential:

  ```yaml
  apiVersion: v1
  kind: Secret
  metadata:
    name: payments-github-token
    namespace: team-payments
    labels:
      varroa.dev/tenant-credential: "true"
    annotations:
      varroa.dev/credential-id: github-token
      varroa.dev/credential-kind: secretText
      varroa.dev/credential-description: "GitHub deploy token"
  stringData:
    secret: <token>
  ```

  `varroa.dev/credential-kind` is `usernamePassword` (data keys `username` and `password`) or
  `secretText` (data key `secret`). A `Secret` with a missing key, an unknown kind, or a
  duplicate id is skipped, and the `Team`'s `TenantWarnings` condition records the skip.
- **Folder libraries.** A tenant bundle may declare `folderLibraries` on the folder item, with
  the source retriever pipeline-groovy-lib needs.

## Isolation controls

Four controls confine a tenant to its own folder and its own agents:

1. **Network policy.** `networkPolicy.cellIsolation.enabled` renders one `NetworkPolicy` per cell.
   See [Isolate cells](../install/network-policies.md#isolate-cells).
2. **Cloud restriction.** Each tenant's Kubernetes cloud is restricted to its own folder, so one
   tenant's builds cannot provision another tenant's agents.
3. **Agent fence.** The plugin refuses a build on a node labelled for another tenant, or on a
   node with no tenant label.
4. **Build identity.** The plugin authenticates every build, including a Pipeline `node` block, as
   `svc-<team>`. That identity reads only its own folder's credentials and items.

## The tenant webhook door

A tenant's webhook URL has the shape:

```
/hibernation/<token>/teams/<team>/queue/<path>
```

served from the BFF's public host, outside `/api/v1` and login. `<path>` must match
`github-webhook/` or `generic-webhook-trigger/invoke`; any other path answers `404`.

Two credentials guard the door. Varroa mints each once, and never regenerates them:

- `status.pipelines.webhookToken` addresses the tenant and goes in the URL. Read it directly from
  the `Team` object; it is not exposed through the API or the dashboard:

  ```bash
  kubectl get team payments -o jsonpath='{.status.pipelines.webhookToken}'
  ```

- `status.pipelines.webhookSecret` proves the forge sent the body. Configure it as the webhook's
  signing secret in the forge.

The door verifies the delivery by forge: GitHub with an HMAC-SHA256 `X-Hub-Signature-256`,
GitLab with `X-Gitlab-Token`, anything else with `X-Varroa-Token`. An unsigned or mismatched
delivery answers `400`.

A delivery buffers for at most one hour. A tenant that moves cells inside that hour still receives
the replay. Past that bound, the forge's own retry is the recovery path.

**Verify:**

```bash
curl -i -X POST \
  -H "X-GitHub-Event: push" \
  -H "X-Hub-Signature-256: sha256=$(openssl dgst -sha256 -hmac "$SECRET" -hex body.json | cut -d' ' -f2)" \
  --data-binary @body.json \
  "https://<host>/hibernation/<token>/teams/payments/queue/github-webhook/"
```

Expect `202 Accepted`, and a build in the `payments` folder shortly after. A request signed with
the wrong secret, or carrying no signature header, answers `400`.

## Placement lifecycle

The placement pass binds one `Team` to one cell and records the binding in `Team.status.placement`
(`cluster`, `cell`, `cellNamespace`, `generation`). `generation` increases only when the bound cell
changes.

**Move.** An involuntary move happens when the bound cell reports `evacuate` or is gone. The pass
writes the new placement first, then both cells' tenant lists. A claim with an older generation
is refused, so a lost cell never grants a build after the move.

A voluntary move (a rebalance, or a profile change) waits. The reconciler drops the tenant's
folder from the old cell first (`Placed=False`, reason `TenantMoving`), then waits for the old
cell to confirm the folder is idle. It then performs the same two-step move.

**Roll.** When a cell reports `CellReady=False` for a reason other than the three configuration
gates, past `cells.unreadyGracePeriod`, the operator deletes its pod once and sets reason
`CellRolled`. The original fault reason moves into the condition message.

**Eviction.** Past twice the grace period, the operator sets `status.cell.evacuate`. Every tenant
then moves off that cell on its next placement tick. Recovery clears `evacuate`, `unreadySince`,
and `rolledAt` together.

## Troubleshoot placement

Every `Placed` reason on a `Team`:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `TenantPlaced` | True | The tenant is bound to a cell and ready. | None. |
| `TenantMoved` | True | An involuntary move landed on a new cell. | None. The old cell's schedule purge keeps retrying in the background. |
| `RecordStoreUnsupported` | False | `recordStore.enabled` is false. | Enable the record store. |
| `BlobTierUnsupported` | False | `recordStore.blobs.mode` is not `s3`. | Set the blob tier to `s3`. |
| `ProfileNotFound` | False | `spec.pipelines.profile` names no `JenkinsVersionProfile`. | Create the profile, or fix the name. |
| `AgentNamespaceUnmanaged` | False | `agentNamespace` is missing from `spec.namespaces`, or Varroa does not manage it. | Add it to `spec.namespaces` and confirm it is a managed namespace. |
| `AwaitingCell` | False | No cell satisfies the selection rule: enabled, ready, not evacuating, matching profile, and under `maxTenants`. | Create or enable a cell for that profile, or raise its `maxTenants`. |
| `TenantMoving` | False | A voluntary move is waiting for the old cell to finish draining the tenant's folder. | Wait. Check `status.pendingItemDeletions` for a stuck delete on the old cell. |
| `CellNotReady` | False | The pass keeps the tenant on its current cell, but that cell reports `CellReady=False` with no `evacuate` signal yet. | Diagnose the cell's own `CellReady` reason below. |
| `TenantTeardownPending` | False | Dropping `spec.pipelines` is tearing the tenant down, but the schedule purge or the old cell's drain has not confirmed yet. | Wait. The pass retries up to ten times, or up to a ten-minute bounded wait when the cell is unreachable. |

Every `CellReady` reason on a cell `Controller`:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `Connected` | True | The cell is provisioned and accepts tenants. | None. |
| `RecordStoreUnsupported` | False | `recordStore.enabled` is false. | Enable the record store. |
| `BlobTierUnsupported` | False | `recordStore.blobs.mode` is not `s3`. | Set the blob tier to `s3`. |
| `ArtifactStorageLocal` | False | The `ArtifactStorage` condition is not `External`. | Pin `artifact-manager-s3` and add the per-cell artifact `Secret`. |
| `NotProvisioned` | False | The cell's phase has not reached `Connected` yet. | Wait, or check pod status if it stays stuck. |
| `CellRolled` | False | The operator already deleted the pod once to recover. The condition message names the original fault. | Diagnose the reason in the message. If it recurs, that condition is not self-healing. |

## Rehydration

The operator seeds each job's build history onto a fresh cell pod after a move or a roll,
keeping `currentBuild.previousBuild` and the build-number sequence correct.

Each seed carries the job's next build number, its newest successful build, and up to
`rehydrateBuilds` further builds, newest first, with parameters and changesets. A multibranch
child seeds only after branch indexing creates it; the operator retries until then or until
`rehydrateDeadline` passes.

`Team.spec.pipelines` settings for rehydration and the ledger:

| Setting | Default | Range |
|---|---|---|
| `rehydrateBuilds` | `10` | `0` to `100` |
| `rehydrateDeadline` | `30m` | `1m` to `24h` |
| `interruptedRetry` | `manual` | `manual` or `automatic` |
| `concurrency` | `allow` | `allow` or `cancelInProgress` |
| `cronBackstop` | `true` | `true` or `false` |
| `cronCatchUp` | `one` | `one` or `none` |
| `cronGrace` | `5m` | `1m` to `60m` |

**Troubleshoot rehydration.** Every `Rehydrated` reason:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `TenantRehydrated` | True | Every selected seed reached the cell. | None. |
| `RehydrationPending` | False | A bundle is outstanding, or a job is deferred. | Wait; the operator resends each tick. |
| `RehydrationTruncated` | False | Seeds exceed one bundle; some jobs hold only a build number. | Wait; number-only bundles cover every job before full records resume. |
| `RehydrationFailed` | False | The mite rejected the bundle, or `rehydrateDeadline` passed. | Read the message. A give-up needs a fresh generation: move the tenant or roll the cell. |

A build can wait on the claim reason `NOT_READY` even when `Rehydrated` is `True`: its cell
instance may not match the announced one, or the job may lack a build-number seed. The claimer
retries on its own.

## Interrupted runs

The recorder marks a `RUNNING` run interrupted only when its cell process is gone, not because
its last event is old.

| Test | Condition | Reason |
|---|---|---|
| Moved | No heartbeat for 5 minutes, and the run's cell instance differs from the tenant's current one | `cell-rolled` or `tenant-moved` |
| Silent | No heartbeat for 15 minutes | `no-heartbeat` |

Both tests need a signal naming the run's cell instance gone; a slow cell marks nothing.
The moved test also covers a single-tenant controller, whose replaced pod is reported as a
dead instance.

Marking sets the run's result to `INTERRUPTED` and releases no fleet lock; only a terminal event
or a confirmed dead instance does. The recorder cancels the build only when its cell instance is
alive.

The recorder writes one `build.interrupted` activity row per marked run, visible to a live
subscriber only on its next backfill.

Set `interruptedRetry: automatic` to retry an interrupted run once; the default, `manual`,
leaves the retry to a user.

## Backstop scheduler

Jenkins computes every cron fire time inside each cell and exports each job's next three
occurrences. The recorder computes none: once a minute it checks those occurrences against its
ledger.

An occurrence becomes a candidate once older than `cronGrace` (default `5m`, range `1m` to
`60m`) with no matching ledger entry. The recorder triggers the build, keyed by the job path and
the occurrence's minute, so a late cell fire and a backstop fire never both run.

The exported horizon is three occurrences, so a long hibernation can exhaust it.
`cronCatchUp` decides the outcome: `one` (default) fires the newest miss and counts the rest;
`none` fires nothing and counts every miss. A fire wakes a hibernated controller, which then
exports a fresh horizon. A trigger for a controller that never wakes expires after 24 hours.

Each fire writes one `build.scheduled.backstop` row, visible only on the next backfill.

Set `cronBackstop: false` to turn the backstop off.
