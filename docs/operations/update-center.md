# Update Center

The optional update center serves the exact Jenkins plugins selected by version profiles. Enable it when controllers must use an in-cluster plugin source, when egress is restricted, or when plugin artifacts need controlled retention.

## Enable storage and service

```yaml
updateCenter:
  enabled: true
  storage:
    type: local
    local:
      size: 10Gi
```

`local` uses a PVC. `oci` stores content in an OCI registry and requires `storage.oci.ref`; configure `storage.oci.existingSecret` for private registry access. Local storage runs a single writer. Uploads also require a single writer.

## Populate plugins

Choose one or more sources:

| Source | Use |
|---|---|
| `seed.refs` | Import known OCI plugin packs during reconciliation. |
| Pull-through | Fetch missing artifacts from the configured upstream. |
| Upload | Add an `.hpi` through the authenticated API or CLI. |
| Import | Transfer a plugin pack from an OCI, directory, or tar source. |
| Prefetch | Fetch every declared plugin that pull-through can serve, before a controller asks for it. |

```bash
varroactl upload plugin <file.hpi>
varroactl import --from <source> --to <destination>
```

An upload checks each mandatory dependency against the declared plugin set. A dependency declared only at `latest` is checked against the version it resolves to: the highest stored version, or else the newest version that upstream metadata lists. A result below the minimum gives a `lock-too-old` warning. A pinned declaration uses the highest pin. An upload reads each stored dependency's list from its annotation, else from its stored manifest, else from upstream metadata.

A bundle plugin that declares no version is declared at `latest`. An upload never fetches it. The served metadata fills it from upstream for the requesting core.

Enable pull-through only when the update center may reach its configured upstream and archive hosts. In an air-gapped environment, seed or import every required plugin before deploying controllers. See [Air-gapped installation](../install/air-gapped.md) and [Plugin packs](../config/plugin-packs.md).

`seed.secretRef` names a `.dockerconfigjson` or `username`/`password` Secret in the operator namespace. It holds pull credentials for every ref in `seed.refs`, so it must contain exactly one registry entry.

## Prefetch declared plugins

Pull-through stores a plugin only when a Jenkins pod requests it. A declared pin that no pod requests, such as a pin in an unused profile or an open candidate, stays a gap. Set prefetch to fetch these pins before any request:

```yaml
updateCenter:
  pullThrough:
    enabled: true
    prefetch: true
```

Prefetch requires `pullThrough.enabled`. The chart fails to render when prefetch is on and pull-through is off. The operator reads the import token from the `varroa-updatecenter-import-token` Secret. It posts the servable gaps to `POST /api/v1/prefetch`. Each reconcile posts at most 20 plugins. A plugin that fails waits 10 minutes before the operator posts it again.

The `PluginsPrefetched` condition reports the state:

| Reason | Status | Meaning |
|---|---|---|
| `PrefetchComplete` | True | Every servable declared plugin is in the store. |
| `PrefetchInProgress` | False | The operator fetched a batch, and more servable plugins remain. |
| `PrefetchFailed` | False | At least one plugin failed to fetch, or every remaining plugin waits for a retry. |
| `TokenUnavailable` | False | The import token Secret or its key is missing. |
| `TokenRejected` | False | The update center rejected the import token. |
| `PrefetchUnsupported` | False | The update center does not serve the prefetch endpoint. |
| `ServabilityUnavailable` | Unknown | The servability check did not answer, so prefetch waits. |

`PluginsPrefetched` is informational. It does not gate `Ready` or the phase. When prefetch is off, the operator removes the condition.

## Set an explicit download URL

The value of `ProvisioningDefaults.spec.pluginUpdateCenterDownloadURL` must be `<mirror>/download/plugins`, for example `https://updates.jenkins.io/download/plugins`. The plugin installer appends `<name>/<version>/<name>.hpi` to it. When the first pin answers `404` or `410`, `plugins-init` stops at once. When a later download fails, the installer fetches it from `https://archives.jenkins.io/`. The controller reports both cases as `UpdateCenterFallback=True`:

| Reason | Meaning |
|---|---|
| `DownloadURLNotFound` | The download URL answers `404` or `410` for the first pin. |
| `PluginArchiveFallback` | Some plugins came from the public archive. The message names the first one. |

## Plugin dependencies

The metadata document `update-center.actual.json` lists the dependencies of each served plugin. Each entry names a dependency, its minimum version, and whether it is optional. The list comes from the plugin's own manifest. The document also gives each plugin its required Jenkins core version. The served entry always carries `requiredCore`, which is an empty string when the plugin declares none.

The Jenkins plugin installer reads these entries for the `latest` version of a plugin. For an explicit version, the installer does not read these entries from this document.

A plugin whose manifest cannot be read is left out of the metadata document. The service logs a warning. The plugin stays downloadable at its explicit version. The operator reports these plugins in `status.unknownDependencies`. See [Check readiness](#check-readiness). A slow blob store can delay a manifest read. The inventory waits at most 15 seconds for these reads, reports a plugin whose read is not complete as unknown, and lists it on the next pass.

## Check readiness

```bash
kubectl get updatecenter varroa-update-center
varroactl get versionprofiles
```

The update center reports two signals about the declared plugins:

- `CoverageComplete`, `status.gapCount`, and `status.gaps` report declared plugins that are not in the store. The list is informational while pull-through is on, because pull-through fetches a missing plugin on demand. Check it before an air-gapped cutover. With prefetch on, `CoverageComplete` reaches True without a controller request.
- `PluginsServable`, `status.unservableCount`, and `status.unservable` report declared plugins the update center cannot serve, from the store or by pull-through. Each entry has a reason:

| Reason | Meaning |
|---|---|
| `NotListed` | No upstream source lists the version, and the archive checksum fallback has no record of it. |
| `ArchiveDisabled` | The version is an aged pin. Upstream metadata no longer lists it, and `updateCenter.pullThrough.archiveURL` is empty. |
| `PullThroughDisabled` | The plugin is not stored, and pull-through is off. |

A `ProfileCandidate` uses the same servability answers for its own closure.

- `DependenciesKnown`, `status.unknownDependencyCount`, and `status.unknownDependencies` report stored plugin versions whose dependency list the update center cannot read. The list holds the first 50 entries, sorted by name and version. The `DependenciesKnown` condition is `False` while any such version exists. These fields are informational. They do not affect `Ready`.

A plugin pinned at `latest` counts as stored when the store holds a version that runs on the lowest core that requests it. The Jenkins plugin installer sends the controller core version in the metadata request (`?version=<core>`). For that request, the update center advertises the highest stored version that the core runs. If no stored version runs on the core, it advertises the highest stored version, and the installer stops with a requires-a-greater-version-of-Jenkins error. A plugin that the declared set pins at a stored version always advertises that pinned version, whatever core the request names. With pull-through on, a declared `latest` plugin with no compatible stored version gets the newest compatible version from upstream metadata. The update center reads upstream metadata from the weekly source and from the LTS line of each version profile. A controller whose LTS line has no version profile reads only the weekly source. The download path does not carry a core version. It serves the stored version that the metadata advertises without a core filter. A `latest` pin that no source lists reports `NotListed`.

The lowest requesting core is the profile core, the candidate core, or the lowest deployed core of the controllers that use the bundle. A `latest` entry in `status.gaps` and `status.unservable` shows that core in its `core` field. A controller on a two-segment version, such as `2.479`, requests that weekly core. The `jenkins/jenkins:2.479` image runs weekly 2.479, not an LTS patch. A profile `resolveVersion` replaces the version. A controller on the default baseline requests the baseline core, which is a weekly release. A bundle that no controller uses is checked without a core. Controllers in member clusters are not counted. A controller that sets its Jenkins image through `resourceOverlay` is read by its `spec.version`, not by the overlay tag.

`Ready` follows `PluginsServable`. An unservable plugin sets `Ready=False`, and the phase becomes `Degraded`. An `Unknown` `PluginsServable` does not block `Ready`. It means upstream could not answer for some plugins.

Resolve an unservable plugin by seeding the artifact, restoring the archive URL, or changing the version profile. A member cluster needs its own enabled and populated update center when its controllers cannot reach the core service.
