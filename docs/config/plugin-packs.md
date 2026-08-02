# Plugin Packs

<!-- sources: internal/oci/pluginpack.go, cmd/varroactl/export.go, cmd/varroactl/import.go, docs/varroactl.md -->

A **plugin pack** is an OCI artifact that bundles Jenkins plugins — one layer per plugin — so a plugin set can be transferred, cached, and inspected with standard OCI tooling.

There are two **kinds** of pack:

| Kind | Contents | Built by |
|---|---|---|
| `profile` | a fully-resolved version-profile plugin set | `varroactl export plugins` |
| `addon` | exactly one standalone plugin | `varroactl export plugin-addon`, and the update center's pull-through cache |

The `kind` field is the only discriminator. A pack holding one plugin is not an addon by virtue of its size, and an empty `profile` field does not imply an addon — a reader must consult `kind`.

Plugin packs are produced by `varroactl export plugins` and `varroactl export plugin-addon`, and consumed by `varroactl import` and the in-cluster update center.

## Artifact format

Each pack is a single OCI manifest with the following media types and annotations.

### Manifest

| Field | Value |
|---|---|
| `artifactType` | `application/vnd.varroa.pluginpack.v1` |
| `config.mediaType` | `application/vnd.varroa.pluginpack.config.v1+json` |
| `layers[].mediaType` | `application/vnd.varroa.plugin.hpi.v1` |

### Config blob (JSON)

The config blob is a JSON object with these fields:

| Field | Type | Description |
|---|---|---|
| `kind` | string | `profile` or `addon`. Required — a pack with no `kind` is rejected on read |
| `jenkinsVersion` | string | For `profile`, the Jenkins version the pack was built for. For `addon`, the plugin's own `Jenkins-Version` (its minimum core), or empty if the manifest omits it |
| `profile` | string | For `profile`, the profile name passed to `--profile`, required non-empty. For `addon`, **always empty** |
| `lockHash` | string | SHA-256 hex of sorted `name@version` lines (order-independent) |
| `pluginCount` | integer | Number of plugin layers in the pack. `1` for an addon |
| `createdAt` | string | RFC 3339 timestamp of when the pack was built |

These invariants are validated **before** anything is pushed, so a rejected pack leaves the destination untouched.

### Layer annotations

Each layer (one per plugin) carries these annotations. The first three are always written; the rest are **omitted entirely** when they have no value, so a pack never carries misleading empty metadata. A reader must treat all six optional keys as absent-by-default.

| Annotation | Type | Value |
|---|---|---|
| `dev.varroa.plugin.name` | string | Artifact ID of the plugin |
| `dev.varroa.plugin.version` | string | Pinned version string |
| `dev.varroa.plugin.sha256` | string | `sha256:<hex>` digest of the layer content |
| `dev.varroa.plugin.upstreamUrl` | string | Download URL the content was fetched from. Absent on an addon built from a local file |
| `dev.varroa.plugin.displayName` | string | The plugin's `Long-Name`, from its HPI manifest |
| `dev.varroa.plugin.requiredCore` | string | The plugin's `Jenkins-Version` — the minimum Jenkins core it needs — from its HPI manifest |
| `dev.varroa.plugin.dependencies` | JSON array | The plugin's `Plugin-Dependencies`, from its HPI manifest (see below) |
| `dev.varroa.plugin.description` | string | Operator-supplied via `--description`. Addon packs only |
| `dev.varroa.plugin.tags` | JSON array of strings | Operator-supplied via repeated `--tag`. Addon packs only |

The three HPI-derived annotations (`displayName`, `requiredCore`, `dependencies`) are read out of the plugin's own `META-INF/MANIFEST.MF` by every producer, because every producer already holds the plugin bytes when it builds the pack. If a plugin's manifest cannot be parsed, a bulk producer omits all three, logs a warning, and packs the plugin anyway — identity for a profile pack comes from the resolved lock entry, so the pack is still correct and installable. `varroactl export plugins` names the affected plugins in its JSON output.

#### Dependency array shape

`dev.varroa.plugin.dependencies` is a JSON array preserving the order the plugin declared:

```json
[{"name":"mailer","min":"534.v1b_36f5864073","optional":false},
 {"name":"configuration-as-code","min":"2082.vdb_db_4622e9fa_","optional":true}]
```

`min` is a **minimum, not a pin**, recorded verbatim from the manifest. A resolved version newer than `min` satisfies it; only an older one conflicts. Varroa never normalizes or truncates the string, so a consumer comparing against it sees exactly what the plugin declared.

`optional` mirrors the manifest's `;resolution:=optional` marker. Jenkins tolerates an absent optional dependency, so nothing in Varroa requires one.

A **malformed** `tags` or `dependencies` value is an error on read, not a silently empty list: a corrupted dependency list must not read as "no dependencies".

### Manifest annotations

| Annotation | Value |
|---|---|
| `dev.varroa.pack.profile` | Profile name (same as `--profile`) |
| `dev.varroa.pack.jenkinsVersion` | Jenkins version string |
| `dev.varroa.pack.lockHash` | Same value as the config blob's `lockHash` (for fast index-time inspection) |

`kind` is a config-blob field only. It is deliberately **not** mirrored as a manifest annotation, so there is exactly one place a reader can learn a pack's kind.

## Dual-tag strategy

When `varroactl export plugins --to oci://...` is given a destination **without** an explicit `:<tag>`, it writes two tags:

1. A **floating** `<profile>` tag that always points to the latest build of that profile.
2. An **immutable** `<profile>-<lockHash12>` tag pinned to that specific plugin closure.

The dual-tag scheme makes it safe to pull `<profile>` in automation (always the latest) while retaining the ability to reference a specific historical closure by its lock-hash suffix.

When an explicit `:<tag>` is given, only that single tag is written. This is what CI seed-pack publishing does — the workflow in `.github/workflows/release.yaml` passes `<profileName>` as the tag, so the pack lands at `ghcr.io/varroaci/varroa-jenkins/plugin-pack:<profileName>` with no dual-tag.

Addon packs never dual-tag. A profile tag is mutable, so callers need a content-derived tag to pin the exact closure; an addon holds a single plugin whose version is already in its recommended tag, and a second tag would name the same thing twice.

## Usage

### Export a pack from a local lock file (offline)

```bash
# Generate the --plugins-file from the lockfile (one-liner).
# This exports only the baseline version's plugins set — there is no --jenkins-version
# flag because the version is determined by the profile entry in the lockfile.
cat > /tmp/plugins.yaml <<PYEOF
core:
  - configuration-as-code
plugins:
  - artifactId: configuration-as-code
    version: 2100.vb_fd699d2a_09c
PYEOF

varroactl export plugins \
  --profile 2-555 \
  --plugins-file /tmp/plugins.yaml \
  --to oci://ghcr.io/myorg/plugin-pack:2-555
```

### Export a pack resolved live from the update center

```bash
varroactl export plugins \
  --profile 2-555 \
  --to oci://ghcr.io/myorg/plugin-pack
```

When no `--plugins-file` is given, `varroactl` resolves the plugin closure live from `updates.jenkins.io` using the profile name to look up version metadata.

### LTS-line dynamic-stable fallback

For LTS-line profiles with a `resolveVersion` (e.g., a profile pinning Jenkins `2.555` with `resolveVersion: "2.555.3"`), the export process performs a two-phase sha256 metadata lookup:

1. **Phase 1** — fetches the *current* `update-center.actual.json` from `--download-url-base` and looks up each plugin's declared sha256.
2. **Fallback (Phase 2)** — if the current metadata reports a version mismatch (the pinned plugin version is older than the current weekly), and `resolveVersion` is set, the process automatically retries the sha256 lookup against `<downloadURLBase>/dynamic-stable-<resolveVersion>/update-center.actual.json`.

**Important:** the `.hpi` blob download URL always uses the *original* `--download-url-base` root — it is never redirected to the `dynamic-stable` path. The `--download-url-base` flag still means "root base for both metadata and blobs." The dynamic-stable derivation is an internal, automatic fallback, not a new flag.

### Export to a local directory (OCI layout)

```bash
varroactl export plugins \
  --profile 2-555 \
  --to dir:///tmp/plugin-pack-2-555
```

### Export to a tar archive

```bash
varroactl export plugins \
  --profile 2-555 \
  --to tar:///tmp/plugin-pack-2-555.tar.gz
```

### Import (copy) a pack between destinations

```bash
# Pull from a registry into a local directory
varroactl import \
  --from oci://ghcr.io/myorg/plugin-pack:2-555 \
  --to dir:///tmp/plugin-pack-copy
```

### Registry auth

For `oci://` destinations, use `--registry-config` to point to a Docker `config.json` and `--insecure` to disable TLS:

```bash
varroactl export plugins \
  --profile 2-555 \
  --to oci://localhost:5000/plugin-pack:2-555 \
  --registry-config ~/.docker/config.json \
  --insecure
```

## `uc://` scheme

The `uc://` scheme is recognised syntactically but is **not functional** in this build. It requires the update-center service (C2), which is not yet implemented. Attempting to use it prints:

```
uc:// requires the update-center service (not available in this build)
```

## Seed packs published by CI

The `release.yaml` workflow publishes a plugin pack for each version profile during every release. The tags follow:

```
ghcr.io/varroaci/varroa-jenkins/plugin-pack:<profileName>
```

For example, a profile named `2-555` targeting Jenkins `2.555` publishes to:

```
ghcr.io/varroaci/varroa-jenkins/plugin-pack:2-555
```

These are single-tag packs (no immutable dual-tag) — the workflow passes the profile name explicitly as the tag to `--to`.

## See also

- [varroactl CLI reference](../varroactl.md) — full flag reference for `export plugins` and `import`
- [Jenkins versions](jenkins-versions.md) — version profiles and plugin locks
- [Plugin pinning](plugin-pinning.md) — where plugin lists come from and how they combine
- [Air-gapped installation](../install/air-gapped.md) — offline export/import runbook for restricted-egress clusters
