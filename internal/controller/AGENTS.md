# internal/controller

## Purpose

The operator half of Varroa (`cmd/operator`): controller-runtime reconcilers for every
`varroa.dev/v1alpha1` CRD, the `ResourceClient` abstraction over the Kubernetes API, and
the desired-state/command building that feeds the mite fleet over gRPC. This is the
package the root AGENTS.md "Controller reconciler" and "Provisioning"/"Version profiles"
sections describe.

## Ownership

- Owns: the `Controller` CR phase state machine, ComposedBundle→mite desired-state
  resolution and hashing, plugin-set/version-profile resolution, orchestration of
  RBAC-CRD reconciliation (built-in role seeding, JenkinsRole/JenkinsRoleBinding→mite
  push), Group/Team/User CRD reconciliation, fleet-wide BroodOperation/BroodSchedule
  reconcilers, webhook replay, seeding of the built-in starter bundle (`starter.go`),
  and the K8s resource builders (StatefulSet/Service/Ingress/ConfigMap/Secret) in
  `clientset_client.go`.
- Does not own: the mite gRPC server/transport (`internal/mite`), JCasC composition
  logic itself (`internal/bundle` — this package only reads the already-materialized
  ConfigMap and resolves vars), RBAC YAML generation (`internal/rbac` — this package
  only calls it), CRD type definitions (`api/v1alpha1`), or the BFF HTTP surface
  (`internal/api` — though `NATSReconcilerProxy` here is the bridge target for BFF's
  remote-cluster reconcile/reprovision/approve triggers).

## Local Contracts

- **`spec.composedBundleRef` is optional.** `Reconciler.effectiveBundleRef` is the single
  place that resolves it: a nil ref falls back by convention to the `varroa-starter`
  `ComposedBundle` in the operator namespace. Every read goes through it — the Pending
  bundle check, `resolveBundleForController`, and `bundleIdentOf` (sibling wave gating,
  which must keep sharing one identity across zero-config controllers). Dereferencing
  `cr.Spec.ComposedBundleRef` directly anywhere else reintroduces the nil panic this
  replaced.
- `StarterReconciler` (`starter.go`) seeds that bundle on a 60s ticker: two `CatalogItem`s
  whose `status.content` it writes itself (there is no source to sync — the content is
  `go:embed`ed from [bundles/](../../bundles/AGENTS.md)) plus the `ComposedBundle`
  composing them. Ownership is the `varroa.dev/starter` label and nothing else; objects
  under the reserved names that lack it are skipped and logged, never overwritten. A tick
  that would write identical bytes performs no write — the content hash is carried in the
  `varroa.dev/starter-hash` **annotation**, not a label; see
  [bundles/AGENTS.md](../../bundles/AGENTS.md) for why.
- **Brood verb policy is enforced in `reconcilePending`, never in the BFF.**
  `verbPolicyAllows` (`broodoperation_controller.go`) reads
  `ProvisioningDefaults.broodPolicy` and denies with a terminal phase + reason
  (`BroodOperationStatus` has no conditions). kubectl and GitOps create
  `BroodOperation` objects directly and never traverse the HTTP API, so the BFF's
  matching check in `handleBroodCreate` is UX only. Both call the same
  `BroodPolicy.ExecuteGroovyAllowed` in `api/v1alpha1` so they cannot drift.
  A failed read of the optional singleton **allows** — absence and a transient
  API error are indistinguishable here, and Varroa RBAC is the authorization
  boundary that has already run.
- **`dispatchTarget`'s Stop/Start must use `ApplyControllerSpecSSAIfExists`,
  never a separate `Get`-then-`ApplyControllerSpecSSA`.**
  `ApplyControllerSpecSSAIfExists` never creates the target: each attempt
  performs its own GET immediately before applying, fails terminally with
  `apierrors.NotFound` when absent, and stamps that GET's resourceVersion
  into the apply so the write conflicts if the object changed between the
  GET and the apply. Only an optimistic-lock (stale-resourceVersion)
  conflict retries (`retry.OnError`, `retry.DefaultRetry`, gated by
  `!isFieldManagerConflict(err)`) — a live controller's reconciler patches
  status continuously, which bumps resourceVersion on its own and would
  otherwise make this window reject routinely on an unrelated write; each
  retry re-GETs, so the existence guard is re-checked before every write.
  An SSA field-manager ownership conflict (force=false, another manager owns
  a touched field) surfaces immediately instead — identical retries can't
  resolve an ownership dispute, so `isFieldManagerConflict` excludes it from
  the retry budget. NotFound is never retried either. A caller with an
  earlier, separately-obtained `Get` result must not reuse it — the
  precondition is anchored to the GET this method performs, not an earlier
  one from a different client. `ApplyControllerSpecSSA`'s own
  create-on-absent behavior and existing call sites are unaffected. Each
  retry attempt hands the completion pass a fresh `runtime.DeepCopyJSON` of
  the caller's patch, never the shared map: `backfill` fills owned-leaf keys
  into its patch argument in place, so a reused map would carry a prior
  attempt's already-backfilled values into the next attempt and skip
  re-backfilling them from that attempt's own (possibly newer) GET.
- **executeGroovy audit carries a pointer, never a script body.**
  `groovyProvenance` puts `scriptSnapshotRef`/`scriptItemRef` (catalog) or
  `scriptSha256`/`scriptBytes` (inline) on `broodop.target.finished`. The
  activity stream retains up to 90 days and scripts routinely carry credentials.
  It is on `target.finished` and not `run.started` because `buildRunStartedEvent`
  fires before `resolveGroovyScript` has run.
- **executeGroovy "Succeeded" means the script ran, not that the call landed.**
  `/scriptText` answers HTTP 200 even for compile errors, so `runGroovyOnTarget`
  wraps every script via `wrapGroovyForClassification` (a GroovyShell harness with
  completion sentinels, `broodop_groovy_exec.go`) and `classifyGroovyOutput` turns
  a compile error or thrown exception into a Failed target. Dispatching a raw
  script to `/scriptText` reintroduces #529 (compile failures reported Succeeded).
- **The `upgrade` brood verb releases through the same annotation the
  upgrade-policy gate holds behind, never by writing `spec.version` directly
  for a release.** `dispatchUpgrade` resolves the target image (from
  `spec.action.upgrade.targetVersion` when set, otherwise from the target's
  own current version/profile; a `resourceOverlay` image override always
  wins) and writes it to the `varroa.dev/upgrade-release` annotation.
  `upgradePolicyVersionRollGate` (`upgradepolicy_gate.go`) consumes and
  clears that annotation when it matches the version-roll gate's computed
  `targetImage`, releasing a `manual`-policy hold without touching the
  cluster-wide dial. Only a `targetVersion` bulk move (granularity A) also
  writes `spec.version`, and only after the annotation write succeeds —
  `evaluateDispatchedTarget` requires the annotation gone AND the phase
  having left `{Connected, Running}` in the same evaluation before marking
  the target released, then waits for a return to `Connected` before
  Succeeded, so a target is never marked done on the write alone. Either
  granularity runs `bundle.CheckPluginPins` against the target's own
  unresolved bundle content first, and checks the resolved profile's
  `PluginsServable` condition when it has an open `ProfileCandidate`; either
  failing fails just that target with a distinct reason before anything is
  written.
- `Reconciler` (`controller_controller.go`) is the `Controller` CRD's controller-runtime
  reconciler. `SetupWithManager` (`operator.go`) wires: `For(&Controller{})`; an
  `UpdateFunc` predicate that skips status-only updates and only reacts to
  `Generation` changes or a bump of the `wake-requested`/`force-reprovision`
  annotations; `WatchesRawSource(source.Channel(r.reconcileEvents, ...))` for
  on-demand triggers (BFF-relayed reconcile/reprovision/wake, without this the
  generation-only predicate would never fire for an annotation-less trigger); and
  `Watches` on `JenkinsRoleBinding`/`JenkinsRole` that enqueue **every** Controller so
  an RBAC CRD edit is pushed live instead of waiting for the periodic tick.
- Phase dispatch is `reconcileController` (`controller_controller.go:1278`): handles
  deletion/finalization first, then `PowerState` `Stopped`/`Hibernated` short-circuits
  (scale StatefulSet to 0, `ScaleStatefulSet` is idempotent), then dispatches on
  `cr.Status.Phase` to `handlePending` (1499), `handleProvisioning` (1757),
  `handleRunning` (3013), `handleConnected` (2186), `handleFailed` (3100).
  - `handleProvisioning`: builds the Service (`reconcileService`, shared with the
    post-provisioning path so desired-Service derivation lives in one place),
    agent ServiceAccount+RBAC, bootstrap-token Secret (`ensureBootstrapToken`:
    re-minted only via `tokenNeedsRemint` — a still-valid token is never rotated,
    since the Secret takes up to ~60s to propagate to mounted volumes; also
    called on every not-connected `handleRunning` tick, because the 15-min TTL
    routinely expires during long plugins-init and waiting for the Pending
    reset wedges the mite for minutes), the `<prefix>-plugins` ConfigMap
    (**blocks** provisioning with an error if the ComposedBundle hasn't materialized
    yet — a core-only `plugins.txt` baked here would never self-heal once connected),
    runs `EvaluateCoreCompat` (`corecompat.go`) as a bake-time gate before any
    StatefulSet work, then `CreateStatefulSet` (create-or-update semantics).
  - `handleConnected`: syncs StatefulSet OIDC env vars every tick (idempotent, diffed
    against the live StatefulSet); retries a timed-out restart drain; tracks mite
    connectivity with a 3-tick grace period on disconnect (`staleMiteThreshold`-based)
    before forcing back to `Pending`; evaluates `ConditionPluginInstallRequired` by
    comparing the resolved managed-plugin checksum against the baked `plugins.txt`
    ConfigMap — automatic mode or an approved checksum (`ApprovedPluginRollChecksum`)
    transitions `Connected → Provisioning` to roll (**this is the plugin-roll
    convergence path**; manual+unapproved surfaces an approvable `PendingPluginRoll`
    instead); builds+pushes the `DesiredStateCommand` with a 5-minute convergence
    short-circuit (skip push if hash+RBAC unchanged and pushed recently), bypassed by
    `forcePush` (mite reconnect epoch change) or the one-shot force-reprovision
    annotation.
  - Before dispatching to `handleRunning`/`handleConnected`, `reconcileController`
    runs `reconcileVersionRoll` then (if it didn't already transition)
    `reconcileContainerSpecRoll` — the jenkins-image check compares an effective
    desired image (`effectiveDesiredJenkinsImage`) against the
    `varroa.dev/computed-images`-stamped container image on the live StatefulSet
    (`GetStatefulSetImages`, keyed by container name); `reconcileContainerSpecRoll`
    does the same for the mite container's **image, resource requests, and
    imagePullPolicy together** (one generalized check, not three parallel ones,
    so the fields can never drift out of sync with each other), all three read
    in a single `GetStatefulSetContainerSpecs` call (`clientset_client.go`) — image
    with the same computed-images-stamp/out-of-band-preservation semantics as
    `GetStatefulSetImages` below, resources/imagePullPolicy as a plain
    live-vs-desired read with no preservation stamp (nothing asks for that
    semantic on those two fields). `GetStatefulSetContainerSpecs` replaced a
    separate `GetStatefulSetImages` + `GetStatefulSetMiteResources` pair of
    calls that read the identical StatefulSet object twice on every
    Connected/Running reconcile tick — an avoidable extra API read per
    controller per tick on a large fleet (PR #373 review). An empty live
    `imagePullPolicy` (a template that predates Fix 2 baking the field onto
    the mite container at all) is defaulted to `defaultMiteImagePullPolicy`
    before comparison, or it would read as permanent drift against the
    desired default and trigger an unnecessary fleet-wide roll the first time
    this check runs against pre-existing StatefulSets (PR #373 review — same
    representation-mismatch-as-drift class of bug as the quantity issue
    below). The cpu/memory delta (`resourceListsEqual`, `controller_controller.go`) compares by parsed
    `resource.Quantity`, not raw string equality: `"1"` and `"1000m"` are the
    same quantity spelled differently and must read as converged, or a live
    value spelled differently from the desired value's source (overlay vs.
    `spec.miteSpec.resources` vs. the operator default) would report
    permanent drift with nothing left to actually change — an unconvergeable
    roll loop (PR #373 review). An empty string (unset) is never treated as
    equal to a set quantity. `overlay.ResourcesOverride`
    (`internal/overlay/imageoverride.go`) accepts any YAML scalar type for
    `requests.cpu`/`requests.memory`, not just quoted strings: an unquoted
    numeric overlay value (`cpu: 1`) decodes via `sigs.k8s.io/yaml` as
    `float64`, and a bare `.(string)` type assertion silently missed it,
    reading as "no override" and comparing against the wrong desired value
    (same PR #373 review, same root cause as the quantity-equality issue
    above: a representation mismatch masquerading as real drift). Any delta flips
    `Connected`/`Running → Provisioning` so `CreateStatefulSet` rolls the
    container (a second, non-plugin example of the plugin-roll convergence
    pattern above; issue #368 for the mite image originally, resources/pullPolicy
    added in fix/mite-convergence-hardening). Only the jenkins-image path runs
    through `evaluateVersionRollGate` (Jenkins core-compat); mite-container edits
    are Varroa's own component and need no gate — editing spec.miteSpec.* is its
    own approval. `effectiveDesiredMiteImage`/`effectiveDesiredMiteResources`/
    `effectiveDesiredMiteImagePullPolicy` are the single defaulting functions
    (each: `resourceOverlay.statefulSet`'s mite-container value wins if declared
    — independently per cpu/memory for resources — else the explicit
    `spec.miteSpec.*` field, else the operator default — `defaultMiteImage()` /
    zero resources / `defaultMiteImagePullPolicy` `"IfNotPresent"`) shared by
    provisioning bake (`handleProvisioning`) and this drift check — same
    lockstep requirement as `resolveCoreSet`/`coreSetForCr` below; an unset
    field must compare equal to exactly what provisioning bakes, or the
    controller rolls forever. The overlay precedence matters because
    `CreateStatefulSet` applies `resourceOverlay`/`podOverrides` *before*
    (re)stamping `varroa.dev/computed-images`: if a desired-value helper ignored
    an overlay override, the stamp/live state would forever reflect the overlay
    while the helper kept comparing against the spec/default, producing an
    unconvergeable roll loop (`effectiveDesiredJenkinsImage` has the same
    overlay check for the jenkins container, via `overlay.ImageOverride`;
    `overlay.ResourcesOverride`/`overlay.PullPolicyOverride` cover the other two
    mite fields, all three sharing the `findOverlayContainer` helper in
    `internal/overlay/imageoverride.go`). `podOverrides` cannot hit this: it's
    compiled by `CompilePodOverrides` against a single fixed container name
    (`overlay.JenkinsContainerName`) and has no image/resources field at all.
  - **Out-of-band image preservation** (`CreateStatefulSet`'s update path,
    `clientset_client.go`): when a live container image differs from the
    previous `varroa.dev/computed-images` stamp, that's a human hotfix applied
    directly to the StatefulSet (bypassing the operator) — e.g. `kubectl patch`
    the mite image to work around a bug before a fix ships. The update path
    preserves that live image **only while the operator's own newly-computed
    value for that container is unchanged from the old stamp**
    (`stampNew[name] == stampOld[name]`); the moment spec-driven intent moves
    (`effectiveDesiredMiteImage()` changes via class/spec/overlay resolution,
    or `spec.version` edited, or a resourceOverlay change),
    the new desired value wins over the stale hotfix — a real spec edit is
    stronger evidence of intent than an old out-of-band patch. The
    `varroa.dev/computed-images`
    stamp is written **once, from the desired state, before the preservation
    loop runs** and is deliberately *not* re-derived from the post-preservation
    template — the stamp's meaning is "the operator's own desired-value
    baseline as of this reconcile," and the `(2)` preservation check above only
    works if `prev` keeps that meaning across ticks. Re-deriving the stamp from
    what actually landed on the template (tried and reverted during PR #373
    review) turns `prev` into "whatever got applied" on the very next tick,
    which no longer equals `want` even when desired never moved — silently
    stomping the preserved override back to the (unchanged) desired value one
    reconcile later. Ground truth for "what's actually on the template right
    now" is the live map (`GetStatefulSetImages`'s second return,
    `GetStatefulSetContainerSpecs`), used only for the informational "preserved
    (out-of-band override)" note in `reconcileContainerSpecRoll`/
    `reconcileVersionRoll`, never to decide whether to roll. This preservation
    logic is shared by the jenkins and mite containers (and any future stamped
    container) — the jenkins-container path (`reconcileVersionRoll`) gets the
    identical fix for free from the shared code, and its tests assert the same
    semantics. `imagePullPolicy` preservation is **not** shared identically:
    it still piggybacks on the image predicate above for every container
    *except* `mite`, whose `imagePullPolicy` has its own independent
    spec-driven drift check (`reconcileContainerSpecRoll`). Preserving the mite
    container's out-of-band pull policy alongside a preserved out-of-band
    image would silently revert a genuine desired-image/pull-policy change
    (resolved through class→spec→overlay precedence) back to the stale live value
    on every Provisioning pass — an
    unconvergeable Connected->Provisioning loop (PR #373 review). Other
    containers (jenkins, plugins-init, init-groovy, ...) have no such
    independent check anywhere in the reconciler, so preserving their
    out-of-band pull-policy edits remains correct — nothing will ever try to
    converge them back.
- **`varroa.dev/casc-content-hash`** is a deterministic hash of the CASC
  ConfigMap payload (`cascContentHash`, `sha256Hex(json.Marshal(data))` — JSON
  marshal of a `map[string]string` sorts keys and escapes values
  unambiguously, avoiding the collision a delimiter-joined concatenation
  would risk), stamped on the pod
  **template** (not object metadata, unlike `varroa.dev/computed-images`) so a
  content change rolls the pod even when the running Jenkins container never
  reruns init to pick it up. `CreateStatefulSet` stamps it **after**
  `PodOverrides`/`ResourceOverlay` are merged so a user overlay cannot
  silently drop it — do not move the stamp before the overlay merge. The
  automatic roll assumes the default `RollingUpdate` strategy; a
  `resourceOverlay` setting `spec.updateStrategy: OnDelete` takes over pod
  recycling and needs a manual pod delete to pick up the change, same as the
  plugin-checksum roll.
- **Immutable STS spec fields are preserved from live on update** —
  `CreateStatefulSet`'s update branch carries `volumeClaimTemplates`,
  `serviceName`, `selector`, and `podManagementPolicy` verbatim from the existing
  StatefulSet (k8s forbids updating them; sending a rendered difference rejects
  the whole update and wedges Provisioning forever — PR #382, caught live on the
  post-#379 canary). `spec.persistence` and overlay edits to these fields are
  therefore creation-time-only: they take effect on controller teardown/recreate.
- **Fleet pod identity** — `buildStatefulSet` sets
  `app.kubernetes.io/managed-by: varroa-operator` on pod-template labels only;
  `reconcileFleetPodLabel` converges existing Running/Connected StatefulSets by
  changing only that live template-label map. Never add it to StatefulSet object
  labels or the immutable selector.
- `resolveBundleForController` (`controller_controller.go:217`) is the bundle hot
  path: requires `cr.Spec.ComposedBundleRef`; blocks (returns error) until the
  ComposedBundle is `Ready` or `Drifted` (Drifted has valid content — drift is only a
  warning; `Pending`/`Invalid` block); reads the **unresolved** content from the
  ConfigMap named by `cb.Status.ContentRef`; injects `varroa_controller_*` vars —
  `varroa_controller_endpoint` must point at the UID-named `<prefix>-svc` Service
  (`controllerPrefix(cr)+"-svc"`), never the bare CR name, or agents get connection
  refused; external URL/path-prefix depend on `IngressSpec.RoutingMode()`
  (subdomain vs path); merges the resolved `JenkinsVersionProfile`'s
  `spec.jcasc.content` via `bundle.MergeJenkinsYAML` **before** calling
  `bundle.ResolveVars`, so `${varroa_controller_*}` placeholders inside the overlay
  also resolve; then resolves all vars and runs `findUnresolvedVars` as the
  completeness check — any leftover `${...}` fails resolution. Returns
  `(*bundle.MaterializedBundle, cb.Status.ResolvedHash, bundleIdentity, error)`.
- Version profiles: `resolveProfileForCr` → `ResolveProfile` (`versionresolve.go`)
  matches **exact version → LTS line (`lineKey`, `major.minor`) →
  `pluginlock.Baseline()`** (embedded fallback, no profile). `resolveCoreSet` /
  `coreSetForCr` (`controller_controller.go:822`/`850`) turn the matched profile (or
  baseline) into `[]pluginlock.PluginEntry`, fed into `managedPluginLines` for both
  provisioning-time `plugins.txt` and connected-phase drift detection — these two call
  sites must stay in sync or plugin drift will fire spuriously.
- `ResourceClient` (`controller_controller.go`, ~40 methods) is the operator's deep-op
  K8s surface (StatefulSet/Service/Ingress/Secret/ConfigMap/pod/wake operations plus the
  named special-semantics methods `PatchControllerStatus`/`ApplyControllerSpecSSA`/
  `SetHibernated`/`ClearUserPassword`); `ClientsetClient` (`clientset_client.go`)
  is the only real implementation (client-go clientset + dynamic client + REST config)
  and also implements `crdstore.Backend` (`crdstore_backend.go`) — all per-kind CRD CRUD
  goes through `internal/crdstore` typed helpers on the reconcilers' `store` field, never
  through new interface methods. Because that funnels every CRD call in the process onto
  one dynamic client, and client-go builds one rate-limit bucket **per REST client**, the
  constructors call `TuneClientRates` to lift client-go's QPS 5 / Burst 10 defaults to
  50/100 — at the old defaults a bulk pass throttled everything behind it (#510).
  It is deliberately a finite ceiling rather than controller-runtime's `QPS = -1`: API
  Priority and Fairness is not guaranteed on every target cluster. An explicitly
  configured QPS is never overridden. `cmd/gateway` calls the same helper; the manager is
  built from this same config (`cmd/operator/main.go`), so it inherits the setting. Pure desired-state derivation (STS spec build, plugin-
  roll gate, update-center gate, mite health) lives in `desiredstate.go` with table tests
  in `desiredstate_test.go`; the phase handlers gather → decide → apply.
  `CreateStatefulSet`/`CreateService`/`CreateIngress` are create-or-update.
  `EnsureWakeEndpointSlice`/`DeleteWakeEndpointSlice` own the hibernation traffic flip;
  slice failures are warning-only and confirmed deletes are memoized for retry safety.
  The slice name is `<full-service-name>-wake`; retaining the full Service name as the
  prefix is required for ingress-nginx EndpointSlice association.
  Cleanup still runs when wake serving is disabled and on provisioning timeout, so a
  stale flip cannot strand Failed/rollback controllers on the operator wake port.
  `reconcileIngress` (`controller_controller.go:3268`) runs `CreateIngress` every tick
  for `Running`/`Connected` so Ingress spec/TLS changes converge without a
  re-provision.
- `SetProvisioningDefaults` and `RootDomain` share `provisioningDefaultsMu`; the
  every-replica defaults refresh feeds both reconcile policy and wake-server host
  resolution without racing HTTP requests.
- `PatchControllerStatus` (`clientset_client.go:1442`) sends a JSON **merge patch**.
  Fields only added to the patch map when non-empty (`status.X != "" / != nil`
  guards — e.g. `AppliedBundleHash`, `FirstConnectedAt`, `WakeToken`,
  `LastDesiredPushAt`) are **sticky**: a merge patch that omits a key can never clear
  it server-side. Fields always included (even zero-valued) — `phase`, `conditions`,
  `overlayWarnings`, `pendingPluginRoll`, `approvedPluginRollChecksum`, `liveDrift`,
  `rollout`, `lastReconcileError`, `lastReconcileErrorAt` — are the only ones that can
  be cleared. When adding a status field, decide up front whether it must ever be
  cleared; if so it must be unconditional in `statusPatch`, not behind a non-empty
  guard.
- **`ConditionReconcileBlocked`** (C3, `markReconcileBlocked` helper near
  `persistStatusDiagnostics`) is the standing pattern for surfacing a reconcile pass
  that hit an unresolved error: sets the condition `True` + `Reason` (one of 19
  site-specific reasons in `api/v1alpha1/types.go`), stamps
  `LastReconcileError`/`LastReconcileErrorAt`, records the
  `varroa.controller.reconcile.blocked` gauge at `1`. A guarded branch in
  `reconcileController`'s metrics `defer` clears it back to `False` on the next
  successful pass, skipping the clear when `oldPhase == ControllerPhaseFailed` (that
  pass's success reflects the *previous* tick, not resolution) and no-oping when the
  condition was never set (so healthy controllers get zero status noise). New
  reconcile-blocking call sites should call `markReconcileBlocked` rather than
  inventing a parallel mechanism.
- `buildDesiredStateCommand` (`controller_controller.go:3457`) assembles the
  `mitev1.DesiredStateCommand`: strips `authorizationStrategy` and injects the
  project-naming strategy into `JcascYaml`; generates `RbacYaml` via
  `rbacGenerator.GenerateWithAdminCheck` and **refuses to push** RBAC that would leave
  no human admin unless `varroa.dev/allow-admin-lockout=true` is set (surfaced as
  `ConditionRBACLockoutRisk`); attaches a cached mite Jenkins JWT
  (`mintOrGetMiteToken`) to **every** command, even when content is otherwise
  unchanged, so a mite that lost its in-memory token on restart gets a fresh one on
  the next push; `Reload` is always `true` (#166 — config push always uses the
  MANAGE-gated `/configuration-as-code/reload` path, never the admin-gated apply
  path). `cmd.DesiredStateHash = computeDesiredStateHash(cmd)` hashes `JcascYaml` +
  `RbacYaml` + `ItemsYaml` only.
- **Hash re-stamp quirk**: `handleConnected` calls `buildDesiredStateCommand` (which
  sets its own internal hash), then immediately recomputes
  `computeDesiredStateHash(d)` again and reassigns `d.DesiredStateHash`
  (`controller_controller.go` ~2596) before using it for the convergence
  short-circuit and `cr.Status.DesiredStateHash`/`AppliedBundleHash` comparisons.
  Fields mutated **after** that recompute point (`ApplyWhen`, `MaxDeferSec`,
  `DrainTimeoutSec`, the second `Reload = true`) are deliberately excluded from the
  hash — it is content-only. A new field that affects applied Jenkins content must be
  set inside `buildDesiredStateCommand` (before its internal hash) or before the
  handler-level recompute, never after.
- `NATSReconcilerProxy` (`nats_reconciler.go`) implements the same `ReconcilerAPI`
  interface (`reconciler_api.go`) as `*Reconciler`, but publishes over NATS instead of
  reconciling locally — it is what `internal/api` uses to trigger reconcile/
  reprovision/approve-restart/approve-deletion on a **remote** cluster's operator.
- Other controller-runtime / ticker reconcilers in this package:
  - `CatalogReconciler` (`catalog_controller.go`) — `CatalogSource` +
    `ComposedBundle`; `Reconcile` is a **three-way** dispatch: `repoURL` (git
    clone) vs `ociRef` (OCI pull via `oci.RegistryStore` + `Resolve` → digest as
    `observedRevision` → `Pull` → `FetchBlob` → untar) vs the reserved
    `varroa-update-center` source, which carries neither and is backed by the
    update-center store (`uccatalog.go`, `ucclosure.go`, `uccompat.go`). CEL
    cannot read `metadata.namespace`, so "reserved name only in the operator
    namespace" is a runtime guard here, and `CatalogReconciler` also carries the
    teardown backstop that deletes the reserved source when the `UpdateCenter`
    singleton is gone — `UpdateCenterReconciler` is not registered at all when
    the component is disabled, which is exactly when nothing else would clean up.
    All three arms share one write discipline: `desired[name]` is recorded
    **before** the write attempt (a failed write used to make its own item a
    prune candidate in the same pass), writes go through `crdstore.ApplyOwned`
    with the single `itemOwnedBy` predicate (label **and** controller ownerRef
    UID), and prune re-checks the same predicate. Any inventory or profile read
    failure returns **before** prune, leaving `lastSyncTime`/`observedRevision`
    untouched; a 200 decoding to an empty `plugins` list (with no
    `skippedPacks`) is a legitimate empty store and does prune. A 200 whose
    `skippedPacks` is non-empty is a **partial** listing, not a failure:
    `fetchUCInventory` returns it alongside `entries` rather than as an error,
    the readable subset still syncs normally, but `reconcileUpdateCenterSource`
    withholds `pruneCatalogItems` for that pass and surfaces a warning
    naming the unreadable pack refs — a lower bound must never license
    deleting items that unreadable pack may back.
    The update-center arm derives one item per plugin, so at store scale it is
    hundreds of CRD writes per pass and it dominates the catalog tick — and
    because the tick reconciles every source *before* every `ComposedBundle`
    (`cmd/operator/main.go`) and `tickerRunnable` resets its timer only after
    the tick body returns, a long pass here starves bundle recomposition
    outright (#510). `ucSyncDigest` therefore gates derivation: it fingerprints
    the inventory **and** the version profiles, and a pass whose digest matches
    `status.observedRevision` skips straight to `markUCUnchanged`, which
    advances `lastSyncTime` only. The profiles half is load-bearing —
    `resolveClosure`/`evaluateCompat` both read profiles, so a lock edit changes
    derived content while the inventory is byte-identical. The digest cannot see
    items deleted or edited out of band, so `ucFullPassInterval` (30m,
    process-local `ucLastFullPass`) forces a full derive regardless; that field
    is a cost optimization only, never a correctness input, so losing it to a
    restart or leader change simply forces one full pass.
    `ReconcileComposedBundle`'s
    drift loop has OCI branches parallel to the git branches: resolves creds via
    `OCIAuthFromSecret`, resolves manifest digest via `oci.RegistryStore.Resolve`,
    populates `resolvedOCIAuth[i]`, and `hasNewOCIInputs` gates the skip path.
    `Composer.Compose` receives `resolvedOCIAuth` as its 5th argument. An
    `itemRef` that no longer resolves records the `itemMissingRevision`
    sentinel (`"<missing>"`, never valid sha256 hex) rather than being skipped:
    skipping left no `observedRevisions` entry, so the gate saw no drift and the
    bundle kept serving content for a vanished input. The sentinel makes the
    disappearance drift exactly once — the composer then reports it via
    `result.Missing` and last-good content is retained — and stops it
    recomposing on every later tick.
  - `RoleReconciler` (`role_controller.go`) — reconciles the built-in `VarroaRole`/
    `JenkinsRole` singletons (`varroa:admin/operator/developer/viewer/system-mite`)
    and migrates legacy custom roles.
  - `JenkinsVersionProfileReconciler` (`versionprofile_controller.go`) — ticker
    reconciler; materializes `spec.pluginSetRef` into an owned `<name>-pluginset`
    ConfigMap, sets `PluginSetReady`/non-blocking `LockJcascMismatch`. A profile
    is **eligible** for derived-item resolution and badging only when
    `PluginSetReady == True` AND `status.contentRef` is set: a not-ready profile
    can still carry a stale `contentRef`, and a stale lock voting in a unanimity
    test manufactures agreement that does not exist.
  - Derived catalog items (`ucclosure.go`, `uccompat.go`) — the closure solver
    is a fixpoint over a **monotone** constraint accumulator, iterating sorted
    key slices in both passes so the emitted fragment is byte-identical for
    identical inputs. A single traversal is wrong twice over: a name-keyed
    visited set lets the first path to arrive violate another path's minimum,
    and dependency edges are per-version, so recomputing minima from only the
    currently-selected versions cycles forever. `resolveOne` is four ordered,
    first-match-wins rows and fails only when no version can be **named**; a
    shortfall is never a failure. `internal/pluginver` orders every plugin
    version; `internal/jenkinsver` is used at exactly one site — comparing a
    plugin's `requiredCore` against a profile's effective deployed core — because
    it truncates at the first `-` and would make two distinct plugin releases
    compare equal. Verdicts are advisory: they never set `status.valid = false`,
    and `EvaluateCoreCompat`/`pluginVersionConflict` remain the only gates.
  - `GroupReconciler`, `UserReconciler`, `TeamReconciler` (`group_controller.go`,
    `user_controller.go`, `team_controller.go`) — Group/User/Team CRDs; Team also
    provisions/owns a namespace.
  - `BroodOperationReconciler` / `BroodScheduleReconciler`
    (`broodoperation_controller.go`, `broodschedule_controller.go`) — fleet-wide
    operation fan-out (dispatch, per-target tracking, timeout) and CronJob-backed
    scheduled brood operations.
- `bootstrap.go` — `BootstrapLocalAdmin` seeds the local-mode admin user/password on
  first boot.
- `webhook_replay.go` — drains a NATS queue of webhook deliveries received while a
  controller was hibernated, replaying them once the mite reconnects.
- `sharding/` (`ring.go`, `manager.go`) — consistent-hash shard ownership so multiple
  operator replicas split the `Controller` watch set; `SetShardOwnership`/
  `ownsController`/`EnqueueShards` on `*Reconciler` consume it.
- `pluginlock/lock.go` — the embedded plugin-lock baseline
  (`pluginlock.Baseline()`, `PluginEntry`) used as the last-resort version-profile
  match when no `JenkinsVersionProfile` matches a controller's version.
- `pluginlock/seed/` — embedded, generated ship-time default
  `JenkinsVersionProfile` content (`profile.yaml`/`plugins.yaml` per version,
  parsed by `pluginlock.Seeds()`), regenerated by `hack/gen-plugin-lock.sh`.
  Never hand-edit. `versionprofile_seed.go` (`VersionProfileSeedReconciler`) is
  the runtime consumer: it applies each seed as an owner-labeled
  `JenkinsVersionProfile` CR plus a paired pluginset ConfigMap, skipping any
  object a hand-authored or promoted CR/ConfigMap already owns.

## Work Guidance

- Any change to `DesiredStateCommand` fields must account for
  `computeDesiredStateHash`'s field set and the re-stamp ordering above — decide
  whether the new field affects applied content (must be hashed) or is
  transport/policy metadata (excluded, like `ApplyWhen`/`MaxDeferSec`).
- New `Controller` status fields: decide sticky-vs-clearable up front and follow the
  unconditional-inclusion pattern in `PatchControllerStatus` if it must ever clear.
- New phase-affecting signals belong on `cr.Status.Conditions` via `setCondition`
  (the pattern every reconciler here uses), not new top-level status booleans.
- `resolveCoreSet`/`coreSetForCr` must stay in lockstep (provisioning bake vs.
  connected-phase drift check) — a divergence causes spurious
  `PluginInstallRequired` flapping.

## Verification

```bash
make test
make lint
go test -race -count=1 ./internal/controller/...
```
