# Update Center

<!-- sources: api/v1alpha1/types.go (UpdateCenter CRD), internal/updatecenter/ (server + client), internal/controller/updatecenter_controller.go (reconciler) -->

The Varroa Update Center is an in-cluster HTTP service that serves Jenkins plugin metadata and HPI downloads. It replaces the public `updates.jenkins.io` for air-gapped installations and provides pull-through caching for clusters with limited external bandwidth.

## CRD: `UpdateCenter`

A single cluster-scoped `UpdateCenter` custom resource named `varroa-update-center` controls the component. The operator's `UpdateCenterReconciler` checks storage readiness, imports seed plugin packs, computes coverage gaps against declared profiles and bundles, and derives a lifecycle phase.

### `spec.storage`

Two backends are available:

| Type | Backing store | Trade-offs |
|------|---------------|------------|
| `local` | Single PVC (`ReadWriteOnce`), Recreate strategy | Simpler, no external dependency; single replica only |
| `oci` | OCI-compatible container registry | Multi-replica (HA), relies on registry availability; `existingSecret` for auth |

**`local` example:**

```yaml
spec:
  storage:
    type: local
    local:
      storageClassName: "fast-ssd"
      size: "50Gi"
```

**`oci` example:**

```yaml
spec:
  storage:
    type: oci
    oci:
      ref: "ghcr.io/myorg/updatecenter:latest"
      existingSecret: "ghcr-cred"
      insecure: false
```

> **PVC persistence.** The `local`-mode PVC (`varroa-updatecenter`) carries
> `helm.sh/resource-policy: keep`, so disabling `updateCenter.enabled` or running
> `helm uninstall` does **not** delete it — your cached plugin blobs survive, and
> re-enabling the component re-adopts the same volume. To reclaim the storage on a
> deliberate decommission, delete the PVC by hand after disabling:
> `kubectl delete pvc varroa-updatecenter -n <namespace>`.

### `spec.pullThrough`

When enabled, the update center fetches plugins not found in the local store from an upstream update center, verifies the SHA-256 checksum, caches the blob, and serves it. Subsequent requests for the same plugin serve the cached copy without contacting the upstream.

**LTS-line resolution.** Pull-through resolves each plugin's SHA-256 across an ordered set of upstream metadata sources: the *current weekly* `update-center.actual.json` first, then one `dynamic-stable-<version>/update-center.actual.json` source per declared LTS-line profile. Because the weekly metadata lists only the latest version of each plugin, an LTS-line `JenkinsVersionProfile` that pins an aged version (e.g. `role-strategy@867…` after the weekly has moved to `878…`) is resolved from its matching `dynamic-stable` source. A version present in **no** source returns a 404 (not-found upstream), unchanged. This means LTS-line pins are served automatically — pre-seeding via `varroactl export`/`import` is no longer required for reactive pull-through (seed packs remain useful for air-gapped installs, where pull-through is off).

The LTS sources are **derived automatically**: on each reconcile (after the storage-readiness check), the operator scans declared `JenkinsVersionProfile`s, and for every one whose `resolveVersion` is an exact `MAJOR.MINOR.PATCH` it derives `<pullThrough.upstreamURL>/dynamic-stable-<resolveVersion>/update-center.actual.json`. The deduplicated list is written to the operator-managed `varroa-updatecenter-metadata` ConfigMap, which the update-center Deployment mounts (via `VARROA_UC_LTS_METADATA_FILE`); the server re-reads it on each metadata-cache refresh. No manual configuration is needed — add an LTS profile and its metadata source is consulted on the next tick. The active sources are reported on `status.resolvedMetadataSources` (weekly first).

> **Staleness window.** A newly-derived source becomes effective after the kubelet syncs the mounted ConfigMap (~1 min) plus one metadata-cache TTL (1 h). Profiles change rarely, so this lag is normally invisible; if you need a source picked up immediately, restart the `varroa-updatecenter` Deployment.

```yaml
spec:
  pullThrough:
    enabled: true
    upstreamURL: "https://updates.jenkins.io"
    downloadURL: "https://updates.jenkins.io/download"
```

### `spec.seed.refs`

A list of OCI references for plugin packs to import at startup. On first reconciliation the operator resolves each ref, copies the pack into the store, and records the digest in `status.seedImportedDigests`. Already-imported digests are skipped on subsequent ticks — the import is idempotent.

```yaml
spec:
  seed:
    refs:
      - "registry.example.org/plugins/base-plugins:v1"
      - "registry.example.org/plugins/security-plugins:v2"
```

The chart ships a default seed ref naming the first-party `varroa-mcp-tools` addon pack, so an enabled update center holds it without operator action:

```yaml
# charts/varroa/values.yaml
updateCenter:
  seed:
    refs:
      - ghcr.io/varroaci/varroa-jenkins/plugin-addon:varroa-mcp-tools-1.0.0
```

Setting `updateCenter.seed.refs` in your own values **replaces** this list; it does not append to it. If you override it and still want the MCP tooling, carry the default ref across.

An unreachable seed ref is not fatal. Readiness is `StorageReady ∧ (CoverageComplete ∨ pull-through)` and does not consume `SeedImported`, so an air-gapped install that cannot reach ghcr reports an informational `SeedImported=False` condition and a `Ready` update center.

## Status

The reconciler writes the following status fields.

### `status.phase`

| Phase | Meaning |
|-------|---------|
| `Pending` | Initial state, before any pass completes |
| `Ready` | Storage reachable **and** either coverage is complete or pull-through is enabled (gaps are served on demand) |
| `Degraded` | Storage reachable, coverage incomplete, and pull-through is **disabled** (air-gapped) — the store cannot serve the missing plugins; **or** the store's HTTP API is unhealthy (inventory unavailable) regardless of pull-through |
| `Error` | Storage unreachable |

### `status.conditions`

| Condition | True when |
|-----------|-----------|
| `StorageReady` | The configured backend (`local` PVC is `Bound`, or `oci` registry responds to `ListManifests`) |
| `SeedImported` | All configured seed refs resolved and imported (or `refs` is empty) |
| `CoverageComplete` | Every plugin declared by JenkinsVersionProfiles and ComposedBundles exists in the store |
| `Ready` | `StorageReady` **and** (`CoverageComplete` **or** pull-through covers the gaps) — see below |

**Readiness and pull-through.** Controllers only route their plugins-init container to the in-cluster UC when the `Ready` condition is `True` (unless an explicit `ProvisioningDefaults` override forces it regardless of readiness — see [Cross-cluster access](#cross-cluster-access-hive-clusters) for that lever). Because pull-through serves any declared plugin on demand — pulling it from upstream on first request, and `jenkins-plugin-cli` itself falls back to upstream for anything the store still can't resolve — a pull-through UC is usable even with a cold or partial cache. So when pull-through is enabled, genuine coverage gaps do **not** hold the UC out of `Ready`; the `Ready` condition carries reason `PullThroughServing` and `CoverageComplete` stays `False` (with the gaps still listed) as an informational quality signal. This is deliberate: if gaps blocked readiness, controllers would never route to the UC, so the store would never warm — a pull-through UC that starts empty would stay empty forever. An **air-gapped** UC (pull-through disabled) is genuinely `Degraded` when coverage is incomplete, because it can only serve what it already holds. A failure to *compute* coverage (the store's inventory endpoint is down) is never waived by pull-through — that signals an unhealthy UC, so `Ready` stays `False` with reason `InventoryUnavailable`.

### `status.gaps`

When `CoverageComplete` is `False`, `gaps` lists up to 50 plugins that are declared but absent. Each gap records the plugin name, required version, and the profile or bundle that requires it. When there are more than 50 gaps, the list is truncated and the `CoverageComplete` condition message includes `"N more gaps not shown"`.

## Importing plugin packs

There are two paths for populating the store:

### Automatic (reconciler)

Set `spec.seed.refs` on the `UpdateCenter` CR. The reconciler resolves each ref, copies the OCI plugin pack into the store, and records the imported digest. This runs on every reconciliation tick but skips already-known digests.

### Manual (`varroactl import --to uc://`)

Use the CLI to push a plugin pack directly to the update center's HTTP import endpoint:

```bash
export VARROACTL_UC_TOKEN=$(kubectl get secret -n varroa-system \
  varroa-updatecenter-import-token -o jsonpath='{.data.token}' | base64 -d)

varroactl import \
  --from oci://registry.example.org/plugins/my-pack:v1 \
  --to uc://varroa-updatecenter.varroa-system.svc:8080
```

The `--to uc://` target requires the `VARROACTL_UC_TOKEN` environment variable. The CLI builds a tar.gz of the source plugin pack and POSTs it to the `/api/v1/import` endpoint.

## Uploading a plugin

Both import paths above *pull*: they need the plugin to already exist somewhere fetchable. Uploading is the push tier — for an organisation's internal plugin, or a first-party addon that has not been published yet.

From the dashboard, open **Admin & access → Update Center**, pick a `.hpi`/`.jpi`, press **Preview closure**, review, then **Upload**. From the CLI:

```bash
varroactl upload plugin ./varroa-mcp-tools.hpi --dry-run   # resolve and validate, store nothing
varroactl upload plugin ./varroa-mcp-tools.hpi             # commit
```

Three things distinguish this from `varroactl import --to uc://`:

- **It goes through the BFF with your API key**, not through `uc://` and `VARROACTL_UC_TOKEN`. The import token is shared, so an import is unattributable; an upload is recorded against a real user, both in the activity feed and in the pack's `uploadedBy` config field.
- **It requires the `updatecenter` resource and the `upload` verb.** `varroa:admin` has it through `*`/`*` and `varroa:operator` is granted it explicitly; `developer` and `viewer` are not.
- **It resolves the plugin's dependency closure before storing anything.** An import stores whatever it is given.

### Enabling uploads

Uploads require the update center to be a genuine **single writer**. An upload is a read-modify-write against a store with no conditional-push primitive: two writers uploading the same `name@version` would produce two different manifests on the same tag, and no in-process lock can prevent that.

`updateCenter.uploads.enabled` (default `true`) therefore forces the update-center Deployment to `replicas: 1` with a `Recreate` rollout strategy — replica count alone is not enough, because a one-replica Deployment on the default `RollingUpdate` runs the old and new pods concurrently during a rollout. Set it to `false` to keep a horizontally-scaled registry-backed update center; the endpoint then returns `501 uploads-require-single-writer`.

Reads — metadata, downloads, inventory — are unaffected and stay horizontally scalable either way.

## Dependency closure

A plugin's `META-INF/MANIFEST.MF` declares `Plugin-Dependencies` as **minimum** versions, not pins: `workflow-api:1384.vdc05a_48f535f` is satisfied by `1413.v2ff1a_5e720fa_`. Pushing an HPI whose mandatory closure is not satisfiable produces a plugin that installs and then fails at Jenkins boot, discovered only on the next provisioning cycle — so the closure is resolved and validated at upload time, before a single byte is committed.

Every **mandatory** dependency is walked transitively. Optional dependencies (`;resolution:=optional`) are reported and never resolved. Each dependency resolves against the store, then the declared plugin set, then upstream:

| status | meaning | bytes fetched |
|---|---|---|
| `satisfied-store` | the store already holds a version that satisfies the minimum | no |
| `lock-too-old` | a profile or bundle pins a version older than the minimum — a **warning**; provisioning's plugin-version gate has the final say | no |
| `declared-not-yet-stored` | a profile or bundle pins a satisfying version that is not in the store yet; on-demand pull-through will fetch that exact version | no |
| `planned-fetch` | nothing declares it, so the upload fetches the newest satisfying version | **yes** |

**A declared plugin is never fetched by an upload.** If a profile or bundle pins a version, the upload leaves it alone: a second writer choosing its own version would shadow the declaration. That is why `declared-not-yet-stored` is a warning rather than a fetch.

Note also that a stored copy can only ever *satisfy* a requirement, never terminate one: an old cached `X@1.0` against an `X ≥ 2.0` requirement falls through to the declared or upstream tier like any other unsatisfied dependency.

### Stored is not installed

The served `update-center.actual.json` carries an empty `dependencies` array. Storing an uploaded plugin's closure therefore makes those bytes **present in the store**, not **installed alongside it**: `jenkins-plugin-cli` installs exactly the plugin set provisioning writes into `plugins.txt`, and nothing expands a dependency list on its behalf.

Until derived catalog items land, **a bundle installing an uploaded plugin must enumerate every closure member itself**. The upload response and `varroactl upload plugin --dry-run` both print the resolved closure precisely so you can copy it into the bundle.

## Rejection reasons

A rejected upload writes nothing. Whichever envelope code is chosen, **every** rejecting dependency is listed in the response with its own reason, what was found in each tier, and a remediation — so one round trip shows every problem rather than one per retry.

Per-dependency reasons:

| reason | meaning | retryable | remediation |
|---|---|---|---|
| `not-in-store` | the plugin is absent from the store and pull-through is disabled — the air-gap case | no | seed it via `spec.seed.refs` or `varroactl import --to uc://…` |
| `unreachable` | every metadata source is healthy and none lists a version satisfying the minimum | no | for a declared plugin this is an aged pin: pre-seed the version with `varroactl export`/`import` or `spec.seed.refs`, or refresh the pin. Otherwise upstream simply has nothing new enough |
| `metadata-unavailable` | at least one upstream metadata source failed to fetch or parse, so the negative is unprovable | **yes** | retry once the source recovers |
| `closure-unverifiable` | a mandatory dependency's own dependency list cannot be determined, so the closure was never validated | no | re-import that plugin as a `kind`-bearing pack carrying the dependency annotation, or pre-seed a version upstream still lists |

Envelope codes, highest precedence first — structural failures before validation failures before resolution failures, and permanent always before retryable:

| `error` | HTTP | meaning |
|---|---|---|
| `closure-too-deep` | 422 | the closure exceeded the depth cap of 32 — a corrupt or hostile manifest chain, not a real dependency graph |
| `closure-unverifiable` | 422 | ≥1 dependency's own dependency list could not be determined |
| `unresolved-dependencies` | 422 | ≥1 dependency is `not-in-store` or `unreachable` |
| `metadata-unavailable` | 503 | only degraded-metadata dependencies remain; **retryable** |
| `invalid-artifact` | 400 | not a ZIP, or `META-INF/MANIFEST.MF` is missing or unparseable |
| `missing-manifest-fields` | 400 | no `Short-Name`/`Extension-Name`, or no `Plugin-Version` |
| `malformed-upload` | 400 | the request body is not `multipart/form-data`, or has no `file` part |
| `forbidden` | 403 | the caller lacks `updatecenter`/`upload` |
| `duplicate` | 409 | the store already holds this `name@version` at the same digest |
| `version-digest-conflict` | 409 | the store holds this `name@version` at a **different** digest. A fixed version's bytes must never change: publish a new version rather than replacing one |
| `update-center-disabled` | 409 | the `UpdateCenter` CR does not exist |
| `too-large` | 413 | the artifact exceeds `updateCenter.uploads.maxBytes` (default 256 MiB) |
| `update-center-status-unavailable` | 500 | reading the `UpdateCenter` CR failed | 
| `uploads-require-single-writer` | 501 | see [Enabling uploads](#enabling-uploads) |
| `fetch-failed` | 502 | a dependency download or its checksum verification failed; **nothing was written** |
| `update-center-unreachable` | 502 | the update center could not be reached |
| `declared-set-unavailable` | 503 | the operator has not written the declared plugin set yet, or it is unreadable. **Retryable** — it resolves on the operator's next reconcile. The upload is refused rather than planned as if nothing were pinned, which would fetch unpinned versions for plugins that are in fact locked |

## Uploaded plugins in the catalog

Putting bytes in the store does not make a plugin *selectable*. That is what derived catalog items are for.

When the update center is enabled, the operator creates and owns a reserved `CatalogSource` named **`varroa-update-center`** in the operator namespace. It is the only catalog source that sets neither `repoURL` nor `ociRef` — its content is the store. From it the operator derives one `CatalogItem` of `type: plugin` per stored `(plugin, version)`, and prunes an item when its plugin leaves the store.

You do not create this source, and you should not edit it: the operator reasserts its spec on every reconcile, and deletes it when the `UpdateCenter` CR goes away. A `CatalogSource` named `varroa-update-center` in any *other* namespace is rejected with `phase: Error`.

**Items live only in the operator namespace, and that is enough.** A `ComposedBundle` in any namespace can reference one by an unqualified `itemRef`: the composer resolves local-namespace-first and falls back to the operator namespace. There is no per-namespace copy to keep in sync.

### The closure is pinned for you

Each derived item's content is a `plugins.yaml` fragment listing the plugin **and its full mandatory dependency closure**, every entry pinned to an exact version. This is what closes the gap described under [Stored is not installed](#stored-is-not-installed): the served metadata carries an empty `dependencies` array, so `jenkins-plugin-cli` installs exactly what it is given, and the item gives it everything.

The closure is resolved from the **store**, and only from the store, so that one item is correct for every version profile. A dependency the store does not hold falls back to a profile lock pin, but only when every eligible profile's lock agrees on one version — a disagreement makes the item invalid rather than guessing. Optional dependencies are excluded, and declared versions are treated as minimums, exactly as at upload time; see [Dependency closure](#dependency-closure) for what those minimums mean and [Rejection reasons](#rejection-reasons) for how an unsatisfiable one is reported at upload.

`status.closure` on the item records what was pinned and why: each entry's version, whether the root declared it directly or it came in transitively, whether the version came from the store or from a lock, and the minimum in force.

An item is marked `status.valid: false` — and carries no content — in exactly three cases, all of them **derivability** failures:

| cause | what to do |
|---|---|
| a mandatory dependency is in neither the store nor a unanimous profile lock | seed or upload the missing plugin |
| the store reports two different entries for one `(name, version)` | a fixed version's bytes and metadata must never differ; re-export or re-import the offending pack |
| resolution did not converge | a solver defect, not your data — file a bug with the item name |

Only the affected plugin is invalidated. Every other plugin derives normally and the source stays `Ready`.

### Finding them

Open **Catalog** in the dashboard. Update-center-backed plugins appear in their own **Update Center** group, above the source-backed items. A plugin stored at several versions is one row with a version selector rather than one row per version; the row defaults to the version your profile lock pins, or to the newest stored version when no single lock pins it.

Opening a row shows the pinned closure and, beside each entry, what each version profile's lock pins for it. A cell marked `≠` means that profile pins a different version, which is what `pluginVersionConflict` blocks on at provisioning time — worth resolving before you compose the item into a bundle.

If the group is missing entirely, check that the `UpdateCenter` CR exists and that `kubectl -n <operator-ns> get catalogsource varroa-update-center` reports `phase: Ready`. A source stuck in `Error` names the reason in `status.message`; an unreachable or partially-readable store is reported there and **prunes nothing** while it lasts.

## Compat badges

Each derived item carries one advisory verdict per `JenkinsVersionProfile`, surfaced as a badge in the catalog browser and as a full per-profile matrix on the item's detail page.

| verdict | meaning |
|---|---|
| `compatible` | nothing to report against this profile |
| `core-too-old` | a plugin in the closure declares a `Plugin-RequiredCore` newer than the Jenkins version this profile actually deploys |
| `dep-below-minimum` | the best version the **store** holds for a closure entry is below its declared minimum — the same for every profile |
| `lock-too-old` | this profile's lock pins a closure entry below its effective minimum, or supplied a pin that itself falls short |
| `unknown` | there is nothing to judge: no plugin declares a required core, the profile's version cannot be compared, or the profile's plugin set is not ready |

The profile's *effective* Jenkins version is `spec.resolveVersion` when set, otherwise `spec.version`. An LTS-line profile deploys the resolved patch, so a plugin requiring `2.555.1` is not flagged against a profile resolving to `2.555.3`.

**These verdicts never block anything.** They do not set `status.valid`, they do not prevent selecting an item into a `ComposedBundle`, and they do not stop provisioning. A badge is a heads-up, not a gate.

The real gates are elsewhere and are unchanged:

- **Core compatibility** is enforced at provisioning against the *controller's* Jenkins version and its profile.
- **Plugin version conflicts** are enforced at provisioning when a bundle requests a plugin at a version differing from the profile lock's pin — in either direction. That is why the detail view shows lock pins beside selected versions: it predicts the conflict at the point of selection, where a compat verdict deliberately does not.

A profile whose plugin set is not ready, or which has no materialized lock, yields `unknown` for every item — never a concrete verdict. Its lock may be stale, and a judgement from data we already distrust is worse than none.

## Plugin pack format

The normative reference is [Plugin Packs](../config/plugin-packs.md). Two facts matter operationally:

- Every pack declares a `kind` in its config blob — `profile` for a resolved version-profile set, `addon` for a single standalone plugin. A pack with no `kind` is rejected on read.
- A plugin's identity comes from its own `META-INF/MANIFEST.MF`, never from a command flag, so a pack cannot be mislabeled relative to the bytes it holds.

## Addon packs

An addon pack holds exactly one plugin. It exists for plugins that are not published to `updates.jenkins.io` at all — pull-through cannot fetch them and a profile export cannot resolve them, so without an addon pack their bytes have no path into an air-gapped or UC-backed install.

Build one from a local `.hpi`:

```bash
varroactl export plugin-addon \
  --hpi ./varroa-mcp-tools.hpi \
  --to oci://ghcr.io/myorg/plugin-addon:varroa-mcp-tools-1.0.0 \
  --description "MCP endpoint tooling" \
  --tag mcp --tag first-party
```

| Flag | Required | Meaning |
|---|---|---|
| `--hpi <path>` | yes | the local `.hpi` to pack |
| `--to <dest>` | yes | `oci://`, `dir://`, or `tar://`. `uc://` is rejected — build to a registry or a directory and `varroactl import` from there |
| `--tag <s>` | no, repeatable | recorded on the plugin layer |
| `--description <s>` | no | recorded on the plugin layer |
| `--registry-config <path>` | no | Docker `config.json` for registry auth |
| `--insecure` | no | plain HTTP for the registry |
| `--dry-run` | no | print the resolved pack config and annotations, push nothing |

The plugin's **name and version are read from the archive's manifest** and no flag can override them. `--tag` and `--description` are free-form metadata on the layer; they do not affect identity.

An unparseable `.hpi` fails the command and pushes nothing — for an addon there is no other source for identity, so there is no pack to build.

The recommended tag shape is `<repo>/plugin-addon:<name>-<version>`, in a repository separate from `plugin-pack`. That is a **naming convention, not an enforced guarantee**: a version-bearing OCI tag is not immutable, and this command neither reads an existing tag nor refuses to overwrite one. If you need a re-push of the same `name@version` with different bytes to be rejected, that is an upload-path property, not something a publishing tool provides.

### Seeding a first-party addon

1. Build and push the addon pack as above, using a version-bearing tag.
2. Name that exact ref in `updateCenter.seed.refs` (or `spec.seed.refs` on the CR).
3. On the next reconciliation the operator resolves the ref, copies the pack into the store, and records its digest in `status.seedImportedDigests`.
4. Confirm the plugin is served:

```bash
kubectl exec -n varroa-system deploy/varroa-updatecenter -- \
  wget -qO- localhost:8080/update-center.actual.json | grep varroa-mcp-tools
```

## Stores holding packs written before `kind`

A store populated by an older Varroa can hold packs whose config blob predates the `kind` field. A strict reader drops those packs out of the served inventory and out of coverage, so a download of a plugin they hold 404s. There is no migration code and none is planned: `kind` is not inferrable from a legacy config, and rewriting a content-addressed config blob would mean rebuilding and retagging the manifest that references it by digest.

One kind-less (or otherwise unreadable) pack degrades coverage for the plugins it alone holds — it does **not** take down the rest of the store. `GET /api/v1/inventory` serves every plugin from every readable pack and names each unreadable one, by ref and parse error, in a `skippedPacks` array; it only returns `503` when **no** pack in the store could be read at all. The derived catalog (`CatalogSource` of kind `updateCenter`) stays `Ready` on a partial scan — the readable subset still syncs — but withholds pruning for that pass (a partial listing is a lower bound, not proof the missing plugins are gone) and surfaces a warning in `status.message` naming the skipped pack refs. The operator's `UpdateCenter` gap analysis likewise computes gaps from the readable subset and appends a `(inventory partial: ...)` note to `CoverageComplete`'s message rather than failing the check. Use the disclosed refs to identify which pack needs the re-export/re-import roll-forward below.

Roll-forward depends on how the legacy pack got there:

| Origin | Roll-forward |
|---|---|
| first-party ghcr profile packs (seed refs, `varroactl import`) | republished with `kind`; re-import them |
| pull-through packs | self-healing — the next fetch of the same `name@version` rewrites the pack under the same stable ref |
| a store where pull-through was enabled and later **disabled** | does not self-heal; use the documented re-import path |
| third-party or locally built packs, and any `seed.refs` entry pointing at a registry other than the first-party one | **needs operator action** — see below |

The last case is the one to watch. `varroactl import` copies its source artifact through unchanged and the import handler never inspects `kind`, so **re-importing the same legacy artifact does not repair it**. Re-export a `kind`-bearing pack from the original plugin bytes and import that instead:

```bash
# profile pack
varroactl export plugins --profile <name> --to oci://<registry>/<repo>:<tag>
# single plugin
varroactl export plugin-addon --hpi ./<plugin>.hpi --to oci://<registry>/<repo>:<name>-<version>
```

## Evaluating store health

Check the overall phase and gaps:

```bash
kubectl get updatecenter varroa-update-center -o yaml
```

A non-empty `gaps` list means some plugins expected by your profiles or bundles are not in the store. Investigate by inspecting the specific `CoverageComplete` condition message. With pull-through **disabled** (air-gapped) this drives the phase to `Degraded`; with pull-through **enabled** the phase stays `Ready` (gaps are fetched on demand) and the gaps are informational — see [Readiness and pull-through](#statusconditions).

## Air-gapped setups

In a fully air-gapped cluster with no access to `updates.jenkins.io`:

1. Set `updateCenter.enabled=true` in your Helm values.
2. Use `storage.type=local` and pre-populate the PVC, or import via `varroactl import --to uc://`.
3. Set `pullThrough.enabled=false` (the default).
4. The operator will refuse to provision controllers until the update center reports `Ready` — the `WaitingForUpdateCenter` condition on `ProvisioningDefaults` or controllers will block provisioning. Pre-import all required plugin packs before enabling the update center for a seamless transition.

When the update center is **unavailable** but you have external access, set `updateCenter.enabled=false`. The operator and BFF then fall back to the public `updates.jenkins.io` for metadata; no blocking condition is set.

## Viewing update-center status (dashboard UI)

The Varroa dashboard provides a management page for the update center. Status, gaps and inventory are read-only; the upload section appears only for callers holding `updatecenter`/`upload` (see [Uploading a plugin](#uploading-a-plugin)).

### Navigation

From the dashboard sidebar, open **Admin & access** → **Update Center** (or navigate directly to `/administration/update-center`). The page is gated to the `admin` role — non-admin users receive a 403.

### Disabled state

If the `UpdateCenter` CR does not exist or the component has not been enabled (the Helm value `updateCenter.enabled` is `false`), the page shows a single centered message:

> Update Center is not enabled on this cluster.

No status card, gaps table, or inventory table is rendered. Refer to the `updateCenter.enabled` chart value to enable the component.

### Status card

When the update center is enabled and the CR exists, a status card appears at the top of the page showing:

- **Phase badge** — one of `Pending`, `Ready`, `Degraded`, or `Error`, styled with the page's badge tokens (`Ready` gets the accent colour, `Degraded`/`Error` get the warning/danger colours).
- **Plugin count and store size** — total number of plugins in the store and the human-readable byte size (e.g. `2.0 MB`).
- **Conditions** — the four condition types (`StorageReady`, `SeedImported`, `CoverageComplete`, `Ready`), each shown as a badge with an inline status label. Hovering over a condition's label reveals its `message` field (set by the reconciler), following the same `title`-attribute pattern used by the Versions tab.
- **Storage backend** — the configured storage type (`local` or `oci`).
- **Pull-through** — whether pull-through proxy is enabled or disabled.
- **Last sync time** — the `status.lastSyncTime` formatted in the browser's locale, or `never` if the update center has never synced.

### Gaps table

Whenever the `gaps` list is non-empty, a **Plugin gaps** table appears below the status card — independent of phase, so a pull-through update center can show `Ready` with gaps still listed. Each row shows the plugin name (with a warning-styled badge), the required version, and the profile or bundle that declared it. The table is hidden entirely when `gaps` is empty.

A gap means a plugin is declared in a `JenkinsVersionProfile` or `ComposedBundle` but is absent from the update center's store. Resolving gaps typically involves importing the missing plugin pack or updating the profile to remove the dependency.

### Plugin inventory table

Below the status card (and gaps table, if present) is the plugin inventory table. It lists every plugin currently stored in the update center, with columns for **Name**, **Version**, **SHA-256** (truncated to 12 hex characters), and **Size** (human-readable byte format).

A search input above the table lets you filter the list by plugin name. Filtering is case-insensitive and debounced (~250 ms): each keystroke sends a `?q=<query>` query parameter to the backend, so the list updates without a full page reload. An empty search box returns the full unfiltered inventory.

### Dashboard gaps chip

On the main dashboard page (Brood overview), the component performs an independent background fetch of `GET /api/v1/updatecenter`. If the update center is enabled and has one or more gaps, a small warning chip appears near the top of the dashboard:

> ⚠ N plugin(s) missing from Update Center

Clicking the chip navigates to the Update Center page. The chip is absent when the update center is disabled, has zero gaps, or when the fetch fails (a failed fetch never surfaces a dashboard-wide error banner — it is silently swallowed and logged at most).

(Screenshot placeholder: a manual follow-up will capture the dashboard chip and the Update Center settings page once a live cluster is available.)

### Controller plugin inventory and drift

Every connected controller reports the set of plugins actually installed on disk. This is distinct from the desired plugin set — the last list Varroa asked Jenkins to install — and is collected from the running Jenkins process itself.

**Collection.** The primary source is Jenkins' `GET /pluginManager/api/json?depth=2`, which returns each installed plugin's `shortName`, `version`, `enabled`, `detached`, `bundled`, and `dependencies` (as objects carrying the `optional` flag). The mite authenticates with its existing operator-signed JWT — no separate permission is needed. If Jenkins is unreachable (pre-boot, down, or the API returns an error), the mite falls back to scanning `$JENKINS_HOME/plugins` and parsing each `.hpi`/`.jpi` archive. The filesystem source cannot observe the `enabled`, `detached`, or `bundled` flags, so those are recorded as unknown. The mite retries the API on every collection tick — a down Jenkins that comes back up recovers on the next heartbeat without operator intervention.

**Transport.** A cheap `installed_plugins_hash` (a `v1:`-prefixed SHA-256 of a canonical JSON document over the normalized inventory) rides every heartbeat. The full inventory is pushed only when the hash changes, the source changes, or the operator issues an on-demand `COLLECT_PLUGIN_INVENTORY` command. It is never sent on a heartbeat.

**Closure-aware drift.** Comparing installed plugins against the declared set without considering the dependency graph produces a naive count that is larger than the actionable set. Declared plugins pull their own mandatory dependencies, and those are not drift — they are expected. The operator expands the declared set through the mandatory dependency graph reported by the mite before diffing against actual.

Measured on controller `smoke-mcp` (cluster `core`, 76 declared, 84 installed):

| step | count | what changed |
|---|---|---|
| installed | 84 | total plugins on disk |
| naive drift (installed \\ declared) | 8 | four of those are mandatory deps of declared plugins |
| closure-aware drift | 4 | `checks-api`, `echarts-api`, `jackson3-api`, `jquery3-api` are mandatory dependencies |
| actionable | 2 | two plugins are reachable from no expected root and are hand-installed |

A fourth mandatory-dependency plugin, `javax-mail-api`, is reachable only through an optional edge — it is classified as `optional-dependency` (advisory, never actionable). Treating `javax-mail-api` as actionable would be a false alarm.

**Provenance classes.** Every installed plugin is assigned exactly one class, evaluated in this order (first match wins):

| class | test | drift? |
|---|---|---|
| `bootstrap` | the closure root (`varroa-mite-auth`, derived from the embedded plugin lock) | no |
| `declared` | a member of the declared plugin set (resolved core set, controller spec, or composed bundle) | no |
| `jenkins-supplied` | `detached` or `bundled` is true (Jenkins put it on disk without being asked) | no |
| `dependency` | reachable from a class 1, 2, or 3 plugin through mandatory edges only | no |
| `optional-dependency` | reachable from an expected root through a path with at least one optional edge | advisory |
| `unmanaged` | none of the above — hand-installed and not expected | **actionable** |

`declared` is evaluated before `jenkins-supplied`: a plugin that is both (which every detached plugin in a normal lock set is) reports as `declared`. The operator asked for it; that it also happens to ship with core is the less useful fact.

**Version drift** is reported separately for `declared` plugins only. Each declared plugin is compared against its declared version: `ahead` (installed exceeds declared — `jenkins-plugin-cli` never downgrades, so this is sticky), `behind` (convergence has not run or failed), or `missing` (declared but not installed). A matching version is not reported.

**Degraded, indeterminate, and stale states.** Three conditions suppress the `PluginInventoryDrift` condition:

- **Degraded:** the inventory came from the filesystem fallback. The `detached` and `bundled` flags are unobservable, so the `jenkins-supplied` class cannot be assigned. A degraded inventory reports drift as advisory; it never sets the drift condition true.
- **Indeterminate:** optional dependency edges were dropped to fit the transport budget. Classes 1–4 remain exact, but classes 5 and 6 are collapsed into a single `unmanagedOrOptional` bucket. The drift condition is suppressed — the operator must not promote the combined bucket to actionable.
- **Stale:** the mite disconnected, collection failed, or the read model was evicted. The controller retains its last known inventory marked stale. A stale inventory never produces a drift signal.

The ladder is deliberate: a degraded or indeterminate inventory calling an unactionable set "actionable" would train operators to mute the alert, which is worse than not alerting at all.

**Status and endpoint.** A bounded summary — hash, collection and observation times, source, freshness and degradation flags, total count, per-class counts, and capped drift and version-drift rows — is written to `Controller.status.pluginInventory`. The full classified inventory with per-plugin metadata and attribution is persisted to the read model and served at:

```
GET /api/v1/clusters/{cluster}/controllers/{namespace}/{name}/plugins
```

This endpoint uses the existing controller-read authorization verb. It cross-checks the stored classification against `Controller.status` and returns `detailStale: true` when they disagree. When no classification has been stored, it returns the status summary with `detailAvailable: false`.

**This change detects and does not remediate.** No adopt, remove, or enforce action is available. A controller with `unmanaged` plugins sets the `PluginInventoryDrift` condition true, and the controller detail page surfaces them under a Plugins tab. Remediation is a later governance capability.

## Cross-cluster access (hive clusters)

The update center runs on the **core** cluster only — its Deployment, Service, PVC, and `UpdateCenter` CR are all gated off on hive clusters, and hive operators are not injected with an update-center URL. By default a controller on a hive therefore installs plugins straight from the public `updates.jenkins.io`, bypassing the core's store and pull-through cache entirely.

Pointing hive controllers at the core update center is a **manual, advanced setup** — there is no chart value that renders it. The pieces below are created by hand (with `kubectl apply`) and are not reconciled by Helm or the operator; if you delete them they stay gone, and a chart upgrade will not recreate or revert them. Weigh it against the alternative of simply letting hives pull from upstream, or running a separate update center per cluster (each core-mode install has its own).

The plugin-source precedence the operator applies is: **explicit `ProvisioningDefaults` URL → in-cluster update center (core only) → upstream `updates.jenkins.io`**. Because a hive has no in-cluster update center, the explicit `ProvisioningDefaults` override is the lever — and it is honoured regardless of the UC's phase, since it bypasses the readiness gate the in-cluster path waits on (see [Readiness and pull-through](#statusconditions)). The override remains necessary for hives even when the core UC reaches `Ready` on its own, because the core's in-cluster URL is never injected into hive operators.

**1. Expose the core update center.** It listens on a `ClusterIP` Service (`varroa-updatecenter:8080`); publish it to the LAN however your core cluster already exposes HTTP services. An Ingress is usually cleanest (NodePorts often fail cross-cluster behind a host firewall). Serve it over plain HTTP to avoid TLS-trust friction with the plugin installer:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: varroa-uc-ext
  namespace: varroa-system        # the core release namespace
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/proxy-body-size: "0"      # HPIs can be large
spec:
  ingressClassName: nginx
  rules:
    - host: uc.example.internal    # must resolve from hive pods to the core ingress
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: varroa-updatecenter
                port:
                  number: 8080
```

**2. Allow the ingress controller through the update center's NetworkPolicy.** The chart-shipped `varroa-np-updatecenter` admits ingress on `8080` only from controller pods (`app.kubernetes.io/managed-by: varroa-operator`, any namespace), the operator, and the BFF — the ingress controller is not on that list, so requests time out (`504 upstream timed out`). Add a second, additive policy (do not edit the chart-managed one):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: varroa-np-updatecenter-ingress-ext
  namespace: varroa-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: varroa-updatecenter
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
          podSelector:
            matchLabels:
              app.kubernetes.io/name: ingress-nginx
      ports:
        - port: 8080
          protocol: TCP
```

**3. Point the hive at the exposed endpoint** via the hive's cluster-scoped `ProvisioningDefaults/varroa-defaults`:

```bash
kubectl --context <hive> patch provisioningdefaults.varroa.dev varroa-defaults --type merge -p '{
  "spec": {
    "pluginUpdateCenterURL": "http://uc.example.internal/update-center.actual.json",
    "pluginUpdateCenterDownloadURL": "http://uc.example.internal/download"
  }
}'
```

New controllers on the hive (and existing ones on their next provision or plugin roll) now install through the core update center, exercising its pull-through cache and store.

> **Fallback is graceful.** If a hive controller requests a pinned version the update center cannot resolve (e.g. an aged pin that has dropped out of both the weekly and the matching `dynamic-stable` metadata — see [LTS-line resolution](#specpullthrough)), that download returns a 404 and the Jenkins plugin installer transparently falls back to `updates.jenkins.io` for just that plugin, computing the checksum on fetch. The controller still comes up; only the drifted pins skip the cache. The durable fix for such drift is refreshing the profile lock so pins match a resolvable version.

## See also

- [Air-gapped installation](../install/air-gapped.md) — full runbook for offline seeding and restricted-egress operation
- [Clusters](clusters.md) — core vs. hive roles and the per-cluster constraints on hive controllers

