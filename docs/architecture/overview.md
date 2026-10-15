# Architecture Overview

Varroa manages Jenkins controllers as Kubernetes resources. One namespaced `Controller` creates one Jenkins StatefulSet, Service, persistent volume, mite sidecar, and optional Ingress.

## Control Plane

```mermaid
flowchart LR
    U[User or API client] --> F[Dashboard]
    F --> B[BFF API]
    B <--> N[(NATS JetStream)]
    O[Operator] <--> N
    G[Gateway] <--> N
    O --> K[Kubernetes API]
    K --> P[Jenkins pod]
    P --> J[Jenkins]
    P --> M[mite]
    M <-->|gRPC mTLS| G
    M -->|localhost| J
```

| Component | Function |
|---|---|
| Operator | Provisions Jenkins resources. |
| Gateway | Terminates mite mTLS. |
| BFF | Serves APIs and activity. |
| Dashboard | Provides the browser client. |
| NATS JetStream | Carries messages and shared state. Components re-read their bus credential on every reconnect, so a rotated password needs no restart. |
| mite | Observes and configures Jenkins. |
| Dex | Brokers identity into OIDC. |
| Update center | Serves pinned plugins. |

Only the dashboard, BFF, and Jenkins routes need external HTTP access. The gateway remains a cluster-internal Service. The mite initiates its connection, so the control plane does not require inbound access to Jenkins.

## Build Data Plane

The Jenkins plugin, gated by the [build record store](../operations/build-records.md), sends
three kinds of traffic to the mite over a localhost listener: build events, build claims, and
console log chunks.

```mermaid
flowchart LR
    PL[Jenkins plugin] -->|localhost| M[mite]
    M <-->|gRPC mTLS| G[Gateway]
    G -->|publish| S[(varroa_builds stream)]
    G <-->|claim request/reply| L[Ledger]
    G -->|append| B[(Blob store)]
    S --> R[Recorder]
    R --> L
```

Build events travel one way, from the plugin through the mite to the gateway, which publishes
them onto the `varroa_builds` JetStream stream. The recorder consumes that stream and writes
runs, stages, and test summaries.

Build claims travel a round trip. A queued build asks the gateway, over the same mite stream,
whether it may run. The gateway forwards the request to the recorder's ledger and returns the
ledger's answer: a granted build number, a refusal, or a request to retry. See
[Build claims](../operations/build-claims.md) for the claim decisions, the timeout budget, and
how to turn the gate on.

Console log chunks travel one way, from the plugin through the mite to the gateway, which
appends each chunk straight to the blob store and acknowledges it once the write lands. This
path never touches the `varroa_builds` stream: a chunk that entered JetStream would evict real
audit history within days. Archived artifacts follow a separate path, straight from the
uploading process to the blob store under the writing cell's own object prefix. See
[Build logs and artifacts](../operations/build-logs-and-artifacts.md) for the full log path, the
live-tail mechanism, and the per-cell artifact prefix.

## Activity Path

Activity events publish onto NATS subjects `activity.<cluster>.<ns>.<ctrl>` and
`activity.<cluster>._global`. Events include lifecycle changes, user actions, and mite-observed
changes. Two consumers read that traffic independently. `BusFanout` subscribes it for live SSE.
The recorder's own JetStream stream, `varroa_activity`, holds a fixed 72-hour buffer that the
recorder drains into the Postgres `activity` table. `GET /activity` and MCP `list_activity` read
that table through `deps.Backfill`, whenever the
[build record store](../operations/build-records.md) is enabled. Without it, they answer empty,
and live SSE is all that remains.

Job schedules travel the build data plane instead: the mite polls the plugin's trigger inventory,
and the gateway republishes it on the `varroa_builds` stream into the `job_schedules` table. See
[Build records](../operations/build-records.md#job-schedules).

## Controller Configuration

Varroa resolves a controller from installation defaults, an optional `ControllerClass`, and the `Controller` spec. The controller spec has the highest precedence.

Jenkins configuration comes from a `ComposedBundle`. Its ordered inputs can reference catalog items, Git repositories, or OCI artifacts. An omitted `spec.composedBundleRef` selects the built-in `varroa-starter` bundle. A `JenkinsVersionProfile` supplies the compatible plugin set and any version-specific JCasC overlay. `JenkinsRole` and `JenkinsRoleBinding` resources define Jenkins authorization.

See [Composed bundles](../config/composed-bundles.md), [Jenkins versions](../config/jenkins-versions.md), and [Jenkins RBAC](../security/jenkins-rbac.md).

## Lifecycle

Read `status.phase` and `status.conditions` when operating a controller.

| Phase | Meaning |
|---|---|
| `Pending` | Waiting for reconciliation. |
| `Provisioning` | Resolving configuration or creating Kubernetes resources. |
| `Running` | Jenkins is running, but the mite stream is not ready or is lost. |
| `Connected` | The mite stream is active. This is the normal steady state. |
| `Stopped` | `spec.powerState: Stopped` scaled the StatefulSet to zero. |
| `Hibernated` | Inactivity policy parked the controller. |
| `Failed` | A blocking provisioning or operation error occurred. |

A phase says where the controller is in its lifecycle. It does not say whether the controller is stuck there. Varroa reports "needs attention" separately, on the dashboard, the controllers list, and the detail page. It is derived from these status signals, in precedence order.

| Signal | Shown as | Meaning |
|---|---|---|
| `status.phase: Failed` | Failed | A blocking provisioning or operation error. See the `Failed` condition. |
| `ReconcileBlocked` condition | Blocked | The operator cannot proceed. The bundle is unreadable, or a plugin pin conflicts with the version-profile lock (a cell ignores a pin on `job-dsl`). The message names the fix. |
| `JenkinsBootFailed` condition | Boot failed | The Jenkins container is crash-looping, or its image cannot be pulled. The message carries the exit code and restart count. Read the container logs. |
| `PluginRollFailed` condition | Plugin roll failed | The `plugins-init` step failed. |
| `status.lastApplyResult.succeeded: false` | Apply failed | The mite could not apply the last desired state. |
| `MiteConnected=False`, reason `StreamLost` | Mite unreachable | The mite stream is lost. The pod stays in place. |

`status.lastReconciledAt` advances on every reconcile pass that completes without error, in every phase. Read the attention signal first, not this timestamp. Most wedged controllers still reconcile cleanly and so keep a fresh stamp: a Boot failed, Plugin roll failed, or Apply failed controller records the problem in its status and the pass still succeeds. The stamp goes stale only for a Blocked controller, whose pass ends in an error, and during a `Failed` phase dwell. Use `lastReconciledAt` for one distinction: a stale value with no attention signal means the operator is not reconciling this controller at all, rather than reconciling it and being blocked.

A lost mite stream keeps the pod and the phase `Running`. The operator rolls the pod only when the gateway CA or endpoint changed. A cell also rolls after its unready grace period.

Reloadable configuration can apply without replacing the pod. Changes to plugins, images, or other restart-class settings can return the controller to `Provisioning` while Varroa rolls it.

```bash
kubectl get controller <name> -n <namespace>
kubectl describe controller <name> -n <namespace>
```

Use [Troubleshooting](../operations/troubleshooting.md) when a controller does not reach `Connected`.

## Core Resources

All resources use `varroa.dev/v1alpha1`.

| Resource | Scope | Purpose |
|---|---|---|
| `Controller` | Namespaced | Declares one Jenkins controller. |
| `ComposedBundle`, `CatalogSource`, `CatalogItem` | Namespaced | Supply Jenkins configuration. |
| `PodTemplate` | Namespaced | Defines reusable Kubernetes agents. |
| `ProvisioningDefaults`, `ControllerClass` | Cluster | Supply reusable controller defaults. |
| `JenkinsVersionProfile` | Cluster | Pins Jenkins and plugin compatibility. |
| `VarroaRole`, `VarroaRoleBinding` | Cluster | Authorize dashboard and API actions. |
| `JenkinsRole`, `JenkinsRoleBinding` | Cluster | Authorize actions inside Jenkins. |
| `UpdateCenter` | Cluster | Declares the optional plugin service. |

Continue with [The mite](mite.md), [Scaling](scaling.md), or [Your first controller](../tutorials/first-controller.md).
