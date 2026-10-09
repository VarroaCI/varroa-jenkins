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

```bash
varroactl upload plugin <file.hpi>
varroactl import --from <source> --to <destination>
```

Enable pull-through only when the update center may reach its configured upstream and archive hosts. In an air-gapped environment, seed or import every required plugin before deploying controllers. See [Air-gapped installation](../install/air-gapped.md) and [Plugin packs](../config/plugin-packs.md).

`seed.secretRef` names a `.dockerconfigjson` or `username`/`password` Secret in the operator namespace. It holds pull credentials for every ref in `seed.refs`, so it must contain exactly one registry entry.

## Set an explicit download URL

The value of `ProvisioningDefaults.spec.pluginUpdateCenterDownloadURL` must be `<mirror>/download/plugins`, for example `https://updates.jenkins.io/download/plugins`. The plugin installer appends `<name>/<version>/<name>.hpi` to it. When the first pin answers `404` or `410`, `plugins-init` stops at once. When a later download fails, the installer fetches it from `https://archives.jenkins.io/`. The controller reports both cases as `UpdateCenterFallback=True`:

| Reason | Meaning |
|---|---|
| `DownloadURLNotFound` | The download URL answers `404` or `410` for the first pin. |
| `PluginArchiveFallback` | Some plugins came from the public archive. The message names the first one. |

## Check readiness

```bash
kubectl get updatecenter varroa-update-center
varroactl get versionprofiles
```

The update center reports two signals about the declared plugins:

- `CoverageComplete`, `status.gapCount`, and `status.gaps` report declared plugins that are not in the store. The list is informational while pull-through is on, because pull-through fetches a missing plugin on demand. Check it before an air-gapped cutover.
- `PluginsServable`, `status.unservableCount`, and `status.unservable` report declared plugins the update center cannot serve, from the store or by pull-through. Each entry has a reason:

| Reason | Meaning |
|---|---|
| `NotListed` | No upstream source lists the version, and the archive checksum fallback has no record of it. |
| `ArchiveDisabled` | The version is an aged pin. Upstream metadata no longer lists it, and `updateCenter.pullThrough.archiveURL` is empty. |
| `PullThroughDisabled` | The plugin is not stored, and pull-through is off. |

A plugin pinned at `latest` counts as stored when the store holds any version of it. The update center serves the version that `update-center.actual.json` advertises. When the store holds no version, pull-through fetches the newest version that upstream metadata lists. A `latest` pin that no source lists reports `NotListed`.

`Ready` follows `PluginsServable`. An unservable plugin sets `Ready=False`, and the phase becomes `Degraded`. An `Unknown` `PluginsServable` does not block `Ready`. It means upstream could not answer for some plugins.

Resolve an unservable plugin by seeding the artifact, restoring the archive URL, or changing the version profile. A member cluster needs its own enabled and populated update center when its controllers cannot reach the core service.
