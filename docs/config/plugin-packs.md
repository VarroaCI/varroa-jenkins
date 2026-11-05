# Build and move plugin packs

A plugin pack is an OCI artifact containing checksum-verified Jenkins plugin
files.

| Kind | Contents | Export command |
|---|---|---|
| `profile` | Resolved version-profile plugin set | `varroactl export plugins` |
| `addon` | One local HPI and its manifest metadata | `varroactl export plugin-addon` |

## Export a profile pack

```bash
varroactl export plugins \
  --profile jenkins-version-2-580 \
  --to oci://ghcr.io/varroaci/plugin-pack
```

Use `--plugins-file` to supply a local lock instead of resolving it from the
cluster. A seed pack should contain the full dependency closure so startup does
not require update-center pull-through.

Destinations without an explicit OCI tag receive a floating profile tag and a
content-addressed tag containing the lock hash. An explicit tag writes only that tag.
Export fails when a plugin checksum cannot be resolved or verified.
A plugin at `latest`, or with no version in `--plugins-file`, resolves to a concrete version before download. For a profile, the newest version that the profile's core runs is used. For a profile, the export reads the LTS metadata of the highest core tier at or below the profile core. It reads the tier list from `tiers.json` under the download base. When that read fails, it uses the profile core. In offline mode, the newest listed version is used. Export fails when no metadata source lists a usable version.
A metadata source that fails is not fetched again during the same export.

## Export one addon

```bash
varroactl export plugin-addon \
  --hpi ./example-plugin.hpi \
  --to oci://ghcr.io/varroaci/plugin-addon:example-plugin-1.0.0 \
  --description "Example plugin" \
  --tag approved
```

The HPI manifest supplies the plugin name, version, required core, and
dependencies. Addon packs contain exactly one plugin.

## Copy or import

For a complete air-gap transfer that includes the chart and the images, use [release bundles](../install/air-gapped.md#transfer-one-bundle). The commands below move one pack.

Sources and destinations support `oci://`, `dir://`, and `tar://`:

```bash
varroactl import \
  --from oci://ghcr.io/varroaci/plugin-pack:jenkins-version-2-580 \
  --to dir:///tmp/plugin-pack
```

Upload to an update center with a destination-only `uc://` URL:

```bash
export VARROACTL_UC_TOKEN='<import-token>'
varroactl import \
  --from tar:///tmp/plugin-pack-2-580.tar.gz \
  --to uc://update-center.example.com
```

The update center rejects a pack that holds a plugin at `latest` or with no
version. The error names the plugin. Export the pack again with a concrete
version.

The update center reads each pack before it stores anything. It also rejects a pack with no kind or an unknown kind. It rejects a manifest that is not a plugin pack. It rejects a pack with a layer that is not a plugin blob, a pack with a missing blob, and a layout with no manifest. A rejected import stores nothing, and `varroactl import` exits with code 1. A copy failure answers `500` and can leave earlier packs of the same import in the store. Import the packs again after the fault clears. A pack with no kind comes from an old export. Export it again with the current `varroactl`.

Use `--registry-config` for registry credentials. Restrict `--insecure` to
trusted development networks.

## Validate the artifact

| Part | Media type |
|---|---|
| Manifest `artifactType` | `application/vnd.varroa.pluginpack.v1` |
| Config | `application/vnd.varroa.pluginpack.config.v1+json` |
| Plugin layer | `application/vnd.varroa.plugin.hpi.v1` |

Consumers reject missing or unknown pack kinds, checksum mismatches, and
malformed structured annotations. Dependency versions in addon metadata are
minimums, not exact pins. An empty dependency list is written as `[]`. A missing
dependencies annotation means the list is unknown, not empty. When the stored manifest is also unreadable, each derived catalog item that needs the plugin is invalid.

See [update center operations](../operations/update-center.md) for seeding and
pull-through behavior.
