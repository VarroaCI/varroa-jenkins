# Cells and Pipelines Tenants

A cell is a shared Jenkins controller that runs many teams' pipelines as tenants.

## What a cell is

A cell is a `Controller` with `spec.cell.enabled: true`. Its Jenkins home is an `emptyDir`, so a
pod roll loses local state and mite identity. [Rehydration](#rehydration) restores build
history, and the new instance [deletes the old agent pods](tenant-pod-templates.md#agent-pod-lifetime).
`spec.cell.enabled` cannot change after creation; patches fail with `cell.enabled is immutable`. A `cell` block without `enabled: true` is not a cell.
`spec.cell.maxTenants` can; it caps tenants.

A cell has no ingress host. Tenants use the
[tenant webhook door](webhooks.md).

Six limitations apply:

- A cell never auto-hibernates.
- `powerState: Stopped` on a cell destroys every in-flight build.
- Every branch source on a cell sees each webhook; only the job whose SCM matches reacts.
- A rehydrated run shows no stage view, culprit list, or test detail.
- The role-strategy screen shows a tenant's service identity as unknown until its first build.
- An ad-hoc job from the UI or script console does not survive a pod roll. Only a job in
  the tenant's [`items.yaml`](../config/items.md) bundle does.

## Prerequisites

A cell needs:

- The [build record store](build-records.md) enabled (`recordStore.enabled: true`).
- The `s3` blob tier (`recordStore.blobs.mode: s3`). A cell refuses the `postgres` tier.
- External artifact storage. Pin `artifact-manager-s3` in the cell's `JenkinsVersionProfile` and
  configure [Artifact credentials](artifact-credentials.md).

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

Apply it and wait for `Connected`.

**Verify:**

```bash
kubectl get controller cell-jenkins-lts -n varroa-system \
  -o jsonpath='{.status.conditions[?(@.type=="CellReady")]}'
```

Expect `"status":"True","reason":"Connected"`.

## Pools

A `CellPool` sizes its cells. `maxRunsPerTenant` caps a tenant's builds per cell. Zero means none. An auto-created pool
takes its template from `cells.cellTemplate`. See [Cell pools](cell-pools.md),
[Cell views](cell-views.md).

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
A `pipelines` block requires `profile` and `agentNamespace`.
`pipelines.profile` must name an existing `JenkinsVersionProfile`. An unset
`pipelines.composedBundleRef` places the tenant with an empty folder. An unset
`pipelines.cellPool` resolves to the profile's own pool. For a dedicated cell, name a
pool whose `maxTenantsPerCell` is 1.

**Verify:**

```bash
kubectl get team payments -o jsonpath='{.status.conditions[?(@.type=="Placed")]}'
```

Expect `"status":"True","reason":"TenantPlaced"`.

## What composes a tenant

- **Folder.** The tenant's `items.yaml` goes under one top-level folder named for the `Team`.
  The operator ignores `plugins.yaml`, `rbac.yaml`, and `jenkins.yaml` except its
  [pod templates](tenant-pod-templates.md). A job that the bundle drops leaves the
  folder; its next trigger fails with `JOB_NOT_FOUND`. See
  [Build records](build-records.md).
- **Held folder.** Tenant items resolve only their own variables. The operator
  keeps the folder and jobs on these faults:
  - an item still holds `${`, a `${varroa_*}` name too;
  - the items do not parse, hold an unknown key, set `root`, or fail a lint rule;
  - an item needs a plugin the cell does not pin;
  - a pipeline template does not load, or its body fails a lint rule. Only a failing template
    leaves the fragment; the tenant's other templates stay;
  - the bundle is missing or not built.

  The `Team` reports `TenantWarnings=True`, reason `ItemsHeld`. A first-time held tenant has no
  folder until fixed. Pipelines pages declare no job for a held folder; run history shows.
  The pages read the hold from the cell, so a change shows after the next cell reconcile.
- **Item role.** The operator synthesizes a role for the folder's subtree, granting build,
  read, and replay permissions to the service identity.
- **Service identity.** Builds in the folder run as `svc-<team>`, a synthesized identity
  with no user record.
- **Cloud and agent namespace.** The tenant gets one restricted Kubernetes cloud pointed at
  `spec.pipelines.agentNamespace`. A pipeline selects it with `agent { label '<team>-default' }` or `cloud: '<team>'`;
  `agent any` lands on the default cloud, which the agent fence refuses.
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

  `varroa.dev/credential-kind` selects one of these kinds:

  | Kind | Data keys | Notes |
  |---|---|---|
  | `usernamePassword` | `username`, `password` | |
  | `secretText` | `secret` | |
  | `sshPrivateKey` | `privateKey`, `username`, optional `passphrase` | |
  | `secretFile` | `file` | Annotation `varroa.dev/credential-filename`, default the Secret name |
  | `certificate` | `keystore` (binary PKCS#12), optional `password` | |
  | `githubApp` | `appID`, `privateKey` (PKCS#8 PEM), optional `owner`, `apiUri` | Convert with `openssl pkcs8 -topk8 -nocrypt` |

  Varroa skips a `Secret` with a missing key, an unknown kind, or a duplicate id, and records
  it in `TenantWarnings`. It also skips `secretFile`, `certificate`, or `githubApp`
  values over 64 KiB, and a keystore that is not binary PKCS#12. A value reaches Jenkins as written, `${`
  included. The controller diff drops comments and shows each credential as
  `<redacted>`, so a rotation shows no change. A GitHub deploy key:

  ```yaml
  apiVersion: v1
  kind: Secret
  metadata:
    name: payments-deploy-key
    namespace: team-payments
    labels:
      varroa.dev/tenant-credential: "true"
    annotations:
      varroa.dev/credential-id: github-deploy-key
      varroa.dev/credential-kind: sshPrivateKey
  stringData:
    username: git
    privateKey: |
      <OpenSSH private key>
  ```

  Example with `kubectl`:

  ```bash
  kubectl -n team-payments create secret generic payments-kubeconfig --from-file=file=./kubeconfig
  kubectl -n team-payments label secret payments-kubeconfig varroa.dev/tenant-credential=true
  kubectl -n team-payments annotate secret payments-kubeconfig \
    varroa.dev/credential-id=payments-kubeconfig varroa.dev/credential-kind=secretFile
  kubectl -n team-payments create secret generic payments-signing \
    --from-file=keystore=./signing.p12 --from-literal=password=<password>
  kubectl -n team-payments label secret payments-signing varroa.dev/tenant-credential=true
  kubectl -n team-payments annotate secret payments-signing \
    varroa.dev/credential-id=payments-signing varroa.dev/credential-kind=certificate
  ```

  A [runner](runners.md) credential uses the same data keys as `sshPrivateKey` in its own labelled `Secret`. A cell trusts a git host key on the first connection, because an agent
  pod has no `known_hosts` file. To pin host keys, set
  `security.gitHostKeyVerificationConfiguration.sshHostKeyVerificationStrategy` in the cell's own
  bundle.
- **Folder libraries.** A tenant folder can load shared libraries. See [Tenant shared libraries](tenant-libraries.md).

## Isolation controls

Five controls confine a tenant:

1. **Network policy.** `networkPolicy.cellIsolation.enabled` renders one `NetworkPolicy` per cell.
   See [Isolate cells](../install/network-policies.md#isolate-cells).
2. **Cloud restriction.** Each tenant's Kubernetes cloud serves its own folder only.
3. **Agent fence.** The plugin refuses a build on a node labelled for another tenant or on an
   unlabelled node.
4. **Build identity.** The plugin authenticates every build, including a Pipeline `node` block, as
   `svc-<team>`. That identity reads only its own folder's credentials and items.
5. **Script sandbox.** Tenant code is sandboxed. See [Script approvals](script-approvals.md).

## The tenant webhook door

The door paths, the forge rules, and the token fence are in [Tenant webhooks](webhooks.md).

## Placement lifecycle

The placement pass binds one `Team` to one cell, and records it in `Team.status.placement`
(`cluster`, `cell`, `cellNamespace`, `generation`). `generation` rises only when the bound cell
changes.

**Move.** An involuntary move happens when the bound cell reports `evacuate` or is gone. The pass
writes the new placement first, then both cells' tenant lists. A claim carrying an older
generation is refused, so a lost cell never grants a build after the move.

A voluntary move (a rebalance, or a profile change) waits. The reconciler drops the tenant's
folder from the old cell first (`Placed=False`, reason `TenantMoving`), waits until that cell
confirms the folder is idle, then does the same two-step move.

**Roll.** A cell that reports `CellReady=False` past `cells.unreadyGracePeriod`, for a reason
other than the three configuration gates, gets its pod deleted once and the reason `CellRolled`.
The roll waits up to 30 minutes from the pod start while an init container runs without a
restart. The grace counts from the init finish. A pod that has not started Jenkins gets a
30-second delete grace.

**Teardown** frees the placement record and the name at once.

**Eviction.** Past twice that grace period, the operator sets `status.cell.evacuate`, and every
tenant moves off on its next placement tick. The init wait applies here too. Only recovery clears
`evacuate`, `unreadySince`, and `rolledAt`.

## Troubleshoot placement

Every `Placed` reason on a `Team`:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `TenantPlaced` | True | The tenant is bound to a cell and ready. | None. |
| `TenantMoved` | True | An involuntary move landed on a new cell. | None. The old cell's schedule purge retries in the background. |
| `RecordStoreUnsupported` | False | `recordStore.enabled` is false. | Enable the record store. |
| `BlobTierUnsupported` | False | `recordStore.blobs.mode` is not `s3`. | Set the blob tier to `s3`. |
| `ProfileNotFound` | False | `spec.pipelines.profile` names no `JenkinsVersionProfile`. | Create the profile, or fix the name. |
| `CellPoolNotFound` | False | `spec.pipelines.cellPool` names no pool, or names one serving another profile. The message names the profile the pool serves. | Create the pool, or fix the name. |
| `AgentNamespaceUnmanaged` | False | `agentNamespace` is missing from `spec.namespaces`, or Varroa does not manage it. | Add it to `spec.namespaces` as a managed namespace. |
| `AwaitingCell` | False | No cell of the tenant's pool is enabled, ready, not evacuating, and under its resolved capacity. | Wait for the pool to create a cell, or raise its `warm` or `maxTenantsPerCell`. |
| `CellCapReached` | False | The tenant's pool sits at its `maxCells` and no cell is free. | Raise `maxCells`, or move the tenant to another pool. |
| `TenantMoving` | False | A voluntary move is waiting for the old cell to finish draining the tenant's folder. | Wait. Check `status.pendingItemDeletions` for a stuck delete on the old cell. |
| `CellNotReady` | False | The tenant stays on its cell, which reports `CellReady=False` without an `evacuate` signal. | Diagnose the cell's own `CellReady` reason below. |
| `AgentServiceFailed` | False | An agent Service write or delete failed. The placement is kept. | See [Agent Services](agents.md). |
| `TenantTeardownPending` | False | Teardown waits for the schedule purge or the old cell's drain to confirm. | Wait. The pass retries up to ten times, or waits ten minutes when the cell is unreachable. |

Every `CellReady` reason on a cell `Controller`:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `Connected` | True | The cell is provisioned and accepts tenants. | None. |
| `RecordStoreUnsupported` | False | `recordStore.enabled` is false. | Enable the record store. |
| `BlobTierUnsupported` | False | `recordStore.blobs.mode` is not `s3`. | Set the blob tier to `s3`. |
| `ArtifactStorageLocal` | False | The `ArtifactStorage` condition is not `External`. | Pin `artifact-manager-s3` and fix the credential that the condition names. |
| `NotProvisioned` | False | The cell's phase has not reached `Connected` yet. | Wait, or check pod status. |
| `MiteUnreachable` | False | The mite stream is lost. The roll applies. | Check the gateway. |
| `CellRolled` | False | The operator already deleted the pod once to recover. The condition message names the original fault. | Diagnose the reason in the message. If it recurs, that condition is not self-healing. |

## Rehydration

The operator seeds each job's build history onto a fresh cell pod after a move or a roll,
keeping `currentBuild.previousBuild` and the build-number sequence correct.

Each seed carries the job's next build number, its newest successful build, and up to
`rehydrateBuilds` further builds, newest first, with parameters and changesets. A multibranch
child seeds only after branch indexing creates it; the operator retries until then or until
`rehydrateDeadline` passes. The mite seeds a job that the same push creates after the items
apply, so a roll needs no second push. The mite drops the records of a job that the catalog deleted.

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
| `RehydrationPending` | False | A bundle is outstanding, or a job is deferred. Only deferred jobs and jobs with no number seed wait. Above 256 such jobs for a tenant, or past a cell-wide size limit, the largest tenants wait whole. | Wait; the operator resends after a result or a timeout. |
| `RehydrationTruncated` | False | Seeds exceed one bundle. Only jobs with no build number wait. Above 256 such jobs, the whole tenant waits. | Wait; number-only bundles cover every job before full records resume. |
| `RehydrationFailed` | False | The mite rejected the bundle, or `rehydrateDeadline` passed. Past the deadline, the deferred jobs build with no seeded history. | Read the message. A bundle that never applied needs a fresh generation: move the tenant or roll the cell. |

A build can still wait on the claim reason `NOT_READY` while `Rehydrated` is `True`; the
claimer retries. The operator applies a rehydration result and announces the seed within
seconds, so the first build after a cell roll waits about one claim retry.

## Interrupted runs

The recorder marks a `RUNNING` run interrupted only when its cell process is gone.
[Cell loss](cell-loss.md) describes the signals, the windows, and the reasons. The same marks
apply to a single-tenant controller whose pod is replaced.

Set `interruptedRetry: automatic` to retry an interrupted run once on the job's home slot; the
default, `manual`, leaves the retry to a user.

## Backstop scheduler

Each cell exports each job's next three cron occurrences, and the store keeps recent passed ones.
The recorder checks them against its ledger every minute. After a cell restart, a
[Jenkinsfile cron trigger](../config/items.md#schedule-a-job) is not exported until the job
builds again.

An occurrence becomes a candidate once older than `cronGrace` with no matching ledger entry. The recorder triggers the build, keyed by the job path and
the occurrence's minute, so a late cell fire and a backstop fire never both run.

`cronCatchUp` decides what several misses of one job fire: `one` (default) fires the newest and
counts the rest; `none` fires nothing and counts every miss. A long hibernation can exhaust the
three-occurrence horizon. A fire wakes a hibernated controller, which then exports a fresh
horizon. A trigger for a controller that never wakes expires after 24 hours. A cell roll loses no
occurrence; see [Cell loss](cell-loss.md#a-backstop-fire-during-a-cell-roll).
