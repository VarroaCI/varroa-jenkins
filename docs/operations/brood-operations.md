# Brood Operations

<!-- Sources: internal/controller/broodoperation_controller.go, internal/api/handlers_broodops.go -->

Orchestrated bulk controller operations: run a single action (restart, reprovision, reconcile, stop, start) across multiple controllers with wave ordering, concurrency caps, and failure policies. Each run is one `BroodOperation` CR that is garbage-collected after its TTL.

## Concepts

A `BroodOperation` represents exactly one execution. Its spec is **immutable** after creation (except `suspend` and `ttlSecondsAfterFinished`) — you cannot change the verb, targets, or execution parameters once the run starts. The run progresses through these phases:

```
Pending → Running → (Suspended) → Succeeded | Failed | Canceled
```

- **Pending** — tenancy validated, targets resolved and snapshotted to status.
- **Running** — targets dispatched wave-by-wave, subject to maxParallel and failure policy.
- **Suspended** — `spec.suspend=true` paused dispatch; in-flight targets still tracked.
- **Succeeded** — every target reached a terminal state and none failed.
- **Failed** — at least one target failed (per the failure policy's definition).
- **Canceled** — the CR was deleted; finalizer tidied remaining targets.

Finished runs are garbage-collected `ttlSecondsAfterFinished` seconds (default 7 days) after `finishedAt`. History survives in the [activity feed](observability.md).

## Verbs

| Verb | Applicability | Completion predicate | Timeout |
|---|---|---|---|
| `restart` | phase == Connected | pod recreated after dispatch AND Ready; old pod still standing after 3m → Failed "restart not observed" | 15m |
| `reprovision` | phase != Stopped | phase leaves {Connected,Running} within 3m (else Failed "not observed"), then returns to Connected | 30m |
| `reconcile` | always | `status.lastReconciledAt > dispatchedAt` | 5m |
| `stop` | phase != Stopped | `status.phase == Stopped` | 10m |
| `start` | phase == Stopped | `status.phase == Connected` | 20m |
| `executeGroovy` | phase == Connected | goroutine result (see below) | 60s (call) / 5m (crash backstop) |

### Restart semantics

A **brood restart** deletes the target's Jenkins pod — the same mechanism as the single-controller restart approval. The StatefulSet recreates the pod, and the mite-owned SIGTERM drain quiets Jenkins down within the pod's termination grace period, so running builds get a bounded chance to finish. There is no mite-side "safe restart": Jenkins' `/safeRestart` requires ADMINISTER, which the mite deliberately does not hold (see the mite minimal-permissions model). The completion predicate is pod evidence: the pod's `creationTimestamp` must be newer than the dispatch and the pod must be **Ready**. Ready is now backed by the shipped Jenkins readiness probe, so the predicate means "Jenkins is actually serving" rather than just "the containers are up".

### ExecuteGroovy semantics

`executeGroovy` runs an arbitrary Groovy script against each target controller's `/scriptText` (the Jenkins script-console endpoint). It is the **only verb that bypasses the mite** — the operator dispatches directly to the target's Jenkins via HTTP, minting a short-lived `system:varroa-operator` JWT (`GenerateOperatorJenkinsToken` in `internal/mite/jenkinstoken.go`) that carries the `ROLE:varroa:system-operator` authority.

#### Run a Groovy script from the dashboard

1. Open the **Run operation…** wizard (Controllers page or Brood Operations page).
2. Select one or more controllers and choose verb **`executeGroovy`**.
3. Choose the script source:
   - **Inline script** — type Groovy code directly into the text area. The script text is sent verbatim in the operation spec.
   - **Catalog item** — select from available `groovy`-typed catalog items. Only items that are `valid` and of type `groovy` are shown. Catalog-item mode requires a **single target cluster**; if your selection spans multiple clusters the picker is disabled with a hint message.
4. If the selected catalog item declares **variables**, an inline form appears. Fill required variables (marked with `*`); optional variables with a declared default are filled automatically. Variable values are submitted as `itemRef.variables` and interpolated into the script via `${var}` substitution.
5. **Preview** checks grammar and target applicability (same as other verbs).
6. **Create & Run** dispatches the operation. On success, per-target output is shown as a collapsible **Output** section on the operation detail page.

Requires `controller-manage` permission in a team namespace; operator-namespace runs (including cross-namespace selections) require admin. **This verb runs arbitrary code on every targeted Jenkins — use with care.**

It can also be switched off or restricted to named namespaces with `ProvisioningDefaults.broodPolicy.executeGroovy`, and every target emits an audit event naming who ran what. Both are covered in [executeGroovy security](../security/execute-groovy.md), which is the page to hand a security reviewer.

#### Script source (exactly-one-of)

| Source | Field | Description |
|---|---|---|
| Inline | `action.groovy.script` | Literal Groovy code in the BroodOperation spec |
| Catalog item | `action.groovy.itemRef` | Reference to a `groovy`-typed [CatalogItem](../config/casc-catalog.md). `itemRef.variables` are interpolated via `${var}` **once** before snapshotting |

Inline and catalog-item are mutually exclusive — exactly one must be set.

#### Catalog-sourced script snapshot

When the script comes from a catalog item, the resolved (variable-substituted) text is materialized into an **owner-referenced ConfigMap** in the operation's namespace on first use. Every subsequent target and reconcile reuses that same byte-identical snapshot — the item is resolved only once per operation. If the operator crashes after creating the ConfigMap but before persisting `status.scriptSnapshotRef`, the snapshot is re-read from the ConfigMap by name on the next reconcile (idempotent recovery).

`groovy`-type catalog items are **execution-only** and rejected as `ComposedBundle` inputs (non-fatal per-input error; the rest of the bundle still composes).

#### Dispatch

1. **Resolution**: inline script is used verbatim; item-ref script goes through the local-first/operator-fallback lookup (identical to ComposedBundle cross-namespace resolution), then type-check, optional [pinned-content-hash](../config/casc-catalog.md#how-to-consume-an-item) verification, variable interpolation, and ConfigMap snapshotting.
2. **Async goroutine**: once resolution succeeds the target is marked `Dispatched` and a goroutine is fired (bound to a **60-second context deadline**). Inside it, `runGroovyOnTarget` mints a `system:varroa-operator` JWT (2-minute TTL, aud = `"<namespace>/<name>"` of the target controller) and runs `jenkins.Client.ScriptConsoleOnce` against `http://<controller-prefix>-svc.<ns>.svc.cluster.local:8080/scriptText`. A failure *inside* the goroutine — mint failure (no operator key), a transport error (connection refused/timeout), or a non-2xx response — is captured as the goroutine's `(_, error)` result and surfaces as a `Failed` target on the next poll tick, **not** via the verb timeout. Only a *resolution* failure (step 1) or the operator process dying before any result is written falls through to the timeout.
3. **Timeout backstop**: the outer 5-minute verb timeout (`broodVerbTimeouts[executeGroovy]`, distinct from the 60-second `groovyCallTimeout` per-call deadline) is a crash backstop — if the operator process dies and restarts, a goroutine from before the restart never writes a result, and the target times out 5 minutes after dispatch.
4. **Result capture**: the goroutine writes a `(output, error)` pair into an in-memory map keyed by `(op, target)`. The next reconcile tick's `evaluateDispatchedTarget` peeks at this map — on success it marks the target `Succeeded` and persists `BroodTargetStatus.Output` (truncated to 4096 bytes on a UTF-8-safe boundary) and `Reason` (256-byte truncation for errors). On failure the target is marked `Failed`. The map entry is deleted only after the status patch succeeds (peek/ack contract), so a failed patch retries on the next tick.

#### Output and error fields

| Field | Cap | Behavior |
|---|---|---|
| `BroodTargetStatus.Output` | 4096 bytes UTF-8-safe | Script stdout/stderr on success; partial output may survive on error |
| `BroodTargetStatus.Reason` | 256 bytes UTF-8-safe | Error message on failure; empty on success |

#### At-most-once observation (not at-most-once effect)

`executeGroovy` provides **at-most-once observation** of the script's result, NOT at-most-once execution. If the operator process crashes or network breaks between the script succeeding in Jenkins and the result being captured into the in-memory map, the next reconcile sees the target still `Dispatched`, lets it time out (5 minutes from dispatch), and reports **`Failed` with a timeout reason**. The script itself **did** run in Jenkins — the timeout is a _result-observation_ failure, not an execution failure. This is identical to how every other brood verb handles operator crashes between send and observe.

#### Unchanged mechanics

- **Wave ordering**, **maxParallel**, and **failure policies** behave identically for `executeGroovy` as for every other verb.
- **Authorization** reuses the standard BroodOperation path (verb-agnostic [`broodOpAccess`](../security/jenkins-rbac.md) — shared API-layer gate; direct `kubectl apply` CR creation is gated only by Kubernetes RBAC on `broodoperations.varroa.dev`).
- This verb is **YAML/CLI-authoring only** (no UI composer support in this change).
- The operator's NetworkPolicy must allow egress TCP/8080 to target namespaces — see [network policies](../install/network-policies.md) for the bimodal `managedNamespaces` configuration.

## Filters

Target filters are ANDed and **silently shrink** the pool — non-matching controllers do not appear in `status.targets` at all.

| Filter | Matches on |
|---|---|
| `phase` | `status.phase` equality |
| `version` | `spec.version` exact match |
| `bundle` | `spec.composedBundleRef.name` exact match |

## Failure policies

| Policy | Behavior |
|---|---|
| `FailFast` | First failed target → all Pending become Skipped("canceled"), all in-flight Dispatched become Failed("abandoned (FailFast)"), run terminates Failed immediately |
| `FailTidy` (default) | First failed target → no further dispatch (Pending become Skipped("canceled") once in-flight drain completes); in-flight tracked to terminal; then Failed. A wave-N failure never opens wave N+1 |
| `FailAtEnd` | Failures gate nothing — every target dispatches in order. Run terminates Failed at the end if any target failed |

## Targeting modes

Exactly one of `targets.names` or `targets.selector` is required. Which form you use depends on the CR's **namespace** (tenancy mode):

### Team namespace (bare names)

The BroodOperation lives in a team namespace (e.g. `teams-payments`). Names are bare controller names in that same namespace.

```yaml
spec:
  action:
    verb: reconcile
  targets:
    names: [ctrl-a, ctrl-b]
```

`namespaces` is forbidden; qualified names (`ns/name`) are rejected.

### Operator namespace — selector mode

The BroodOperation lives in the operator namespace. A label selector targets controllers cluster-wide; `namespaces` is required — either an explicit list or `["all"]`.

```yaml
spec:
  action:
    verb: restart
  targets:
    selector:
      matchLabels:
        tier: canary
    namespaces: [teams-payments, teams-web]
```

`["all"]` scans every namespace that has at least one Controller CR at resolution time (not all Kubernetes Namespace objects).

### Operator namespace — names mode

Entries are `ns/name` qualified. Use this to target specific controllers across namespaces.

```yaml
spec:
  action:
    verb: stop
  targets:
    names: [teams-payments/api, teams-web/web]
```

## Execution parameters

| Parameter | Default | Description |
|---|---|---|
| `maxParallel` | 1 | Maximum targets dispatched concurrently |
| `order` | `rolloutWave` | Dispatch order: `rolloutWave` (wave-based gating) or `name` (flat, all one wave) |
| `failurePolicy` | `FailTidy` | Behavior when a target fails |

## Wave interaction

With `order: rolloutWave` (default), targets are sorted by `(rolloutWave, namespace, name)` and a target in wave N+1 is **never dispatched** while any wave-N target is still Pending or Dispatched. `Skipped` targets never hold the gate. This is the same wave concept used by [rollout waves](rollout-waves.md) for bundle changes.

With `order: name`, all targets are treated as one wave and dispatch proceeds in namespace+name order within the `maxParallel` limit.

## How-to: run, preview, cancel, suspend

### Via the UI

1. Go to the **Controllers** page or the **Brood Operations** page, then click **Run operation…**.
2. Select one or more controllers using the checkboxes.
3. Choose the verb and execution parameters, then click **Preview** to see which controllers will be targeted and their applicability.
4. Click **Create & Run**. From the **Controllers** page, you're taken to the run's live detail view. From the **Brood Operations** page, the wizard closes and the run list refreshes in place — find your new run in the list to open its detail view.
5. On the **Brood Operations** page and the detail view, use **Suspend** / **Resume** and **Cancel** to control the run.

### Via varroactl

```bash
# Preview without creating
varroactl broodop run restart -l tier=canary --filter phase=Connected --dry-run

# Create and watch
varroactl broodop run stop --names api,web -n teams-payments -w

# List runs
varroactl broodop get

# Describe a run (includes per-target table)
varroactl broodop describe teams-payments/broodop-restart-abc12

# Suspend / resume
varroactl broodop suspend teams-payments/broodop-restart-abc12
varroactl broodop suspend teams-payments/broodop-restart-abc12 --off

# Cancel
varroactl broodop cancel teams-payments/broodop-restart-abc12

# Watch a run (live per-target table, exits 0 on Succeeded, 1 on Failed/Canceled)
varroactl broodop watch teams-payments/broodop-restart-abc12
```

#### Watch stream deadline

The `GET /brood-operations/{ns}/{name}/stream` SSE endpoint backing `broodop watch`/`broodop run --watch` closes as soon as the run reaches a terminal phase (Succeeded/Failed/Canceled), regardless of whether every target cluster is currently reachable. If the run never reaches a terminal phase — e.g. its target set stays unreachable the whole time — the server closes the stream after a **1 hour** bound, emitting a final `closed` event with a `deadline_exceeded` reason/message; `varroactl broodop watch` surfaces that message and exits non-zero instead of hanging.

## Limitations

### Cross-cluster runs

The core BFF can initiate brood operations that span multiple clusters, creating an identically-named `BroodOperation` child CR in each target cluster. Each child is resident in its own cluster and executed by that cluster's leader-elected operator — the executor is unchanged. The core aggregates the children into a single **logical run** view.

#### Targeting

The `clusters` field on the create/preview request controls which clusters participate:

- **Absent or `[]`**: defaults to the local (core) cluster only.
- **Explicit list**: `["core", "dev-cluster"]` targets those clusters.
- **`["all"]`**: expands to every known member cluster (local + all heartbeat entries).

The `clusters` dimension composes with the existing three tenancy modes:

| Mode | `clusters` | `targets.names` shape | Fan-out |
|---|---|---|---|
| Team namespace (req `namespace` ≠ operator ns) | ≤1 entry (default local) | bare names (unchanged) | one child in that cluster |
| Operator-ns selector | 1..n explicit, or `["all"]` (default local) | — (selector + optional `namespaces` incl. `all` + filters) | identical spec to each cluster; `namespaces:[all]` resolves against each cluster's own namespace universe |
| Operator-ns names, single-cluster | ≤1 entry (default local) | `ns/name` (unchanged) | one child |
| Operator-ns names, multi-cluster | **forbidden** (derived from qualified names) | every entry 3-token `cluster/ns/name` | BFF partitions per cluster; each child receives plain `ns/name` entries |

All eight rejection rules (400 on violation):

1. Mixing cluster-qualified (3-token) names with unqualified names.
2. Mixing bare names with `ns/name` qualified names.
3. 3-token names in team namespace mode.
4. Explicit `clusters` field alongside cluster-qualified names.
5. Multi-entry `clusters` with unqualified names (single-cluster only).
6. `["all"]` combined with any explicit names.
7. Team namespace mode with more than one cluster.
8. Unknown cluster name anywhere.

Normalization: `clusters: []` behaves as absent (defaults to `[local]`); duplicate cluster entries are silently deduplicated.

#### Logical-run model

A run's identity is `{namespace}/{name}` — the BFF mints a single name (`broodop-<verb>-<5rand>`) shared by every child. Children are grouped by `(namespace, name)` at read time.

**Aggregated phase** (computed over reachable children only; an unreachable cluster contributes no phase):

| # | Condition | Run phase |
|---|---|---|
| 1 | all Succeeded | Succeeded |
| 2 | all terminal ∧ ≥1 Failed | Failed |
| 3 | all terminal (Succeeded/Canceled mix) | Canceled |
| 4 | every non-terminal child Suspended (≥1 Suspended) | Suspended |
| 5 | all Pending | Pending |
| 6 | otherwise | Running |

When zero children are reachable, no phase is computed — readers surface the unreachability instead.

**Summary**: field-wise sum of child `summary` values (`total`, `succeeded`, `failed`, `skipped`).

**startedBy / createdAt**: from the first reachable child (identical across children by construction).

#### Partial-failure semantics (stateless BFF)

Creates fan out concurrently with a per-cluster 10s timeout. Response:

- **201** when ≥1 cluster succeeded — body is a `BroodRun` with per-cluster sections and aggregations.
- **502** when zero clusters succeeded and any failure was transport-level (timeout / no responder).
- Otherwise the mapped HTTP code of the first requested cluster's error (`invalid`→400, `conflict`→409, `not_found`→404, `internal`→500). Body is `{"error":"...","clusters":[BroodRunClusterStatus]}`.

The BFF keeps no record of intent: a run is exactly its existing children. No rollback of partially created runs (the caller may cancel the run).

Cancel and suspend resolve the run's member clusters at request time (via the read path), then fan out. Response is `200` with per-cluster `{cluster, ok, error?}` rows; `404` when no known cluster holds the run.

#### Per-cluster activity events

Each child's executor publishes its own `broodop.run.started` / `broodop.run.finished` / `broodop.target.finished` events, cluster-stamped. An N-cluster run produces N `run.started` events in the global feed.

#### Assumptions

- **Uniform operator namespace**: the operator namespace is assumed identical across clusters. Divergent hive installs degrade to per-cluster operator-side rejection, never silent misbehavior.
- **The BroodOperation CRD is installed in every mode** (full and hive) — no chart change is needed for this feature.
- **NATS ACLs** already cover `operator.>` — no ACL change is needed.

#### #269 — Restart completion predicates

Restart completion now tracks serving readiness because Jenkins pods ship a readiness probe. This applies identically in every cluster and is not changed by this feature.

---

### Legacy (single-cluster) notes

- **No cross-run mutex:** Two brood operations may target the same controller. For powerState toggles (stop/start) this is last-write-wins; a concurrent double restart is one extra pod bounce; reconcile/reprovision triggers coalesce in the controller reconciler.
- **Timeouts are fixed, not user-tunable.** See the verb table above.
- **Wave gating is derived from status at resolution time** (the `rolloutWave` value of each controller's `spec.reconciliationPolicy.rolloutWave`). If you change a controller's wave after the run starts, it has no effect on the in-flight run.

## Controller labeling for selector targeting

To use the label-selector targeting mode, label your controllers:

```bash
kubectl label controller ctrl-a -n teams-payments tier=canary
kubectl label controller ctrl-b -n teams-web tier=prod
```

Then target by selector:

```bash
varroactl broodop run restart -l tier=canary
```

Labels follow Kubernetes conventions: each controller can have multiple labels, and you can combine selectors to narrow the set.

---

**See also:** [Brood Schedules](brood-schedules.md) — scheduled, recurring brood operations with cron expressions.
