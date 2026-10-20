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
  the K8s resource builders (StatefulSet/Service/Ingress/ConfigMap/Secret) in
  `clientset_client.go`, the pipelines-tenant cell machinery (`cells/`,
  `cell_*.go`, `cells_config.go`) that binds a `Team` to a shared,
  multi-tenant Jenkins cell, and build-record rehydration rendering
  (`rehydrate/`).
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
- **The BFF's content read grant hangs off the status patch, never off the compose
  branch.** `ReconcileComposedBundle` writes a `Role` + `RoleBinding`
  (`<bundle>-content-reader`, `get` on `status.contentRef` alone, owner-referenced to the
  bundle) so the BFF can read that ConfigMap outside the release namespace. The write goes
  through `applyStatusAndGrant`, which wraps all nine `patchStatus` call sites, plus one
  direct call on the recompose skip path. Putting it beside the content ConfigMap write
  instead would miss eight exits: six pass a literal `""` contentRef that `omitempty` +
  JSON merge patch never persists (so the **previous** contentRef stays live and readable),
  and two deliberately retain `previousContentRef`. A bundle stuck on a bad git host would
  then keep readable content with a grant that never heals. The effective contentRef is
  `status.ContentRef` falling back to `cb.Status.ContentRef`, for that same reason.
- **A failed grant write is a `status.warnings` entry — never `status.errors`, never a
  phase change.** The bundle's content is still correct; only the BFF's read is blocked, and
  a controller reads that ConfigMap with the operator's own permissions. `status.errors` and
  the phase both describe content, and `ComposedBundleDetail` renders errors unconditionally,
  so an entry there would show a healthy bundle as broken. An empty
  `VARROA_BFF_SERVICE_ACCOUNT` disables the grant entirely and warns once per process
  (`sync.Once`), which is the hive-mode case.
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
  `PluginsServable` condition when it has an open `ProfileCandidate`; a
  `False` condition fails just that target with a distinct reason before
  anything is written. An `Unknown` condition holds the target at `Pending`
  and retries on a later tick.
- **The Jenkins upstream host is one operator value.** `cmd/operator` reads
  `VARROA_JENKINS_UPSTREAM_URL` through `controller.JenkinsUpstreamURLFromEnv` (the only
  parser; the BFF calls it too) and passes it to the upgrade tracker, the candidate
  reconciler, and `SetJenkinsUpstreamURL`. `ClassifyJenkinsUpstream` maps the value to
  `online`, `mirror`, or `off` for display only; the BFF never gates on it. An empty value turns
  the tracker off and makes candidates report `UpstreamMetadataDisabled`, and it sets
  `StatefulSetSpec.JenkinsUpstreamOff`. Every Jenkins JVM then gets
  `UsageStatistics.disabled` (always) and `UpdateCenter.never`/`DownloadService.never`
  (when off), all built in `buildStatefulSet`. Do not add a second source for these
  flags or an unconditional `https://updates.jenkins.io` literal.
  `CreateStatefulSet` stamps `varroa.dev/jenkins-upstream` (`on`/`off`) on the object
  metadata. `reconcileJenkinsUpstreamRoll` (`jenkinsupstream_roll.go`) rolls a Connected
  controller or cell once when the stamp differs from the operator value. A StatefulSet
  with no stamp falls back to its live `JAVA_OPTS`: the never token means `off`. The stamp
  wins over the token, so a `podOverrides` never token does not loop the roll.
- `Reconciler` (`controller_controller.go`) is the `Controller` CRD's controller-runtime
  reconciler. `SetupWithManager` (`operator.go`) wires: `For(&Controller{})`; an
  `UpdateFunc` predicate that skips status-only updates and only reacts to
  `Generation` changes or a bump of the `wake-requested`/`force-reprovision`
  annotations; `WatchesRawSource(source.Channel(r.reconcileEvents, ...))` for
  on-demand triggers (BFF-relayed reconcile/reprovision/wake, without this the
  generation-only predicate would never fire for an annotation-less trigger); and
  `Watches` on `JenkinsRoleBinding`/`JenkinsRole` that enqueue **every** Controller so
  an RBAC CRD edit is pushed live instead of waiting for the periodic tick.
- `NotifyCommandResult` (`controller_controller.go`) enqueues an owned reconcile when a
  desired-state result applies its rehydration seed. It skips a result with deferred
  jobs, a rehydration error, or no applied seed, because the tick paces the bundle resend.
  `CellSeedStateChanged` (`cell_rehydration_entry.go`) decides the Team ticker wake.
- Phase dispatch is `reconcileController` (`controller_controller.go:1278`): handles
  deletion/finalization first, then `PowerState` `Stopped`/`Hibernated` short-circuits
  (scale StatefulSet to 0 through `scaleTickToZero` and `ScaleStatefulSetToZero`), then dispatches on
  `cr.Status.Phase` to `handlePending` (1499), `handleProvisioning` (1757),
  `handleRunning` (3013), `handleConnected` (2186), `handleFailed` (3100).
  - `handleProvisioning`: builds the Service (`reconcileService`, shared with the
    post-provisioning path so desired-Service derivation lives in one place),
    agent ServiceAccount+RBAC, bootstrap-token Secret (`ensureBootstrapToken`:
    re-minted only via `tokenNeedsRemint` — a still-valid token is never rotated,
    since the Secret takes up to ~60s to propagate to mounted volumes — or when
    the pod instance changed: the Secret's `instance` key names the pod that
    first ran with the token, and a replaced pod re-mints, since a cell's
    emptyDir home keeps no mite identity across pods; also
    called on every not-connected `handleRunning` tick, because the 15-min TTL
    routinely expires during long plugins-init and the mite otherwise wedges
    until `provisioningTimeout`), the `<prefix>-plugins` ConfigMap
    (**blocks** provisioning with an error if the ComposedBundle hasn't materialized
    yet — a core-only `plugins.txt` baked here would never self-heal once connected),
    runs `EvaluateCoreCompat` (`corecompat.go`) as a bake-time gate before any
    StatefulSet work, then `createTickStatefulSet` → `CreateStatefulSet` (create-or-update; a create or a scale-up from 0 runs the tick fence first, and the update carries the StatefulSet `resourceVersion` read before the fence).
  - `handleConnected`: syncs StatefulSet OIDC env vars every tick (idempotent, diffed
    against the live StatefulSet); retries a timed-out restart drain; tracks mite
    connectivity with a 3-tick grace period on disconnect (`staleMiteThreshold`-based);
    after it, `markStreamLost` sets `Running` (never `Pending`), calls
    `clearMiteConnection`, and sets `MiteConnected=False/StreamLost`. The pod is
    unchanged, so a reprovision pass only churns; evaluates `ConditionPluginInstallRequired` by
    comparing the resolved managed-plugin checksum against the baked `plugins.txt`
    ConfigMap — automatic mode or an approved checksum (`ApprovedPluginRollChecksum`)
    transitions `Connected → Provisioning` to roll (**this is the plugin-roll
    convergence path**; manual+unapproved surfaces an approvable `PendingPluginRoll`
    instead); builds+pushes the `DesiredStateCommand` with a 5-minute convergence
    short-circuit (skip push if hash+RBAC unchanged and pushed recently), bypassed by
    `forcePush` (mite reconnect epoch change), the one-shot force-reprovision
    annotation, or an attached rehydration bundle (`desired.Rehydration`, never
    a pending tenant with no bundle). NATS keeps no copy of the command, so `DesiredStateHash` and
    `LastDesiredPushAt` are stamped only after `Send` succeeds. On
    `transport.ErrMiteUnreachable` the push raises no `ReconcileBlocked`. The
    connection epoch is recorded only after a delivered push, and a reconnect
    whose hash equals `DesiredStateHash` is pushed past the pause, manual-mode,
    and wave gates (`redeliver`), because the removed KV bucket no longer
    replays it. Each delivered push also stamps `DesiredContentHash`
    (`computeDesiredContentHash`, no tenant homes). In `manual` mode,
    `manualOperatorOwnedChange` pushes a change that keeps that content hash at once
    when `LastApplyResult` shows the delivered hash applied, holds it with no
    pending restart while that hash has no result (an approved push can wait for
    idle builds, and the mite keeps only the newest state), and raises a pending
    restart when that apply failed. A cell whose full hash is unchanged pushes an
    attached rehydration bundle under the same push and hold rule; it raises no
    pending restart and clears a stale one. A push that goes out while a
    rehydration push is in flight carries the held bundle.
  - `handleRunning` never changes the phase of a disconnected mite. Its
    `provisioningTimeout` guard skips a controller whose `StreamLost`
    transition is later than `ProvisioningStartedAt` (`streamLostSince`); a
    later provisioning run re-arms the guard. A reconnect writes
    `MiteConnected=True/StreamRestored` only when the condition already
    exists. `nextRequeueDuration` requeues a `Running` + `StreamLost`
    controller at `streamLostRequeue` (15 s); nothing else wakes it on a
    reconnect. When `handleRunning` moves the phase to `Connected`, the same
    pass runs `reconcileHeadlessRoll`, `reconcileArtifactSecretRoll`,
    `reconcileJenkinsUpstreamRoll`, and `handleConnected` (`runConnectedAfterRunning`), so the first desired-state
    push follows the connect. The chain runs only after a good runner
    contribution read and with no armed force-reprovision annotation. A chained
    error writes the status before it returns. The brood `connected` event wakes
    the owning replica.
  - Before dispatching to `handleRunning`/`handleConnected`, `reconcileController`
    runs `reconcileVersionRoll` then (if it didn't already transition)
    `reconcileContainerSpecRoll`, then `reconcileMiteTransportEnvRoll`
    (`Running` + `StreamLost` + transport still disconnected only: compares the
    StatefulSet template's mite `VARROA_CA_PEM`/`VARROA_ENDPOINT` via
    `GetStatefulSetContainerEnv` with `r.caPEM`/`r.varroaEndpoint`, and rolls
    to Provisioning on a delta or a missing StatefulSet — the CA-regeneration
    recovery path), then `reconcileHeadlessRoll` and
    `reconcileArtifactSecretRoll` (Connected only; the latter compares the per-cell
    artifact Secret's content hash with the `varroa.dev/artifact-secret-hash`
    pod-template annotation, `artifactcasc.go`; in STS mode it rolls only a cell
    whose live `JAVA_OPTS` lacks the shared credentials property), then
    `reconcileJenkinsUpstreamRoll` (Connected only; see the Jenkins upstream bullet) — the jenkins-image check compares an effective
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
  A boot-failed pod cannot roll on its own under `OrderedReady`.
  `replaceStaleBootFailedPod` deletes it once when its `controller-revision-hash`
  lags `status.updateRevision`. The guard requires `RollingUpdate` with no
  partition and skips a terminating pod.
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
  also resolve; computes `coreSetForCr` once and passes it to the cell's own
  tenant merge (`applyCellContent`); after that merge and still
  before `ResolveVars`, calls `checkBranchBuildStrategiesPlugin`
  (`items_engine_safety.go`, design D12) and `checkFolderLibraryPlugins`
  (refuses a folder library whose `RequiredPlugins` the resolved set omits),
  and fails the resolve when the items
  content holds a `multibranch` or `organizationFolder` item and the resolved
  plugin set omits `basic-branch-build-strategies` — a missing class would
  make XStream drop the rendered `buildStrategies` element or fail the item
  create, so a silent omission would produce an initial-index build storm
  with no warning; then resolves all vars and runs `bundle.RemainingVarRefs` as the
  completeness check — any leftover `${...}` fails resolution. A non-cell
  controller then runs `checkPlainFolderLibraryCredentials`
  (`library_credentials_plain.go`), which reads the GLOBAL-scope system store of
  the resolved `jenkins.yaml` and the folder credentials, and refuses a folder
  library whose credential is missing, SYSTEM-scope, or of a rejected kind. For a cell
  both steps skip `items.yaml`: it is tenant content only, which
  `assembleCellContent` already resolved and checked per tenant, so a cell
  variable never reaches a tenant item and a tenant fault never fails the
  cell. After that
  check, `bundle.InjectPipelineTemplates` merges the controller's own
  delivered pipeline-template set (`mat.PipelineTemplatesYAML`, folder `""`),
  then once per entry in `mat.CellTemplateFragments` (a cell only, one entry
  per placed tenant, `frag.Folder` stamped by `applyCellContent`) — after
  `ResolveVars`, never before, because a template body holds its own
  `${...}` Groovy text that is not a catalog variable (design D7). In the
  same place, `injectTenantFolderCredentials` adds each entry of
  `mat.CellFolderCredentials` to its tenant folder in `mat.ItemsYAML`. A
  credential value is Secret data: `ResolveVars` would change a `${...}` text
  in it, and the check would fail every tenant of the cell on it. Returns
  `(*bundle.MaterializedBundle, cb.Status.ResolvedHash, bundleIdentity, ControllerCondition, error)`.
  The four items refusals (`checkBranchBuildStrategiesPlugin`,
  `checkFolderLibraryPlugins`, `checkGiteaPlugin`, `checkPlainFolderLibraryCredentials`)
  wrap their error in `errItemsRefused` (`items_engine_safety.go`). Provisioning
  reports them as `BundleResolved=False/ItemsRefused` and blocks. Connected reports
  them as `BundleFailed=True/ItemsRefused`. Any other resolve fault keeps its own
  reason. A successful resolve in Provisioning sets `BundleResolved=True`.
  On its one success return path it also calls `clearBundleFailed(cr)`, the
  single seam every caller shares for retiring a stale `ConditionBundleFailed`.
  A merge patch cannot clear a condition on its own, so a caller must never
  add its own parallel `removeConditionByType` call for this condition.
  `handlePending` checks only that the bundle exists and clears nothing; the
  Provisioning resolve that follows retires the condition.
- **Artifact-storage merge step** (`artifactcasc.go`) — the last content edit inside
  `resolveBundleForController`, after the version-profile JCasC overlay merge and still before
  `bundle.ResolveVars`. `applyArtifactStorage` calls `artifactStorageFragment`, which renders the
  operator's own `artifact-manager-s3` JCasC fragment (jclouds/s3 factory selection, the `aws`
  root's credential and bucket settings — task 0.5's export puts these under `aws`, not
  `unclassified`) and a `Controller` condition `ArtifactStorage`, together. The fragment emits,
  and the condition reports `True`/`External`, only when all four hold: `VARROA_BLOBSTORE_URL`
  parses to an `s3://` URL (`parseBlobstoreURL`; a `pgblob:` URL or a parse failure yields
  `False`/`LocalOnly`), a candidate artifact Secret exists in the release namespace, and
  `coreSetForCr` resolves a plugin set that pins `artifact-manager-s3` (else
  `False`/`PluginMissing`). The Secret lookup is one pair, shared by every reader:
  `artifactSecretCandidates(cr)` returns `<VARROA_ARTIFACT_SECRET>-<cr.Name>` first, then
  `<VARROA_ARTIFACT_SECRET>-<pool>` when `cr` is a cell carrying a `varroa.dev/cell-pool` label
  (the pool name comes from the label string alone; the `CellPool` object need not exist).
  `lookupArtifactSecret(ctx, candidates)` reads each in order through `ResourceClient.GetSecret`:
  a `NotFound` or empty-data answer moves to the next candidate, any other error stops the lookup
  at once, and an exhausted list returns no error. `artifactStorageFragment`,
  `resolveArtifactSecret` (projects the winning candidate into the controller's own namespace as
  `<controller>-artifacts`, owned by the `Controller`, mounted at
  `/run/secrets/varroa-artifacts`; its third return is an error, not a `found` bool, and
  `handleProvisioning` fails the tick on it rather than discarding it), and
  `reconcileArtifactSecretRoll` (the Connected-phase rotation check) all call this pair, so a
  read fault never hands a cell the pool credential while its own Secret exists, and a rotation
  rolls a pool-scoped cell the same way it rolls a per-cell one. A read fault in
  `artifactStorageFragment` travels up wrapped in the sentinel `errArtifactSecretRead`, through
  `applyArtifactStorage` and `resolveBundleForController`, so `handleProvisioning` marks
  `ReconcileBlocked`/`ArtifactSecretFailed` before the cell configuration gate runs; the Connected
  path instead sets the read-failed `ArtifactStorage` condition, keeps the last pushed desired
  state, and does not report `BundleFailed`; and the plugin-roll check
  (`checkPluginRollFailed`) gives no verdict on that fault, because a verdict computed without
  the bundle's own plugin pins would be wrong. When no candidate exists it is a
  no-op: no strip, no merge, `mat.JenkinsYAML` passes through unchanged. When it does emit,
  `stripArtifactManager`
  first removes any bundle- or profile-overlay-authored key from the same three subtrees
  (`artifactManagerStripPaths`), `bundle.MergeJenkinsYAML` merges the fragment, then
  `injectArtifactCredential` appends a system-scoped `${readFile:...}` credential reference (never
  a secret value) to the global credentials domain.
  **STS mode** (`artifactsts.go`, on when `SetArtifactSTS` receives a config with an issuer Secret;
  exclusive with `VARROA_ARTIFACT_SECRET`, enforced at boot by `cmd/operator/artifact_sts.go`)
  replaces the lookup for cells only: `artifactStorageFragment` calls `artifactSTSFragment`
  (fragment with `credentialsId: ""` and `disableSessionToken: false`, no injected credential),
  `resolveArtifactSecret` calls `resolveArtifactSTSSecret` (returns an empty data hash, so the
  desired-state hash never carries a credential), and `reconcileArtifactSecretRoll` calls
  `reconcileArtifactSTSRoll`. `ensureArtifactSTSCredential` calls STS `AssumeRole` through the
  `artifactIssuer` seam (`stsArtifactIssuer`, aws-sdk-go-v2) with a three-statement session
  policy for the cell's own prefix, and writes `credentials` (an AWS shared credentials file),
  `issued-at`, and `expiration` into `<cell>-artifacts`. A copy is fresh until half of its own
  lifetime. `renewArtifactCredential` runs at the top of the Running/Connected block of
  `reconcileController`, before the dispatch, so the condition written on the same tick sees the
  result. The Jenkins container gets `-Daws.sharedCredentialsFile=...` in `JAVA_OPTS`
  (`StatefulSetSpec.ArtifactSharedCredentials`), never an `AWS_*` env var; the kubelet updates the
  file and the SDK rereads it, so a renewal never rolls the pod. An issue error is held in the
  in-memory `artifactIssueFailures` (message and time; no STS retry inside
  `artifactIssueRetryInterval`, one minute); while it is held the fragment stays and `ArtifactStorage`
  reads `False/IssueFailed`, which turns `CellReady` false without a roll. `reconcileArtifactSTSRoll`
  rolls only a cell whose copy is already fresh (the renewal earlier on the tick wrote it), or a
  credential-less cell would roll every tick. An issue that returns a credential stale at once
  counts as a failure. A first-issue error
  on a provisioning tick blocks with `ArtifactSecretFailed`. No failure ever falls back to a
  static Secret. `ConditionArtifactStorage` is defined here
  rather than alongside the other `ControllerConditionType` constants in `api/v1alpha1/types.go`,
  because this batch's file scope excludes that package; flagged for the coordinator to relocate
  it.
- `pipeline_templates_preview.go`'s `pipelineTemplateSummaries` lists the
  templates a composed bundle delivers (name, version, display name,
  description), reading only the composer's own `PipelineTemplatesYAML`
  output — an unreadable or blank value lists nothing, since the composer
  already reported any parse error on the bundle itself. Called from
  `config_crud.go`'s preview/validate NATS replies onto
  `bus.BundleComposePreview.PipelineTemplates`.
- **Team bundle content reply.** `config_crud_teambundle.go` answers
  `bundles.teamcontent` for a member-placed Team's pipeline read. It returns the
  bundle, four content keys (never `jenkins.yaml`), and `held`. `held` comes from
  `TeamHeld` (`team_held.go`), which checks `status.cell.tenantWarnings` of every member
  cell in `v1alpha1.PlacementCells` order through `v1alpha1.CellHoldsTenant`.
- **`applyRuntimeUpdateSite` is the only writer of `jenkins.updateCenter`**
  (`updatesite.go`). It runs in `resolveBundleForController` after `ResolveVars`
  and replaces any bundle value through `bundle.InjectUpdateSites`. The site URL
  follows the precedence in `resolveRuntimeUpdateSiteURL`: explicit
  `ProvisioningDefaults.spec.pluginUpdateCenterURL`, then the in-cluster update
  center (`ucBaseURL`), then `jenkinsUpstreamURL`, then an empty list. Every URL
  from the update center or the upstream ends in `/update-center.json`. Jenkins
  reads tool-installer metadata only from a site URL with that name; do not use
  `update-center.actual.json`.
- **plugins-init update-center env.** The env holds `JENKINS_UC`, `JENKINS_UC_EXPERIMENTAL` (same value as `JENKINS_UC`), `JENKINS_UC_DOWNLOAD_URL`, and `JENKINS_PLUGIN_INFO`. `resolvePluginUpdateCenterURLs` resolves each with three tiers: explicit `ProvisioningDefaults`, the in-cluster update center when Ready, then empty. The Jenkins image sets `JENKINS_UC_EXPERIMENTAL` to upstream, and the tool reads plugin info for every exact pin.
- **plugins-init reports on its termination log.** `pluginsInitScript`
  (`clientset_client.go`) writes one marker line that `plugininit_report.go`
  parses, so the format is a contract:
  `varroa:download-url-not-found url= plugin=name:version status=` with exit 42
  (a pre-flight one-byte fetch of the first pin answered 404 or 410; no retry),
  and `varroa:archive-fallback count= plugin= url=` on any other exit (the tool
  fell back to `https://archives.jenkins.io/`, which no flag disables). A clean
  run leaves the log empty. The pre-flight `curl` and the tool's `tee` pipeline
  (one subshell that exits with the tool's status) run in the background under
  `wait`, like every long command of the script. A failed tool run keeps no plugin, so each retry
  downloads the full set again. `pluginInitUCReport` reads `State.Terminated`,
  or `LastTerminationState` while the container runs again after a restart;
  `reconcileProvisioningPluginInitFailure` and `checkPluginRollFailed` both call
  it. `pluginInitFailureMessage` reads `LastTerminationState` the same way.
- **`UpdateCenterFallback` has several writers, and each clears only its own
  reasons.** The update-center gate writes `UpdateCenterAvailable`, except for an
  explicit `ProvisioningDefaults` download URL (`ucGateResult.DownloadURLExplicit`),
  which never touches the in-cluster update center: there it only clears its own
  stale `True` verdict (`UpdateCenterUnavailable`). For that URL,
  `handleProvisioning` runs `Reconciler.downloadProbe` (`ucprobe.go`): a ranged GET
  of the first pin (never HEAD, the update center answers 405), five-second
  timeout, ten-minute cache per URL. Only 404/410 give a verdict
  (`DownloadURLNotFound`); a network error gives none, and the check never blocks.
  The plugins-init report writes `DownloadURLNotFound`/`PluginArchiveFallback` and
  clears them with `PluginDownloadsVerified`. Tests stub `downloadProbe` in
  `newTestReconciler`; a new constructor path must stub it too.
- Version profiles: `resolveProfileForCr` → `ResolveProfile` (`versionresolve.go`)
  matches **exact version → LTS line (`lineKey`, `major.minor`) →
  `pluginlock.Baseline()`** (embedded fallback, no profile). `resolveCoreSet` /
  `coreSetForCr` (`controller_controller.go:822`/`850`) turn the matched profile (or
  baseline) into `[]pluginlock.PluginEntry`, fed into `managedPluginLines` for both
  provisioning-time `plugins.txt` and connected-phase drift detection — these two call
  sites must stay in sync or plugin drift will fire spuriously. On a cell, `managedPluginLines` drops each `cellExcludedPlugins` entry (today `job-dsl`) from the core set and from the bundle or `pluginSpec` entries. The pin gates (`CheckPluginPins`, `pluginVersionConflict`) judge the same reduced set (`cellInstalledSet`).
- `ResourceClient` (`controller_controller.go`, ~40 methods) is the operator's deep-op
  K8s surface (StatefulSet/Service/Ingress/Secret/ConfigMap/pod/wake operations, including
  `DeleteControllerPodGrace` for a short-grace, UID-fenced pod delete, plus the
  named special-semantics methods `PatchControllerStatus`/`ApplyControllerSpecSSA`/
  `SetHibernated`/`SetHibernatedAt`/`ClearUserPassword`); `ClientsetClient` (`clientset_client.go`)
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
- `CommandCRUD.HandleList` reads Controllers through `ListSource`
  (`controller_list_source.go`). The operator wires the manager cache
  (`NewCacheControllerListSource(mgr.GetCache())`). The cache syncs on every
  replica, because the Controller reconciler is not leader-elected. Do not list
  Controllers from the apiserver in a bus list handler.
  `CreateStatefulSet` takes a fence that runs before a create or a scale-up from 0.
  `CreateService`/`CreateIngress` are create-or-update.
  `EnsureWakeEndpointSlice`/`DeleteWakeEndpointSlice` own the hibernation traffic flip;
  slice failures are warning-only and confirmed deletes are memoized for retry safety.
  The slice name is `<full-service-name>-wake`; retaining the full Service name as the
  prefix is required for ingress-nginx EndpointSlice association.
  Cleanup still runs when wake serving is disabled and on provisioning timeout, so a
  stale flip cannot strand Failed/rollback controllers on the operator wake port.
  `reconcileIngress` (`controller_controller.go:3268`) runs `CreateIngress` every tick
  for `Running`/`Connected` so Ingress spec/TLS changes converge without a
  re-provision. It also writes `status.endpoint` through `v1alpha1.ExternalURL` from
  the host this operator resolved, and clears it for a headless controller or when no
  host resolves. `controllerStatusPatch` always sends `endpoint`, so an empty value clears it.
- **Headless controllers** — `v1alpha1.IsHeadless(cr)` is the only decision point; no
  other read of `Spec.Headless`/`Spec.Cell.Enabled` may drive this behavior.
  `desiredstate.go` sets `StatefulSetSpec.Headless` from it, and `buildStatefulSet`
  (`clientset_client.go`) appends `VARROA_HEADLESS=true` to the jenkins container's env
  only when true, so a non-headless pod template stays byte-identical.
  `reconcileHeadlessRoll` (`controller_controller.go`), called from the `Connected`
  branch, reads the live jenkins env through `GetStatefulSetContainerEnv` (a
  `ResourceClient` method that takes the container name) and transitions to `Provisioning` on a
  difference from the desired value; it skips the check when no StatefulSet exists yet
  or the read errors, and treats an absent variable as `false`. `reconcileIngress`
  deletes the controller's Ingress and sets `ConditionNoExternalURL`/`Headless` instead
  of building one. The bundle-variable host branch and the location-URL injection both
  gate on `!IsHeadless(cr)`: a headless controller keeps `varroa_controller_endpoint`
  but gets no external URL or path prefix. Its injected `location.url` is built only by
  `v1alpha1.HeadlessLocationURL` from the dashboard origin (`varroaBaseURL`) when that origin
  is set, and is the in-cluster endpoint otherwise. A headless pod also gets
  `KUBERNETES_JENKINS_URL` set to `inClusterEndpoint(cr)` so clouds without `jenkinsUrl`
  keep agents in-cluster. `reconcileSharedHostMismatch`
  clears its condition for a headless controller, since it renders no Ingress to
  mismatch. `internal/api`'s `handleControllerDiff` mirrors all of this so a headless
  controller's diff matches what provisioning applies.
- `SetProvisioningDefaults` and `RootDomain` share `provisioningDefaultsMu`; the
  every-replica defaults refresh feeds both reconcile policy and wake-server host
  resolution without racing HTTP requests.
- `PatchControllerStatus` (`clientset_client.go`) sends a JSON **merge patch** that
  carries the caller's `resourceVersion` as a precondition, and returns the version
  after the patch. An empty version is an error. `controllerStatusPatch` builds its
  body, and the test client applies the same body to its store through
  `Fake.PatchObjectStatusAt`, which enforces the precondition. Fields only added to the body when
  non-empty (`status.X != "" / != nil` guards — e.g. `desiredStateHash`,
  `AppliedBundleHash`, `FirstConnectedAt`, `WakeToken`, `LastDesiredPushAt`) are
  **sticky**: a merge patch that omits a key can never clear it server-side. Fields
  always included (even zero-valued) — `phase`, `conditions`, `overlayWarnings`,
  `pendingRestart`, `pendingPluginRoll`, `approvedPluginRollChecksum`,
  `pendingItemDeletions` (an empty list when nil), `restartDrain`, `liveDrift`,
  `rollout`, `lastReconcileError`, `lastReconcileErrorAt`, `pluginInventory` — are the
  only ones that can be cleared. When adding a status field, decide up front whether
  it must ever be cleared; if so it must be unconditional in `controllerStatusPatch`,
  not behind a non-empty guard.
- **`ItemsApplyFailed` tenant warnings** come from the desired-state result drain
  (`controller_controller.go`). `itemsApplyFailureWarnings` (`cell_items_apply_failure.go`)
  maps each `DesiredStateResult.item_failures` entry to its tenant and writes one warning per
  tenant. A result with refused items replaces every `ItemsApplyFailed` entry. A successful
  items section clears them all. The drain applies this only when the result answers the
  tracked desired-state hash. The same drain also advances `status.cell.appliedTenants` on a
  result with `item_failures`. Each tenant that owns a refused item keeps its previous membership.
  A pod change drops each tenant with an `ItemsApplyFailed` entry from `appliedTenants`.
- **`status.cell.tenants` has exactly one writer: `PatchControllerCellStatus`**
  (`clientset_client.go`). It sends a narrow merge patch carrying only
  `status.cell.tenants`, never the whole `status.cell` object, and only the
  placement pass (`TeamReconciler`, via `cells.Place`) calls it. The blind
  end-of-reconcile `PatchControllerStatus` patch above deliberately omits
  `tenants` from its own `cell` sub-map — it carries only `warm`,
  `appliedTenants`, `evacuate`, `unreadySince`, `rolledAt`, and
  `tenantWarnings` — or a stale in-memory tenant list would clobber a
  placement change the two writers raced on.
- **`status.cell.pendingTenants` (the placement claim) is the second narrow
  key on `status.cell`**, written only by `cells.WriteCellClaim` /
  `ReleaseCellClaim` (`cells/claim.go`) from the same placement pass. They go
  through `crdstore.PatchStatus[Controller]` with a **hand-built map** naming
  `cell.pendingTenants` alone — never a typed status object, which would
  marshal the whole status body. A claim expires after `cells.CellClaimTTL`
  (10 minutes) and each write drops every expired entry. The BFF projection
  (`CellStatusJSON`) carries neither this key nor `rehydration`.
- **`ConditionReconcileBlocked`** (C3, `markReconcileBlocked` helper near
  `persistStatusDiagnostics`) is the standing pattern for surfacing a reconcile pass
  that hit an unresolved error: sets the condition `True` + `Reason` (one of 20
  site-specific reasons in `api/v1alpha1/types.go`, including
  `ArtifactSecretFailed` for a failed artifact Secret lookup or projection, and
  `CellSecretsFailed` for a pool cell whose pool Secret is absent, empty,
  incomplete, or unreadable),
  stamps
  `LastReconcileError`/`LastReconcileErrorAt`, records the
  `varroa.controller.reconcile.blocked` gauge at `1`. A guarded branch in
  `reconcileController`'s metrics `defer` clears it back to `False` on the next
  successful pass, skipping the clear when `oldPhase == ControllerPhaseFailed` (that
  pass's success reflects the *previous* tick, not resolution) and no-oping when the
  condition was never set (so healthy controllers get zero status noise). New
  reconcile-blocking call sites should call `markReconcileBlocked` rather than
  inventing a parallel mechanism.
- **Reconciler metrics** live in `controllerMetrics`, a field of `Reconciler`.
  `NewReconciler` takes a `metric.MeterProvider` as its first parameter and
  builds them on its `"varroa-operator"` meter. `cmd/operator` passes
  `otel.GetMeterProvider()`. A new reconciler instrument goes into
  `newControllerMetrics`, never into a package variable. A test that reads a
  metric builds the reconciler with `newTestReconcilerWithMeterProvider` and a
  provider from `newTestMeterProvider`. A hand-built `&Reconciler{}` that
  reaches a record site sets `metrics: newControllerMetrics(noop.NewMeterProvider())`.
  No test replaces the global provider. The repo-wide guard in
  `internal/telemetry/meter_guard_test.go` enforces this.
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
  path); `Cell` (field 19) is `isCellController(cr)`, so the mite can tell a cell
  with no tenant from a controller that is not a cell. `cmd.DesiredStateHash =
  computeDesiredStateHash(cmd)` hashes the content hash
  (`computeDesiredContentHash`: `JcascYaml`, `RbacYaml`, `ItemsYaml`) and the
  `tenant_homes` hash; `Cell` and `rehydration` stay outside the hash. Both
  hashes frame each part with `writeFramedPart` (`name:length:body`), so content
  that moves between sections changes the hash. Do not change the content-hash
  encoding: stored `pendingRestart.contentHash` values hold it, and
  `TestComputeDesiredContentHash_GoldenValue` pins it. A change of the composite
  encoding costs one push for each controller; in `manual` mode
  `manualOperatorOwnedChange` sends it without approval when the content hash
  matches.
- **Every imperative result leaves a trace** (`imperative_outcome.go`) —
  `handleConnected`'s result drain calls `recordImperativeOutcome` for each
  result without a desired-state payload, before it writes
  `LastImperativeResult`. It counts `varroa.imperative_command.results`
  (`kind` from `CommandResult.command_type`, `outcome` `success`, `failure`,
  or `deferred`). `imperativeOutcomeEvent` builds at most one activity
  event per result, with `CommandResult.actor` in `Actor` and the HTTP
  status in the message. Every `failure` publishes `command.failed`. A
  success publishes `command.succeeded` only for a person-sent run command
  (`personSentRunCommand`: a cancel, input answer, or replay whose actor is
  not empty and not `recordstore.LedgerActionRecorder`). Every other
  success, a trigger success included, and a deferred `SAFE_RESTART` drain
  publish no event: the activity stream holds audit-worthy events only.
  These cell-keyed rows are the operator audit record. A tenant reader
  sees a refused run command on the run detail instead, through the
  gateway's `run.command_refused` or `run.trigger_failed` record.
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
- `SendImperativeCommand` (`imperative_send.go`) is the reconciler's public
  door onto the private `miteTransport` field for a single imperative
  command, mirroring `MintMiteToken`'s same role for tokens; the operator's
  `varroa.ledger.actions.<cluster>` subscriber (`cmd/operator/ledger_action.go`)
  is its first caller. That subscriber dispatches four action kinds —
  `cancel_run`, `retry_run`, `wake_cell`, and `trigger_run` — filtered first
  to actions naming this operator's own cluster. `trigger_run` calls
  `WakeHibernatedController` before it sends the command, unconditionally,
  the same call `wake_cell` makes on its own: the call is a no-op for a
  controller that is not hibernated, so a `trigger_run` action needs no
  separate power-state read before it reaches a running controller.
- Other controller-runtime / ticker reconcilers in this package:
  - `ucservability.go` — `postServability` and `getUCInventory` are the only
    operator clients of the update center's servability and inventory routes.
    `UpdateCenterReconciler` and `ProfileCandidateReconciler` both call them.
    The candidate posts only its unstored pins to the servability route.
    `storedForCore` is the one stored check for a latest pin. Both reconcilers
    post `core` for a latest entry. `requestingCore` reads `deployedCore`.
    A two-segment core is a fixed weekly release on every channel.
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
    `ucSyncDigest` and `tenantHomesHash` encode their inputs with `encoding/json`, never with a joined separator.
    `ReconcileComposedBundle`'s
    drift loop has OCI branches parallel to the git branches: resolves creds via
    `OCIAuthFromSecret`, resolves manifest digest via `oci.RegistryStore.Resolve`,
    populates `resolvedOCIAuth[i]`, and `hasNewOCIInputs` gates the skip path.
    `Composer.Compose` receives `resolvedOCIAuth` as its 5th argument.
    Composed content above `maxComposedContentBytes` (900 KiB) is refused, and the last-good `contentRef` stays.
    `catalogFetchTimeout` (2m) bounds the remote fetch work (git and OCI) of
    one source sync and of one bundle's drift checks plus compose, because the
    ticker runs them in series. Status writes keep the ticker ctx, never the
    timed one, so a timed-out sync still records its error. An
    `itemRef` that no longer resolves records the `ItemMissingRevision`
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
    An incomplete closure is not a verdict. It sets `status.valid = false`,
    stores no content, and sets `ClosureComplete=False` with each unread
    `name@version` (`ucclosurecomplete.go`). A lock-sourced member never counts
    as unknown.
    A lock pin that `ucmeta.IsLatest` matches sets no floor and names no version.
  - `GroupReconciler`, `UserReconciler`, `TeamReconciler` (`group_controller.go`,
    `user_controller.go`, `team_controller.go`) — Group/User/Team CRDs; Team also
    provisions/owns a namespace. A `Team` with `spec.pipelines` set is a
    pipelines tenant: `TeamReconciler` calls `cells.Place` (below) once per
    tick to bind, move, or tear the tenant down from a shared cell.
  - `BroodOperationReconciler` / `BroodScheduleReconciler`
    (`broodoperation_controller.go`, `broodschedule_controller.go`) — fleet-wide
    operation fan-out (dispatch, per-target tracking, timeout) and CronJob-backed
    scheduled brood operations.
- **`cells/` package + the cell reconcilers** — a cell is a `Controller` with
  `spec.cell.enabled`: a disposable Jenkins JVM holding no durable state,
  hosting several `Team`s as top-level folders (design D4/D6). Two
  reconcilers drive it:
  - `TeamReconciler` (`team_controller.go`) is the placement reconciler. For
    every pipelines `Team` it calls `cells.Place` (`cells/placement.go`) —
    pure decision logic, no Kubernetes write of its own: it reads a
    caller-gathered `[]CellCandidate` snapshot, a caller-gathered
    `[]CellPoolSummary` (`Inputs.Pools`, listed once per tick beside the cell
    list), and the `Team`'s own status,
    and returns an `Outcome` plus a mutated `TeamStatus` for the caller to
    apply. A tenant's **resolved pool** is `spec.pipelines.cellPool` when set,
    else the pool named by `spec.pipelines.profile`; gate 4 refuses a named
    pool that is absent or serves another profile with `CellPoolNotFound`,
    while an uncreated **default** pool stays `AwaitingCell` because the pool
    reconciler creates it within one tick. `cells/members.go`'s
    `desiredMembers` answers the whole member list a tenant should hold:
    one cell for each of `spec.pipelines.members` active slots and each of
    `spec.pipelines.standby` warm members, with the scalar placement
    describing slot 0. A slot whose holder is gone takes the newest-applied
    standby, and the announce carries the whole list before the status
    write. The reasons `SlotPending`, `StandbyPending`, and
    `MembersApplied` report the member set. `TeamReconciler` then applies the
    `Outcome` in a fixed order: **the claim first** (`cells.WriteCellClaim` on
    the selected cell, and a failed claim ends the tick with no fence
    written), then `Team.status.placement.cluster` (and the rest of
    `TeamStatus`), then `PatchControllerCellStatus` (above) on the
    bound/vacated cells, then `cells.ReleaseCellClaim` **only for a tenant
    patch that succeeded**. `Place` also names the claim on the `moveNone`
    path while the bound cell's tenant list does not carry the tenant, so a
    refused tenant patch refreshes the claim every tick instead of letting it
    expire. The claim exists
    because the pool drain reads the fence set **before** it re-reads a free
    cell: a fence written after that listing is invisible to the drain, and
    without a write on the cell itself the drain's `DeleteIfUnchanged`
    precondition still passes and deletes the cell the tenant is arriving on.
    **The placement pass never creates a cell** — only the pool reconciler
    does. A voluntary move completes only once
    `Controller.status.cell.appliedTenants` no longer holds the tenant; the
    `Team` finalizer holds through teardown until the schedule purge
    succeeds or `maxPurgeAttempts` (10) is spent (a teardown purge sets
    `PurgeRequest.TeamDeleted` from the `Team`'s `DeletionTimestamp`, and
    the recorder then retires the `Team`'s uid; a dropped `spec.pipelines`
    leaves it false), then a bounded
    `teardownBoundedWait` (10 minutes) release, in both cases raising a
    warning event before releasing — warnings order by tenant then reason.
    Teardown stage two also holds on `cells.Inputs.AgentPodsPending`, inside
    the same bounded wait. The hold lives in `cells.teardown`, not in a
    `Reconcile` finalizer hold, because the finishing tick clears
    `status.pipelines`, the only copy of the agent namespace once
    `spec.pipelines` is gone.
  - **Tenant agent pod reaper** (`team_agent_pods.go`). The operator deletes
    a tenant agent pod that no live JVM refreshes. A Running pod can hold a
    node that a resume adopts late, so every age that can delete one
    outlives `TenantAgentAdoptBound` (`cell_jcasc.go`:
    `recordstore.SilentCellWindow + recordstore.ResumeAdoptBound`, 36
    minutes). The kubernetes plugin's own garbage collection
    (`tenantAgentGCTimeoutSeconds = 2400`, the bound plus 4 minutes, on each
    tenant cloud) covers a Running pod after a cell roll, a slot move, and a
    promotion. The operator acts first on a pod that is not Running
    (`tenantAgentPendingReapAfter`, 3 minutes): no node body ran on it, and
    this covers the case where the new cell pod is itself Pending. The
    operator also covers a Running pod no live JVM holds
    (`tenantAgentRunningReapAfter`, the plugin timeout plus 5 minutes).
    `TestTenantAgentTimeoutsCoverTheResumeWindow` pins this order.
    At a graceful cell stop, the cell's plugin terminates its idle cloud agents
    (`VarroaCellAgentRelease`). After a hard kill, the operator's
    `ReleaseInstanceAgentPods` deletes the idle pods of a confirmed gone instance.
    The garbage collection and the operator's long age then cover only busy pods.
    `orphanTenantAgentPods` is pure: a candidate runs as
    `varroa-agent-<team>`, carries the `kubernetes.jenkins.io/controller`
    label, and carries a `kubernetes.jenkins.io/last-refresh` annotation that
    parses as epoch milliseconds. It goes when older than
    `agentPodReapAfter` (by pod phase; `Unknown` counts as Running) or at
    once in teardown. An API cancel deletes no agent pod. A run that the
    recorder ends releases its own pods: `ReleaseRunAgentPods` (called for the
    `release_agents` action) deletes only the named pods that run as the
    tenant ServiceAccount and carry the controller label, each with its UID.
    `ReleaseInstanceAgentPods` (called for `release_instance_agents`) deletes the
    stale pods of a confirmed gone cell instance, in any operator replica. Its
    filter (`staleInstanceAgentPods`) keeps a pod named in `keep` (the nodes a
    resume can adopt), a pod with a last refresh after `refreshedBefore`, a pod
    refreshed within `tenantAgentPendingReapAfter`, and a pod that is missing the
    annotation or the label. Each delete carries the listed UID. `syncPipelinesChildren` runs the steady-state pass,
    rate-limited by `reapTenantAgentPodsDue` to one list per Team per
    `tenantAgentReapInterval` (1 minute), and never fails the tick on it.
    `Reconcile` runs the teardown pass before `cells.Place`, on the same
    tick as, and before, the tenant ServiceAccount delete, and feeds the
    remaining count into `AgentPodsPending`. The client seam is
    `tenantAgentPodReaper` (`ListTenantAgentPods` with selector
    `jenkins=slave`, `DeletePodWithUID` with a UID precondition), checked
    by type assertion like `tenantServiceWriter`, so `ResourceClient` does
    not grow. RBAC is unchanged: the agent Role already grants pods
    list/delete/patch, and the operator already holds pods list/delete in
    every managed namespace.
  - `cells.WarmPoolReconciler` (`cells/warmpool.go`) is a plain
    `Reconcile(ctx) error` ticker reconciler (not controller-runtime,
    interval 60s in `cmd/operator/main.go`), not a per-`Team` reconciler. It
    sizes every `CellPool` from **that pool's own spec**, never from the chart
    values: `WarmPoolConfig` now only seeds a pool the operator creates, and
    `Enabled` gates auto-creation alone — a hand-applied `CellPool` is
    reconciled even with auto-creation off. It is the only creator of a cell
    and the only writer of `CellPool.status`; it writes no `Controller`
    status, so it never races a status writer above. Each pass, per pool:
    resolve `spec.profileRef` (a missing profile sets `PoolReady=False` with
    reason `ProfileMissing` and stops, creating and deleting nothing); ensure
    the pool's own confidential store (`ensurePoolSecrets`, the Secret
    `varroa-cell-secrets-<pool>` in the operator namespace — a different object
    from the pool's artifact Secret `<base>-<pool>`); adopt
    unlabelled cells of the profile **only when the pool is that profile's
    default pool** (its name equals its own `profileRef`); list cells by
    `varroa.dev/cell-pool` and keep only those whose
    `varroa.dev/cell-profile` matches (the second test keeps a stale label out
    of a new pool of the same name); compute **one** target
    (`desiredFree = max(warm, ceil(deficit / maxTenantsPerCell))`, where
    `deficit` sums each tenant's unmet member demand, minus the owned cells
    that can take a member of **that** tenant — a cell holds at most one
    member of a tenant, so a cell that already holds one offsets nothing,
    whatever spare capacity it has, and a cell with spare capacity offsets
    one member of each other tenant; a free cell offsets nothing either,
    because `len(free)` counts it already) and either fill or drain, never both in a tick, so
    the pass cannot oscillate; re-stamp `spec.version`/`resources`/
    `podOverrides`/`miteSpec.resources` on free cells only (`applyCellTemplate`
    owns `miteSpec.resources` alone and keeps `miteSpec.image` and
    `imagePullPolicy`; `equalCellShape` compares all four); write status.
    `seededPool` deep-copies `WarmPoolConfig.CellTemplate` into a pool it
    creates, and only a create writes it. A wanted fill first runs
    `fillGateReason`, in this order: `ClaimGate != "1"` is
    `RecordStoreUnsupported`; a blob URL that is not S3 is
    `BlobTierUnsupported`; an empty `ArtifactSecretBase` (with no
    `ArtifactIssuerSecret`), a nil `Secrets`, or an absent, empty, or failed
    `Secrets.GetSecret` read of `artifactGateSecret(pool)` (the issuer Secret in
    STS mode, else `<base>-<pool>`) is
    `ArtifactSecretMissing` (a non-`NotFound` error is logged and counted the
    same); a nil `Plugins` or a false
    `Plugins.VersionPinsPlugin(ctx, version, "artifact-manager-s3")` is
    `ArtifactPluginMissing`; a `secretsReady` of false is `SecretsMissing`, the
    last row. A non-empty reason skips the fill only. Drain and
    restamp still run, and the reason feeds the status write, where it
    outranks `CellCapReached`. **`SecretsMissing` is the one reason a pass with
    no fill also reports**: `fillGateReason` runs only for a wanted fill, so
    `reconcilePool`'s own reason switch carries a second `!secretsReady` case
    ahead of `CellCapReached`. The store is a property of the pool, not of a
    fill. `Secrets` (`cells.SecretReader`), `SecretWriter`
    (`cells.SecretWriter`), and `Plugins`
    (`cells.PluginPinReader`) are one-method seams, because `cells` cannot
    import `internal/controller`. `main.go` sets `Secrets` and `SecretWriter`
    to `clientsetClient`, and `Plugins` to the `Reconciler` itself.
    `ensurePoolSecrets` mints all four keys for an absent Secret and for one
    with empty data, adds only the missing keys to a Secret that holds
    `master.key`, and writes nothing for a Secret with another key and no
    `master.key`. `internal/controller/cells/confidentialstore.go` owns the
    byte format; the operator never rotates the store, so a rotation is a hand
    edit plus a hand roll of every cell of the pool. `Reconciler.VersionPinsPlugin` resolves the
    plugin set for a bare version the way a cell of that version would (exact
    profile, then LTS line, then the embedded baseline), never through the
    pool's own `profileRef` by name. `main.go` also seeds
    `WarmPoolConfig.ClaimGate`, `BlobTierS3` (`controller.BlobstoreIsS3`,
    exported because `cells` cannot import the URL parser), and
    `ArtifactSecretBase` from the same three boot-time values the `Reconciler`
    reads. `placedSlots`
    deliberately excludes free cells — `len(free)` already counts them, and
    double-counting would stall a `warm: 0` pool. A created cell is named
    `cell-<pool>-<ordinal>` where `<ordinal>` is scanned against **every
    `Controller` name** in the operator namespace, not the pool's label list,
    so two pools whose names share a prefix never collide. A `CellPool`
    carries the finalizer `varroa.dev/cellpool-drain`: a delete drains the
    pool's free cells, strips no label, and then releases the finalizer, so a
    placed cell lives on as a cell with no effective owner.
  - **Pool cell pod label.** `buildStatefulSetSpec` sets
    `StatefulSetSpec.CellPool` through `cellPoolLabel` (the Controller's
    `varroa.dev/cell-pool` label, and empty for a non-cell), and
    `CreateStatefulSet` writes it on the pod template **after** the
    `podOverrides` merge, the `resourceOverlay` merge, and normalization, so
    no `podLabels` entry or overlay can change it. The selector never carries
    it (immutable). No roll detector watches the label: a running cell gets
    it at its next provisioning pass. The default template's anti-affinity
    and `kubectl delete pod -l varroa.dev/cell-pool=<pool>` both select on it. **Both drain
    paths delete through `deleteFreeCell`, never from the pass's own free
    snapshot**: a fresh read with no tenant, no live claim on that fresh read
    (`status.cell.pendingTenants`), no fence in the pass's fence snapshot, no
    fence in a **fresh** `fencedCells` read taken after the cell read, and a
    `DeleteIfUnchanged` precondition on the uid and resourceVersion the fresh
    read observed. A `Conflict` is a skip, not a failure. The fresh fence read
    is what covers a tenant whose cell patch keeps failing past
    `CellClaimTTL`.
  - **Effective owner** (`cells/cellpool.go`) is the one rule every pool test
    reads: the `CellPool` a cell's `varroa.dev/cell-pool` label names, **and
    only while that pool exists**. A cell with no label, or with a label
    naming a deleted pool, has none. `resolveCellCapacity` chains the cell's
    own `spec.cell.maxTenants`, then the effective owner's
    `maxTenantsPerCell`, then the chart value — never the minimum of two.
    `cellUnreadyGrace` (`cell_readiness.go`) follows the same chain for the
    roll and evacuate timers. Reading the raw label instead of the effective
    owner reintroduces the drift the `effectiveBundleRef` rule already
    forbids.
  - The Controller reconciler's own cell-specific logic lives beside it in
    this package, one file per concern: `cell_script_approval.go`
    (`applyPoolSignatures` renders `CellPool.spec.approvedSignatures` into
    `security.scriptApproval.approvedSignatures` as a sorted, deduplicated
    union with the bundle's entries. The key is always set, even when empty;
    `poolSignatures` is the shared pool read),
    `cell_script_approval_status.go` (`recordPendingSignatures` copies the
    snapshot's refused signatures into `status.cell.pendingSignatures`, capped
    at 25 signatures and 3 requesters each, minus the pool-approved ones; the
    status patch writes both pending keys unconditionally),
    `cell_pool_libraries.go`
    (`applyPoolLibraries` runs in `resolveBundleForController` after the
    tenant template loop and `ResolveVars`. It renders `CellPool.spec.libraries`
    into `unclassified.globalLibraries.libraries` (list-aware: bundle entries
    with a pool name are dropped, pool entries appended, the key always set).
    The git login Secret (label `varroa.dev/pool-library-credential`) goes only
    to `MaterializedBundle.CellPoolLibraryCredentials`, injected into the mite
    command's JcascYaml by `buildDesiredStateCommand`. Faults set the
    `PoolLibraries` condition; a faulty library is held, not rendered),
    `cell_pool_library_egress.go`
    (`poolLibraryEgressPeers` turns each library remote into resolved
    address and port peers on every tick. An IP host needs no lookup. A
    hostname resolves through the replaceable `poolLibraryLookupHost`. A
    failed lookup skips only that library. `cellPoolLibraries` is the shared
    pool-library read),
    `cell_pool_mail.go`
    (`applyPoolMail` renders `CellPool.spec.mail` into
    `MaterializedBundle.CellPoolMail`: the `unclassified` keys of the
    `mailer` and `email-ext` plugins and `location.adminAddress`. The
    `mailer` login (Secret label `varroa.dev/pool-mail-credential`) sits
    inline in the mailer value, so the field reaches only
    `buildDesiredStateCommand`, through `injectPoolMail`, and no ConfigMap
    writer reads it. The `email-ext` entry carries no `credentialsId`.
    A missing `mailer` pin, a missing or malformed login, or a read failure
    renders nothing. An unpinned `email-ext` keeps the mailer and records a
    fault. Faults set the `PoolMail` condition),
    `cell_pool_gitea.go`
    (`applyPoolGiteaServers` runs after `applyPoolLibraries`. It renders
    `CellPool.spec.giteaServers` into `unclassified.giteaServers.servers`
    with `manageHooks: false` and no credential. The key is always set when
    the `gitea` plugin is pinned, as an empty list when no server remains,
    and list-aware: a bundle entry with the same normalized `serverUrl` is
    dropped. An unpinned plugin leaves the JCasC unchanged, because an
    unknown key fails the reload. Faults set the `PoolGiteaServers`
    condition; a server with a `${` token is held),
    `cell_readiness.go`
    (`isCellController`; `cellConfigGateReason` checks, in fixed order, the
    record store, the s3 blob tier, and C3's `ArtifactStorage=External`
    condition before a cell may report `CellReady`; past the gates the base
    reason is `MiteUnreachable` for `Running` + `MiteConnected=False/StreamLost`,
    else `NotProvisioned` below `Connected`; `evaluateCellRoll`
    implements design D6's roll-then-evacuate rule off
    `UnreadySince`/`cellsConfig.UnreadyGracePeriod` — `CellNotReady`
    applies when the pass keeps a placement on a cell reporting
    `CellReady=False` with no `evacuate`. `observeCellPod` reads the pod
    only on a tick where the rule on `UnreadySince` alone would act, and
    `observeInit` turns it into an `initObservation`. While an init
    container runs, or the next one waits in `PodInitializing`/
    `ContainerCreating` after an earlier one exited 0, with no init
    restart, failure, or back-off, inside
    `cellInitRollCap` (30m) of `status.startTime`, the roll and a new
    `evacuate` hold, and the `CellReady` message names the container.
    After init, the roll clock starts at the later of `UnreadySince` and
    the last init finish. `evacuate` stays set until recovery. A
    terminating pod is never deleted again. `rollCellPod` deletes a pod
    whose main containers have not started through
    `DeleteControllerPodGrace` (30s, UID precondition, never 0), and
    every other pod through `DeleteControllerPod`. `pluginsInitScript`
    (`clientset_client.go`) traps `TERM`, because the script is PID 1 and
    the kernel drops an unhandled signal: each long command runs in the
    background under `wait`, and the trap signals the process group and
    exits 143. A new command in that script must run the same way),
    `cell_networkpolicy.go`
    (`reconcileCellNetworkPolicy` rebuilds the `<prefix>-cell`
    NetworkPolicy on every `Running`/`Connected` tick — never provisioning
    — so a tenant add/remove converges without a re-provision; ingress
    admits the BFF and the operator, never the gateway; egress reaches the
    gateway's gRPC and API-key-verify ports, each placed tenant's agent
    namespace, kube-dns, the configured API-server CIDRs, and each pool
    library remote host and port resolved per tick (`cell_pool_library_egress.go`)), `cell_jcasc.go`
    (the fixed cell JCasC fragment — `numExecutors: 0`,
    `MAX_SURVIVABILITY` flow durability, which the pipeline mesh needs
    because a lower level writes the flow program at a checkpoint. A job's
    own `durabilityHint` property (`items.DurabilityHint`) overrides this
    default; the plugin's `RunStatePusher` then pushes no state for that
    job, so a lower hint opts a job fully out of the mesh, not partway.
    `applyCellContent` also merges `cellGitHostKeyDefault`
    (`acceptFirstConnectionStrategy`) under the composed `jenkins.yaml` as
    the merge base, so a strategy in the cell bundle wins. And
    `tenantCloudFor`, which builds a tenant's cloud from the operator's
    constant `<team>-default` pod template followed by the lifted templates
    it receives, and sets `GarbageCollectionTimeout` to
    `tenantAgentGCTimeoutSeconds`; no other cloud sets it), `cell_pod_templates.go` (`liftTenantPodTemplates` reads
    every kubernetes cloud's `templates` list of a tenant's `jenkins.yaml` in
    bundle order. It resolves the tenant's own `variables.yaml`
    (`bundle.ParseVariables`, shared with `resolveBundleForController`) once,
    then applies the six skip rules in fixed order, each a
    `PodTemplateSkipped` warning. It stamps `name`, `namespace`,
    `serviceAccount`, and `label`, drops `labels`, rewrites `inheritFrom`,
    and puts every other key on `TenantCloudTemplate.Raw`. It never lifts a
    template that holds `${` after that resolve, because the cell's
    `ResolveVars` and unresolved-variable check run over the lifted text. Any
    other `jenkins.yaml` content is residue: one `BundleKeysIgnored` warning
    naming each sorted path, never a cell failure), `cell_items_assembly.go`
    (`assembleCellContent` runs the tenant items pipeline plus tenant cloud,
    credential, and pipeline-template fragment assembly. Per tenant it calls
    `readTenantBundleContent`, then `tenantBundleCloudContent` (the lift plus
    the `plugins.yaml`/`rbac.yaml` warnings), then, for a non-draining
    tenant only, `materializeTenantManifest` and the folder.
    `materializeTenantManifest` resolves the tenant's own `variables.yaml`
    in one pass (no cell variable, so `${varroa_*}` stays unresolved), then
    calls `bundle.TenantItemsManifest` (dropped-key checks, `bundle.ParseTenantItems`
    (resolve, `bundle.RemainingVarRefs`, `items.ParseStrict`), `items.ReRoot`, and
    `lint.Run` against `lint.CellRules()` (`NoPollSCMRule`, `NoGlobalCredentialsRule`,
    `NoControllerConfigDependencyRule`, `NoJobDSLRule`)), then the plugin checks
    `checkBranchBuildStrategiesManifest`, `checkFolderLibraryPluginsManifest`
    (every folder library's `RequiredPlugins` must be pinned), and
    `checkGiteaPluginManifest`; after the
    tenant's credentials project, `checkFolderLibraryCredentials`
    (`cell_library_credentials.go`) holds the tenant when a library names a
    credential its folder cannot see or one of a kind its source rejects (the same
    `checkLibraryCredentials` walker that a plain controller uses in
    `library_credentials_plain.go`); the tenant's template fragment runs
    `bundle.LintTenantTemplates` first. The cell lint (`lint.CellRules()`) checks each inline
    `jenkinsfile`; an `scmDefinition` template passes; only a failing template leaves
    the fragment. The filtered fragment must also load (`bundle.InjectPipelineTemplates`) against the base
    `applyCellContent` builds: the cell's `jenkins.yaml` with the cell
    bundle's own templates in it. A failed
    check **holds** the tenant: its folder is left out of
    `items.Merge`, its name goes on the merged `Manifest.Retain` so the
    mite keeps the folder, it carries no folder credential, and an
    `ItemsHeld` tenant warning names the fault. The check first reads
    `dropped-item-keys.yaml`: an undefined top-level key, or `root`, holds the tenant, and
    a `removeStrategy` or `retain` key adds a `BundleKeysIgnored` warning. A held tenant keeps its
    cloud and a fragment that loads; a fragment that does not load is
    dropped. A set `composedBundleRef` whose bundle is not found or has no
    `contentRef` yet (`errTenantBundleNotReady`) also holds the tenant; only
    an unset ref gives an empty folder. A draining tenant is never held. Any
    other read error, of the `ComposedBundle` or of its content ConfigMap,
    for a non-draining tenant still fails the assembly: it is an API
    fault, and a hold would push the cloud with no pod template.
    `appliedTenantsAfterItemsApply` keeps a held tenant's, or a refused tenant's, previous
    `status.cell.appliedTenants` membership, so a held applied folder never
    reads as drained; `updateCellInstance` calls `dropKeptTenants` on a pod
    change, which drops held and refused tenants, because the new pod's empty home holds no folder for them. A draining
    tenant keeps its lifted templates, and a read fault for it logs and
    lifts nothing instead of failing the cell. A non-draining
    tenant's own materialized `PipelineTemplatesYAML`, when non-blank, becomes
    one `bundle.CellTemplateFragment{Folder: team.Name, ...}` entry, returned
    on `assembleCellState.TemplateFragments` for the caller to merge via
    `bundle.InjectPipelineTemplates`), `cell_team_access.go` (`listPlacedTeams`
    reads each `Team` named in `cr.Status.Cell.Tenants`, skipping and
    logging one that is gone rather than failing the tick), and
    `cell_tenant_credentials.go` (`projectTenantCredentials` projects a
    tenant's `varroa.dev/tenant-credential=true` Secrets at `GLOBAL` scope.
    `assembleCellContent` projects onto a separate folder item and carries
    the result on `assembleCellState.FolderCredentials`, never in the merged
    items. It skips and warns on a
    malformed, unknown-kind, or duplicate-id Secret; the six kinds are
    `usernamePassword`, `secretText`, `sshPrivateKey` (a
    `basicSSHUserPrivateKey` with a direct-entry key), `secretFile` (a
    `file` credential), `certificate` (a PKCS#12 `certificate`), and
    `githubApp` (a `GitHubAppCredentials` with a PKCS#8 key). A
    `secretFile`, `certificate`, or `githubApp` value over 64 KiB is
    skipped (`maxBinaryCredentialBytes`, raw bytes). The projected
    credentials of one cell share `cellCredentialBudgetBytes`: 512 KiB,
    measured as wire bytes after base64 encoding. It is the 1 MiB NATS
    `max_payload` minus `rehydrate.BundleBudgetBytes` minus a 256 KiB
    reserve. Tenants are taken in name order; each credential that does
    not fit is skipped, and the scan continues. A carried tenant gets a
    `CredentialSkipped` warning. A tenant held by its library check
    returns its bytes to the budget and reports `ItemsHeld`.)
  - `cells_config.go`'s `ParseCellsConfigFromEnv` reads the operator's eight
    cell-related environment variables into `CellsConfig` (warm-pool
    enable/per-profile, `MaxTenantsPerCell`, `UnreadyGracePeriod`,
    `CellTemplate`, `NetworkPolicyEnabled`, `APIServerEgress`,
    `CellIsolation`), mirroring chart values `cells.*` and
    `networkPolicy.cellIsolation`. A missing or unparsable variable falls
    back to its documented default and only logs a warning — this function
    never fails the boot. `VARROA_CELLS_CELL_TEMPLATE` is JSON: absent,
    invalid, or an unknown key (strict decode) answers `defaultCellTemplate()`,
    and `null`, `{}`, or no known key answers nil (a pool with no template).
    `defaultCellTemplate()` must equal the chart default of
    `cells.cellTemplate`; `TestDefaultCellTemplate_MatchesChartDefault`
    reads `charts/varroa/values.yaml` and fails on drift. The five `cells.*` values
    are **pool seeds**, not live settings: they populate a `CellPool` the
    operator creates, and the operator never rewrites the spec of a pool that
    already exists, so an operator's scale edit survives every tick and every
    restart. `MaxTenantsPerCell` and `UnreadyGracePeriod` stay live as the
    fallback for a cell with no effective owner.
- **Tenant-sync announcement** (`cell_instance.go`, `single_tenant_announce.go`,
  `team_sync.go`, `internal/controller/cells/sync.go`) — the placement-fencing
  capability's write side: it keeps `varroa.tenant_placements` current for
  every controller, cell or not. The completion webhook rides the announce:
  `resolveTeamWebhook` (`team_controller.go`) reads the Secret that
  `notifications.webhook.secretRef` names, in `spec.pipelines.agentNamespace`,
  and sets the `Notifications` condition. `SyncPolicy.Webhook` travels in
  the request's `notification_webhook` field and never in the policy JSON. `updateCellInstance` writes
  `Controller.status.cell.instance` from the running pod's `metadata.uid` on
  every controller-reconciler tick; it clears the field only on a confirmed
  absence, never on a transient pod-read error. **A pod in phase `Failed` or
  `Succeeded` counts as no pod**, exactly as an absent pod does, because an
  exited Jenkins process serves no build; a pod in phase `Running` or
  `Pending` never clears the field. That read is the only proof a cell
  instance is gone, so `DeadInstance.ConfirmedGoneAt` (`cells/sync.go`,
  `TenantSyncDeadInstance` on the wire) is stamped **only** by a producer
  standing behind such a read: `maybeReannounce`'s `deadInstanceDelta` call,
  `announceSingleTenantPlacement`, `memberDeadInstances` for a member
  whose cell is still bound, and `cellPreviousInstances`. That last
  producer reads `status.cell.previousInstance` through
  `CellCandidate.PreviousInstance` for every cell a current or prior
  member holds, on both announce paths, so an empty `SyncCache` (an
  operator restart, a shard move, a recorder outage) still reports a
  rolled pod. It also stamps a confirmation on an unconfirmed cache entry
  that names the same instance. `deadInstanceDelta` and
  `memberDeadInstances` read only the cache. The operator is not the only
  source: the recorder derives an unconfirmed entry from its own stored
  member list (`internal/recordstore`, `deriveDeadInstances`), which covers
  a second roll during a recorder outage and a cell deleted while the cache
  is empty. A cell that **left** the candidate set is read
  by `TeamReconciler.departedMemberPods`, which applies the same pod rule to
  every member cell the tenant still names and hands the answers to
  `Inputs.DepartedPods` — the placement pass performs no Kubernetes read of
  its own. `performMove`'s deleted-cell branch and `memberDeadInstances`
  stamp such an entry only when that reading confirms the pod gone. A
  `NotFound` on the `Controller` confirms nothing: the `Controller` can go
  while its pod still drains and serves builds. Every other read error,
  and a pod that still terminates, confirm nothing either. **Every entry
  names its `cell` and `namespace`, and the announce reply hands the
  recorder's unconfirmed entries back** (`AnnounceResult.Unconfirmed`,
  kept in `SyncCache.Unconfirmed`; the stored `dead_instances` row is the
  durable list, and an empty cache announces at once to fill it again).
  On every tick `TeamReconciler.confirmDepartedInstances` lists the
  pods of each such namespace (`ResourceClient.ListPodPhases`, no label
  selector, so a pod with no label never reads as absent) and confirms an
  instance when no pod carries its UID, or that pod is `Failed` or
  `Succeeded`; a failed list confirms nothing. The confirmed list is
  `Inputs.ConfirmedDeparted`: it makes `maybeReannounce` due, `performMove`
  carries it too, and `appendConfirmedDeparted` drops an instance an
  announced member runs, because the recorder refuses a request that names
  a confirmed instance as stale. Without the reading a lost cell's member was never confirmed
  gone, so the promoted standby never received that member's armed runs. The
  field is a
  `*time.Time`: a zero `time.Time` marshals as year one and would confirm
  every entry at once. A controller that carries no
  cell role is its own tenant and its own cell, so
  `announceSingleTenantPlacement` sends a generation-zero `tenant.sync`
  "announce" request for it on every tick — the fence needs a row for every
  claim, with no exemption. A cell's own tenant announcements instead ride
  `cells.Announcer` through `TeamReconciler`'s placement pass
  (`cells.Place`, `cells/placement.go`). The recorder refuses a stale
  announce with `AnnounceResult.Stale` true, `OK` false, and no error. The
  placement pass treats the refusal as a failed announce, and
  `TeamReconciler.loggedAnnounce` and `announceSingleTenantPlacement` log
  it. `NewNATSSyncAnnouncer` and
  `NewNATSSyncRecordsRequester` (`team_sync.go`) wire that seam and the
  rehydration `records` request over `bus.TenantSyncSubject`, encoding
  `cells.SyncPolicy` (`cells/sync.go`, `Team.spec.pipelines`'s seven
  ledger/rehydration settings) the same way `api/v1alpha1`'s `Effective*`
  accessors compute it, plus `Team.spec.pipelines.exports` (the tenant's
  outward pipelines grants) carried verbatim. `SyncPolicy` holds a slice
  field, so a caller compares two values with `SyncPolicy.Equal`, never
  `==`. An exports-only edit reaches the recorder on the next tick through
  the same-generation reannounce path (`cells/placement.go`'s
  `maybeReannounce`) and never raises the placement generation. **Every
  announcement and every teardown purge carries the tenant object's
  `metadata.uid`** as `Incarnation` (`AnnounceRequest`, `PurgeRequest`,
  `announceSingleTenantPlacement`). The recorder stores it on the
  placement row and refuses a teardown purge that names another
  incarnation, so a `Team` created again under one name keeps the row its
  own first announcement wrote.
- **Cell rehydration send and apply** (`cell_rehydration.go`,
  `cell_rehydration_adapt.go`, `cell_rehydration_applied.go`,
  `cell_rehydration_entry.go`, `cell_rehydration_fetch.go`,
  `internal/controller/cells/rehydration_pass.go`) — the operational half of
  `rehydrate/` below. `attachCellRehydration` runs once per cell reconcile
  tick: for each placed `Team` it checks `rehydrationApplied` against the
  tenant's current placement generation and cell instance, gives up past
  `Team.spec.pipelines.rehydrateDeadline` (`rehydrationExpired`,
  `deadlineEntry`), else fetches the tenant's build records over the
  `records` tenant-sync request (`fetchTenantJobRecords`, enumerate then page
  at 32 job paths) and builds one `rehydrate.JobSeed` per job. Each member
  cell's own `status.cell.rehydration` entry is that member's seed state,
  read through `Inputs.MemberSeed`, so one member's deferred job never
  holds another member's build. `attachTenantHomes`
  (`cell_tenant_homes.go`) stamps `DesiredStateCommand.tenant_homes` from
  the recorder's `homes` request kind, inside the composite hash.
  `rehydrate.SelectForBundle` combines every pending tenant's seeds into one
  cell-wide bundle, falling back to `AdvanceNumberOnlyRound` when the full
  selection cannot fit; `attachCellRehydration` writes the result onto
  `desired.Rehydration` and `Controller.status.cell.rehydration`
  (`preSendEntry` before the send, `applyRehydrationResult` after the mite's
  `DesiredStateResult` returns, attributing `rehydration_deferred_jobs` back
  to each tenant by job-path prefix with `partitionJobPathsByTenant`).
  **An entry carries a command ID only for a delivered push with a bundle.**
  `preSendEntry` writes no command ID; `markRehydrationSent`
  (`cell_rehydration_inflight.go`) stamps it after a delivered push that
  carried `desired.Rehydration`, because the mite reports success for an
  absent rehydration section. It also opens the cell's in-memory
  `rehydrationInFlight` record (command IDs, bundle, tenant placements,
  number-only block, `Until`). **While that record holds, the tick builds
  no new bundle** (`heldRehydrationPlan`): a new command ID would replace
  the stamp before a slow apply reports. The hold ends on a result for any
  of the record's command IDs (`takeRehydrationResult` maps it to the
  stamped ID), a mite reconnect, a covered tenant's placement change, a
  tenant outside the push with a seed to send (a failed fetch never ends
  it; the record and its stamp then stay until a delivered push replaces
  them), or `Until` (`rehydrationPushWindow`: the command deadline plus
  one minute). A restart or shard move loses the record, and the next tick
  sends once more.
  **A number-only block never joins the process-local seeded-job cache on
  send.** `AdvanceNumberOnlyRound` runs against a copy of
  `numberSeededJobsCopy`'s set for the cell instance, and the in-flight record
  keeps the sent block's job paths and the instance it was built for. The
  seeded set belongs to one instance: a rolled pod holds no number file, so
  the set starts empty on a new instance. `handleConnected`'s result-drain calls
  `commitNumberOnlyProgress` with the block that `takeRehydrationResult`
  returns, which copies the paths into the real seeded set only on
  `rehydration_success` with no error, minus any path the mite deferred. A
  failed or unmatched block is never committed, so the next tick's selection
  recomputes and resends it.
  **A failed reload marks only its tenant.** The mite reports each failed reload
  in `rehydration_reload_failures`. `partitionReloadFailuresByTenant` attributes
  it to the owning tenant, and `applyRehydrationResult` sets that tenant's
  `Error` and keeps `Applied`. `carryRehydrationError` keeps the `Error` on a
  resend, so the status shows it until a later result clears it.
  **A deferred job does not drop the applied seed.** `applyRehydrationResult`
  sets `Applied` from the success and the error only. `carryAppliedSeed`
  keeps `Applied` and `Deferred` on the pre-send entry of a resend, for the
  same generation and cell instance. `seedAnswerOf` answers an applied entry's
  instance and its pending jobs (the deferred and unseeded paths), so only
  those jobs wait. A give-up (`deadlineEntry`, `released`) keeps `Applied`
  true only when an earlier apply exists on the same generation and instance.
  That release answers no pending jobs. A bundle that never applied stays
  held as a whole. `deadlineEntry` sets `DeadlineExceeded`. `seedAnswerOf`
  reads that flag, never the `Error` text. `carryRehydrationError` carries the
  flag with `Error`, and `applyRehydrationResult` clears it.
  **A number-only round holds only its uncovered jobs.** `CellRehydrationEntry.Unseeded`
  lists the jobs that wait for a number seed on the instance: the jobs no bundle
  covers, the jobs of the outstanding block while its push is outstanding, and a
  carried path that the tick still enumerates. A successful apply removes the
  covered paths (`applyRehydrationResult`). An applied entry whose `UnseededJobs`
  exceeds the length of `Unseeded` holds the whole tenant. `boundUnseeded` caps the
  list at `maxUnseededPaths` (256); a longer list is dropped and clears `Applied`.
  `capCellUnseeded` caps the encoded size of all `Unseeded` lists in one cell at
  `maxUnseededBytesPerCell` (256 KiB; each path counts its length plus 3 bytes),
  dropping the largest list first (ties by tenant name) until the total fits.
  `rehydrationApplied` also requires an empty `Unseeded`, so a lost in-flight
  record sends the next block on the next tick. The Team reconciler's `PendingJobs`
  input is `cellRehydrationEntryPending`, which returns `seedAnswerOf(...).pending`.
  `cells.RehydrationPass`, called from `TeamReconciler.Reconcile` inside the
  same status patch, derives `Team.status.rehydration` and the `Rehydrated`
  condition from the tenant's own entry on the bound cell by comparing
  generation and cell instance — it never computes a deadline of its own,
  since `attachCellRehydration` already wrote the give-up as the entry's own
  `Error`. **The announcement reads the seed from the cell, never from the
  `Rehydrated` condition.** `cells.currentRehydrationState` calls the same
  `Inputs.MemberSeed` seam the member entries use (`memberSeedReader`,
  `team_controller.go`), because `RehydrationPass` writes that condition
  **after** `cells.Place` runs in the same tick. A read of the condition
  reports the tick before, so the tenant would be announced as unseeded on
  the tick its seed applies and the fence would hold every build of that
  tenant until the next announcement.
- `bootstrap.go` — `BootstrapLocalAdmin` seeds the local-mode admin user/password on
  first boot.
- `webhook_replay.go` — drains a NATS queue of webhook deliveries received while a
  controller was hibernated, replaying them once the mite reconnects. It routes two
  envelope shapes on one filter, chosen from the envelope's own fields, never the
  subject: the controller shape (unchanged) and the tenant shape (`Team` set,
  `Namespace`/`Controller` both empty), whose target it re-resolves from
  `Team.status.placement` at delivery time — a move between ingest and delivery lands
  on the new cell. A tenant delivery naks on an empty placement, a moving tenant
  (`Placed=False`/`TenantMoving`), or an absent/not-`Connected` target `Controller`;
  it terms only when the `Team` itself is gone.
- `sharding/` (`ring.go`, `shardset.go`, `manager.go`) — consistent-hash shard ownership so multiple
  operator replicas split the `Controller` watch set; `SetShardOwnership`/
  `ownsController`/`EnqueueShards` on `*Reconciler` consume it. Only the owning
  replica may hold the inbound subscription of a mite: each holder answers the
  mite's token refresh, and a held result channel fills with no reader.
  `reconcileController` opens it through `EnsureInbound` in Provisioning, Running, Connected, and Failed.
  Every lease API call of the shard manager goes through `callCtx` (`leaseCallTimeout`
  20s, below the 25s deadline). Never set `rest.Config.Timeout`, because watches share it.
  `NewShardManager` takes a `metric.MeterProvider` as its first parameter and
  creates `varroa.operator.shards.held` and `varroa.operator.shard.handoffs` on
  its `"varroa-operator"` meter; it returns an error that `main` exits on.
  `ShardManager` calls `onReleased` once per revoke group. A rebalance pass makes at most two calls: before its claims with the shards that it dropped, and after its claims with the shards whose renewal failed during the claims. A stale sweep makes one call per sweep that revokes shards. The sweep call can run during a pass.
  This is safe: the hook reads the held set when it runs, and a shard that a pass
  claims again enqueues its controllers again through `onAcquired`. The operator wires it to
  `Reconciler.ForgetUnowned`, which calls `Transport.ForgetMatching` for every
  controller that the replica does not own. `ApproveRestart` and `ApproveDeletion`
  return `ErrNotShardOwner` on a replica that does not own the controller, before
  any read; they run outside the workqueue, so each carries its own gate. Their
  status writes go through `crdstore.MutateStatus` and change only the approve's
  own fields, never `PatchControllerStatus`. `Reconcile` and both approve methods
  take `controllerLocks` (`keylock.go`) for the controller's `namespace/name`. The
  tick holds it from its first read to its end status write, so it cannot write
  back stale approve fields. The lock is local to the process: this is correct only
  because the shard owner alone runs the tick and the approve, and because a stalled
  former owner's end write conflicts on its stale `resourceVersion`. `Reconcile` and both
  approve methods enter the shard through `enterShard` (`ShardSet.EnterController`)
  and use the returned context for every write. `ShardManager` releases a shard
  through `ShardSet.Drain`: it waits for that work (`releaseDrainWait` 4s, at
  shutdown `shutdownDrainWait` 10s), cancels what still runs, and waits once more.
  It releases the lease only when the work ended; otherwise the lease must expire.
  A lost lease calls `ShardSet.Revoke`, which cancels the work at once. Each held
  shard records the start of its last confirmed lease write (`ShardSet.Confirm`,
  monotonic clock). A sweep goroutine (`staleSweepInterval` 1s) and both List
  failure returns of a pass call `revokeUnconfirmed`, which revokes every shard
  not confirmed for `leaseDuration - heldLeaseMargin` (25s of 30s). Never compare
  against the Lease `renewTime` for this. The deadline must stay above the
  healthy renewal age of about 20s. A pass renews due shards oldest first, before each claim and after the last claim. Each confirming response is stored, so a second renewal in the same pass carries the new `resourceVersion`. A pass claims a lease that this replica still
  holds, through an Update with the listed `resourceVersion`. The tick
  checks `ctx.Err()` before its end writes. A replica that stalls past its lease
  still holds the shard until its next pass, so the tick writes are also fenced in
  storage: every write of the tick to its own Controller goes through
  `writeTickStatus`, `patchTickMeta`, or `setTickHibernated`. Each sends
  `cr.ResourceVersion` as a precondition and moves it forward on success,
  starting from the tick's read. `setTickHibernated` is the tick's only writer of
  `status.hibernated` (the power-off clear and the auto-hibernate set), through
  `SetHibernatedAt`. The tick never calls `SetHibernated`: its fresh read lets a
  stalled replica hibernate a controller that the new owner woke. The wake,
  hibernate, power-write, and brood paths keep `SetHibernated`. Never adopt a
  version from a fresh read (`SetHibernated`, `crdstore.Get`): that defeats the
  fence. A conflict calls `fenceTick`, which cancels the tick context
  (cause `*tickConflictError`), so the tick sends no further write of any kind.
  The tick scales a StatefulSet down only through `scaleTickToZero`. It reads the
  StatefulSet, writes `writeTickStatus` as a fence, then patches with the StatefulSet
  `resourceVersion` it read. A StatefulSet at 0 gets no fence write.
  The tick scales a StatefulSet up only through `createTickStatefulSet`, which passes
  `writeTickStatus` as the fence to `CreateStatefulSet`. A create, or a scale-up from 0,
  runs the fence. A StatefulSet already at 1 or more gets no fence write.
  `Reconcile` checks `tickFenced` after each write and after the phase machine:
  one Info line, requeue after 1s, no error. A new tick write to the
  Controller must use one of the three helpers. An approve waits at
  most `approveLockWait` (20s) and then fails with a plain-text "retry the approve"
  error. It writes nothing and sends nothing to the mite. A `plugin-roll` approval approves
  only the target checksum that it read (`errPluginRollChanged` otherwise). An
  `approve`/`restart`/`force`/`reload` approval sends only the content that
  `pendingRestart.contentHash` names (`computeDesiredContentHash`: JCasC, RBAC, and
  items, never tenant homes). On a mismatch it sends nothing, records the new content
  through `raisePendingRestart`, and returns `*PendingRestartChangedError` with the new
  sections. A non-empty `expectedContentHash` must equal the pending content hash too.
  `raisePendingRestart` is the only code that sets `pendingRestart`: for the same content
  hash it keeps `detectedAt` and `changes`. An approval stamps `desiredContentHash` with
  `desiredStateHash`; both are sticky in `controllerStatusPatch`. A missing pending restart is
  `ErrNoPendingRestart`. The 10-minute shard-resync tick calls
  `ForgetUnowned` too, because a reconcile in flight during a release can
  subscribe again. The shutdown release (`releaseAll`) calls no hook.
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
- `rehydrate/` — build-record rehydration rendering (design D1/D1a/D2/D4). Pure
  functions, no Kubernetes or NATS I/O: `RenderBuildXML`/`RenderChangelog` render
  one run's `build.xml`/`changelog0.xml`; `BuildJobSeed` selects one job's records
  (`Team.spec.pipelines.rehydrateBuilds` newest, plus the newest successful run,
  always); `SelectWithinBudget` truncates a tenant's seeds to the 256 KiB
  `mitev1.RehydrationBundle` cap; `BuildBundle` gzips, hashes, and mints the
  bundle's UUIDv7 id. Its `Record`/`Change` input type mirrors
  `recordstore.RehydrationRecord`'s field names but does not import that
  package — a caller adapts whatever wire type it holds
  (`recordstore.TenantSyncRecord`) into a `Record`. `Options.HasArtifactManagerS3`
  comes from `coreSetForCr`; `rehydrate` never calls it directly. `testdata/`
  holds one golden `build.xml` per `pluginlock.Seeds()` entry (`UPDATE_GOLDEN=1`
  regenerates them) — the renderer's own output, not a live-Jenkins capture.
  A live capture may still close three gaps: no queue id, no commit author
  email, no affected paths anywhere upstream.
- **Operator list replies are paged** by `pageList` in `list_page.go`, the one
  paging path for controllers, config, brood-operation, and brood-schedule lists. It sorts rows by
  `namespace/name`, skips keys at or below `After`, and fills a reply up to
  `bus.ListPageBudget` (900 KB). `Next` names the last kept row, and it is
  empty on the last page. A single row over the budget fails the list with
  `listRowTooLargeError`.

- **`forgeops/`** — answers the operator forge verbs `check` and `read` (`operator.<cluster>.forge.<verb>`). `chooseCredential` reads the Team's agent-namespace tenant-credential Secrets and keeps the ones whose `varroa.dev/allowed-hosts` lists the repository host, sorted by id. `pickWriter` (`check`, `pullrequest`, commit status) uses the first credential whose `Repository` reports push; a credential that reads but cannot push is skipped. `pickReader` (`read`) uses the first credential that reads the repository. Replies name only the credential id and HTTP status, never a body or a token. `ShardKey(team)` is `team/<name>`, the ring key of every forge verb. `EndPendingStatus` (`commitstatus.go`) serves the recorder's `end_commit_status` action, not a forge verb: it sets each `pending` status whose `target_url` names the job path and build number to `error`. A context with an end state, or another build number, is never written.
- **`forgeops/issue.go`** — answers `operator.<cluster>.forge.issue`. The accepted, enabled `TeamAgent` of the Team must name the Team and enable issue output. Its labels must sit inside `outputs.issues.labels`. A label the forge does not carry answers `invalid_request` and names the label. `issue` builds its candidates with `issueCandidates`: every credential that reads the repository, pushing credentials first, then read-only ones. The operator tries each candidate in turn. A forge 403 on the write moves to the next candidate. Any other error stops the attempt. `forge_write_denied` answers only when every candidate answers 403, and it names each credential tried.
- **`teamagents/`** — the leader-elected `teamagent` ticker (flag `--team-agents`, core install only). It applies one `VarroaRoleBinding` per accepted `TeamAgent`, labelled `varroa.dev/team-agent`, with the `developer` role scoped to the Team. It deletes each labelled binding that no accepted object needs.
- **Activity producers** — `cell.signature_refused` (`cell_script_approval_status.go`, one event per new tenant refusal) and `tenant.placement_moved` (`team_controller.go`, a move between two cells only). Each event builder is a pure function (`refusalEvents`, `placementMovedEvent`), so a test checks the rule without a publisher.

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
