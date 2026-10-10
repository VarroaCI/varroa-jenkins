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
  --profile jenkins-version-2-555 \
  --to oci://ghcr.io/varroaci/plugin-pack
```

Use `--plugins-file` to supply a local lock instead of resolving it from the
cluster. A seed pack should contain the full dependency closure so startup does
not require update-center pull-through.

Destinations without an explicit OCI tag receive a floating profile tag and a
content-addressed tag containing the lock hash. An explicit tag writes only that tag.
Export fails when a plugin checksum cannot be resolved or verified.
A plugin at `latest`, or with no version in `--plugins-file`, resolves to a concrete version before download. For a profile, the newest version that the profile's core runs is used. In offline mode, the newest listed version is used. Export fails when no metadata source lists a usable version.

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

Sources and destinations support `oci://`, `dir://`, and `tar://`:

```bash
varroactl import \
  --from oci://ghcr.io/varroaci/plugin-pack:jenkins-version-2-555 \
  --to dir:///tmp/plugin-pack
```

Upload to an update center with a destination-only `uc://` URL:

```bash
export VARROACTL_UC_TOKEN='<import-token>'
varroactl import \
  --from tar:///tmp/plugin-pack-2-555.tar.gz \
  --to uc://update-center.example.com
```

The update center rejects a pack that holds a plugin at `latest` or with no
version. The error names the plugin. Export the pack again with a concrete
version.

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
dependencies annotation means the list is unknown, not empty.

See [update center operations](../operations/update-center.md) for seeding and
pull-through behavior.
