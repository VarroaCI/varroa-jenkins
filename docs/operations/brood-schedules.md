# Brood Schedules

<!-- Sources: internal/controller/broodschedule_controller.go -->

Scheduled, recurring brood operations: define a cron expression, a template (targets, action verb, execution parameters), and the operator owns a standard Kubernetes `batch/v1 CronJob` that fires `varroactl broodop run <verb>` on every tick. Each fired run is an ordinary `BroodOperation` with `status.startedBy: "schedule:<ns>/<name>"`, visible in the dashboard's run-history panel.

## Concepts

A `BroodSchedule` is a **namespaced** CRD that owns:

- A **`batch/v1 CronJob`** in the same namespace. The cron expression, concurrency policy, deadline, and history limits are passed through verbatim from the schedule's spec.
- A **`Secret`** `<name>-schedule-token` holding a short-lived RS256 JWT signed by the shared mite signing key. The CronJob's pod reads `VARROACTL_API_KEY` from this Secret via `secretKeyRef`.
- A **cluster-scoped `VarroaRoleBinding`** (deterministically named) granting the synthetic identity `schedule:<ns>/<name>` the appropriate built-in role.

The fired Job is bounded to one hour with `activeDeadlineSeconds: 3600` and
cleaned up one hour after completion with
`ttlSecondsAfterFinished: 3600`. These Kubernetes Job bounds are independent
of the BroodOperation CR's retention policy.

The reconciler runs inside the leader-elected operator with a periodic recheck every 15 minutes (independent of the cron expression). Edits to `spec` — schedule change, template change, `waitForCompletion` toggle — trigger an immediate reconcile.

### State machine

```
Normal reconcile loop:
  1. Tenancy check        → sets status.reason on violation
  2. Token mint/rotate     → ensures valid JWT in Secret
  3. VarroaRoleBinding     → create or update
  4. Ordering confirmation → uncached Get confirms binding + Secret exist
  5. CronJob create/update → sets suspend = spec.suspend || tenancyViolated
  6. Status copy           → lastScheduleTime/lastSuccessfulTime/active from CronJob
```

On **deletion** the finalizer (`varroa.dev/broodschedule-rbac`) deletes the cluster-scoped `VarroaRoleBinding` before the finalizer is removed. The owned `CronJob` and `Secret` are cleaned up by Kubernetes GC via `OwnerReference`.

## CRD reference

### BroodScheduleSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `schedule` | `string` | — | Cron expression (required). |
| `suspend` | `bool` | `false` | Pauses the cron schedule. |
| `concurrencyPolicy` | `ConcurrencyPolicy` | `Forbid` | CronJob concurrency: `Allow`, `Forbid`, `Replace`. See caveat below. |
| `startingDeadlineSeconds` | `*int64` | nil | Deadline (seconds) for starting a missed job. |
| `successfulJobsHistoryLimit` | `*int32` | `3` | Number of successful finished jobs to retain. |
| `failedJobsHistoryLimit` | `*int32` | `1` | Number of failed finished jobs to retain. |
| `waitForCompletion` | `bool` | `true` | Whether the Job runs `--watch` and waits for the run outcome. |
| `template` | `BroodScheduleTemplate` | — | The brood operation template (targets, action, clusters, execution, TTL). |

### BroodScheduleTemplate

| Field | Type | Description |
|---|---|---|
| `targets` | `BroodTargets` | Target selection: names or selector (same as `BroodOperation`). |
| `action` | `BroodAction` | The verb (`restart`, `reprovision`, `reconcile`, `stop`, `start`). |
| `clusters` | `[]string` | Target clusters. At most one entry in team-namespace mode. |
| `execution` | `*BroodExecution` | Optional execution parameters (order, maxParallel, failurePolicy). |
| `ttlSecondsAfterFinished` | `*int32` | TTL after the operation finishes (maps to `--ttl` flag). |

When this field is omitted, the schedule passes `--ttl=86400` to each run
(one day). Set it to `0` to keep completed BroodOperation CRs indefinitely, or
set another positive value for a custom TTL. The equivalent CLI override is
`varroactl broodop run ... --ttl=<seconds>`; an explicit `--ttl=0` also means
keep forever.

## Target skips and watch completion

For `reconcile`, targets already **Hibernated** or **Stopped** are recorded as
`Skipped` with reason `target hibernated` or `target stopped`. If a target
enters either phase after dispatch, it is recorded as `Skipped` with reason
`target hibernated during operation` or `target stopped during operation`.
Reconcile evidence observed at the same time wins, so a completed reconcile is
`Succeeded` even if the controller has just transitioned to Hibernated or
Stopped. An operation whose targets are all skipped still succeeds; any failed
target makes the operation fail.

When `waitForCompletion: true`, `varroactl broodop run --watch` stops reading
the SSE stream as soon as it receives `Succeeded`, `Failed`, or `Canceled`.
`Suspended` is not terminal and remains watched until a terminal phase.

### BroodScheduleStatus

| Field | Type | Description |
|---|---|---|
| `lastScheduleTime` | `*metav1.Time` | Last time the CronJob was scheduled (copied verbatim from the CronJob). |
| `lastSuccessfulTime` | `*metav1.Time` | Last time a Job completed successfully. **Meaning depends on `waitForCompletion`** — see below. |
| `active` | `[]corev1.ObjectReference` | Active Jobs (copied verbatim from the CronJob). |
| `reason` | `string` | Non-empty when the schedule is in a problematic state (e.g. `TenancyViolation`). |

## Tenancy modes

BroodSchedule inherits the same targeting tenancy model as [BroodOperation](brood-operations.md#targeting-modes). The tenancy mode is determined by the schedule's own `metadata.namespace`:

| Mode | `BroodSchedule` namespace | `RoleRef` | `Scope` |
|---|---|---|---|
| **Team-namespace** | A team namespace (e.g. `teams-payments`) | `"operator"` | `{Namespaces: [<team-ns>]}` |
| **Operator-namespace, selector or names** | The operator namespace | `"admin"` | `nil` (unrestricted) |

Schedule-specific rules:

- **At most one entry in `template.clusters`** in team-namespace mode. Multi-cluster targeting requires the operator namespace.
- **`template.targets.namespaces` is forbidden** in team-namespace mode (same rule as BroodOperation).

Tenancy is enforced **in Go** at two layers, not via CRD CEL (CEL cannot read `metadata.namespace`):

1. **BFF create-time** (`POST /api/v1/brood-schedules`): plain 400 error, matching every other BFF validation failure.
2. **Reconcile-time, authoritative** (`broodschedule_controller.go`): on violation the reconciler sets `status.reason = "TenancyViolation"`. If an owned CronJob already exists (the edit-into-violation case), the reconciler sets the CronJob's `spec.suspend = true` until the template is fixed. If no CronJob exists yet, creation is withheld.

Each schedule reuses the existing built-in roles (`"operator"` / `"admin"`) — no new role is introduced.

## Synthetic identity + RBAC

Every schedule gets a unique synthetic identity `schedule:<namespace>/<name>`:

- A **JWT** is minted at first reconcile and stored in an owned Secret `<name>-schedule-token`.
- A **`VarroaRoleBinding`** (cluster-scoped, deterministically named) maps this identity to the role matching the schedule's tenancy mode (see table above).
- The binding is named `broodschedule-<sha256(ns/name)>` (truncated to 62 chars for DNS-1123 compliance) and carries `varroa.dev/broodschedule-namespace` / `varroa.dev/broodschedule-name` annotations for human/tooling reverse-lookup.
- On schedule deletion, the finalizer removes the binding; the token Secret is cleaned up by `OwnerReference` GC.

The `VarroaRoleBinding` is cluster-scoped and can therefore be found by the RBAC resolver across every namespace — even in team-namespace mode, where its `Scope` restricts it to the team namespace.

## Image pull secrets

The fired Job runs the same `varroactl` image as the operator, referenced by the
chart-injected `VARROA_VARROACTL_IMAGE`. When that image is pulled from a private
registry, the reconciler stamps the pod's `imagePullSecrets` from the operator's
`VARROA_IMAGE_PULL_SECRETS` env var (populated by the chart from
`global.imagePullSecrets`). Because a Job pod can only reference pull secrets that
live in **its own namespace**, each named pull secret must exist in every namespace
that hosts a `BroodSchedule` — in team-namespace mode that is the team namespace, not
the operator namespace. Team namespaces provisioned by Varroa already receive the
registry secret alongside the controller's; hand-created team namespaces need it
mirrored in (e.g. `kubectl get secret <pull-secret> -n <operator-ns> -o yaml | …`).

The reconciler resolves the BFF endpoint the fired Job calls from the chart-injected
`VARROA_SCHEDULE_BFF_URL` (the release-prefixed in-cluster Service), so a non-default
Helm release name is handled automatically.

## ConcurrencyPolicy caveat

`concurrencyPolicy` governs **trigger-Job overlap** in the native CronJob controller. What that *means* for brood runs depends on `waitForCompletion`:

- **`waitForCompletion: false`** (fire-and-forget): the trigger Job calls `varroactl broodop run` and exits as soon as the BFF accepts the create request. The underlying brood operation runs asynchronously and may overlap with the next scheduled fire — `concurrencyPolicy` only prevents a **second trigger Job** from starting while the first is still running (which is nearly instantaneous for fire-and-forget). It does **not** prevent concurrent brood runs.
- **`waitForCompletion: true`**: the trigger Job runs `varroactl broodop run --watch`, blocking until the run completes (or fails). Here `concurrencyPolicy` genuinely prevents overlapping brood runs, as the Job lives for the entire run duration.

## JWT TTL and rotation

| Property | Value |
|---|---|
| TTL | 1 hour |
| Rotation threshold | Re-minted when less than 30 minutes remain |
| Rotation mechanism | Reconcile-driven best-effort (every 15-minute recheck examines the token's `exp`) |
| Secret type | `Opaque`, key `token`, consumed by Job container via `secretKeyRef` |

The rotation pattern mirrors the mite-cert proactive-renewal precedent. Two rotation attempts land inside the 30-minute window before expiry (at the 15- and 30-minute reconcile ticks), so a single missed reconcile is harmless. An operator outage spanning the full 30-minute window followed by a Job firing immediately after could still see an already-expired token — this is a narrow accepted edge case consistent with the existing mite-cert renewal pattern.

## `waitForCompletion` and `lastSuccessfulTime`

The `status.lastSuccessfulTime` field is copied verbatim from the owned CronJob's status regardless of mode. Its *meaning* depends on the schedule's `waitForCompletion` setting:

- **`waitForCompletion: true`**: the Job runs `varroactl broodop run --watch`, which blocks on the SSE stream and exits non-zero on `Failed`/`Canceled`. `lastSuccessfulTime` means "the watch observed no failure." In the rare case of a premature disconnect (network blip, BFF pod restart), the watch exits zero without ever having seen a terminal phase — so this is very close to, but not perfectly identical to, "the run succeeded."
- **`waitForCompletion: false`** (fire-and-forget): the Job exits 0 as soon as the BFF accepts the create request, independent of the run's eventual outcome. `lastSuccessfulTime` means only that the trigger was **enqueued** — it is NOT a run-outcome signal.

In both modes, `lastSuccessfulTime` reflects what a single-cluster CronJob sees. It does not confirm success across every cluster a multi-cluster fan-out touched — for that, consult the BroodOperation detail views linked from the run-history panel.

## Safety: no duplicate destructive operations

The CronJob's `jobTemplate.spec.backoffLimit` is set to **0** — a single Job attempt with no in-Job retries. This is because `POST /api/v1/brood-operations` creates a *new* `BroodOperation` with a `crypto/rand`-suffixed name on every call; a retried Job after an ambiguous failure (create succeeded server-side but the pod died before exiting) would fire a **second, duplicate** brood operation. For destructive verbs (restart, drain, reprovision), duplicating fan-out actions is a materially worse outcome than skipping a run. With `backoffLimit: 0`, the narrow first-run-only race produces a clean skip; the CronJob's next scheduled tick retries naturally. The one-hour active deadline and one-hour finished-Job TTL above bound stuck and completed trigger Jobs.

## CLI reference

### `varroactl broodop run --ttl`

The `--ttl` flag maps to `ttlSecondsAfterFinished` on the BroodOperation:

```bash
# Run a reconcile with a 5-minute TTL on the resulting operation
varroactl broodop run reconcile --names ctrl-1 -n teams-payments --ttl=300

# Preview without creating
varroactl broodop run restart -l tier=canary --ttl=3600 --dry-run
```

When `--ttl` is omitted, `ttlSecondsAfterFinished` is not set and the operator's default (604800 seconds / 7 days) applies.

### `varroactl broodschedule`

```bash
# Create a schedule
varroactl broodschedule create my-daily-reconcile \
  --cron "0 6 * * *" \
  --verb reconcile \
  --names ctrl-a,ctrl-b \
  -n teams-payments

# Create with selector, execution params, and a TTL
varroactl broodschedule create my-restarter \
  --cron "0 */2 * * *" \
  --verb restart \
  --selector-json '{"matchLabels":{"tier":"canary"}}' \
  --namespaces teams-payments \
  --max-parallel 5 \
  --wait-for-completion=false \
  --ttl 300

# List schedules
varroactl broodschedule get

# Describe a schedule (full config + status)
varroactl broodschedule describe teams-payments/my-daily-reconcile

# Suspend / resume
varroactl broodschedule suspend teams-payments/my-daily-reconcile
varroactl broodschedule suspend teams-payments/my-daily-reconcile --resume

# Delete a schedule (removes CronJob + Secret via GC, removes VarroaRoleBinding via finalizer)
varroactl broodschedule delete teams-payments/my-daily-reconcile
```

## Run history

The dashboard's BroodSchedule detail page includes a **Run History** panel that queries `GET /api/v1/brood-operations?startedBy=schedule:<ns>/<name>` — a filtered view over the existing `BroodOperation` data. Each row links to the corresponding BroodOperation detail page. There is no separate history storage; the run history is exactly the set of `BroodOperation` CRs created by the schedule's fired Jobs.

## Limitations

### Single-cluster CronJob

The CronJob itself is a single-cluster Kubernetes resource resident in the schedule's namespace. When `template.clusters` has multiple entries (operator-namespace mode only), the triggered `varroactl broodop run` fans out to those clusters via the BFF's multi-cluster create path — the CronJob still lives in one cluster.

### `lastSuccessfulTime` fidelity

As noted above, `lastSuccessfulTime` is a single-cluster signal. The authoritative, complete-across-clusters run history is the BFF's `GET /brood-operations?startedBy=schedule:<ns>/<name>` query, not the `lastSuccessfulTime` field alone.

### Token expiry race

An operator outage spanning the full 30-minute rotation window can leave an expired token in the Secret for the next scheduled fire. The failure mode is a single Job pod that cannot authenticate to the BFF and exits with a 401 — safe (no run fires), and the next tick after the operator recovers re-mints the token.

### No CEL tenancy enforcement

Tenancy rules use namespace-aware Go validation, not CRD-level CEL (CEL cannot read `metadata.namespace`). Schedule CRs created with `kubectl` (bypassing the BFF) are caught at reconcile time.
