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

The update center reports gaps when a pinned plugin is unavailable. Resolve gaps by adding the required artifact or changing the version profile. A member cluster needs its own enabled and populated update center when its controllers cannot reach the core service.
