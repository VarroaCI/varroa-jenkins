#!/usr/bin/env bash
# build-plugin-pack.sh — build and publish an OCI plugin pack for a profile
#
# Requirements: yq v4, the ALREADY-BUILT varroactl binary (make build-cli),
#               network access to updates.jenkins.io (for plugin downloads).
#
# Reads internal/controller/pluginlock/lock.yaml for the version profile's
# resolved plugin set, writes a temporary --plugins-file in the YAML shape
# varroactl export plugins expects, then invokes varroactl to download, verify,
# and push the pack as an OCI artifact.
#
# The pack holds the FULL dependency closure (sets.<v>.plugins), not just the
# top-level sets.<v>.core list — see the comment above the plugins-file write.
#
# Usage:
#   hack/build-plugin-pack.sh <profileName> <version> <ghcr-tag-base>
#
# Example:
#   hack/build-plugin-pack.sh 2-555 2.555 ghcr.io/varroaci/varroa-jenkins
#
# The resulting OCI tag is <ghcr-tag-base>/plugin-pack:<profileName>.
# Only that single tag is written (no immutable dual-tag — by design for
# CI seed packs).
#
# The temp --plugins-file is cleaned up on exit via trap.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCKFILE="${REPO_ROOT}/internal/controller/pluginlock/lock.yaml"
VARROACTL="${REPO_ROOT}/bin/varroactl"

if [ $# -ne 3 ]; then
    echo "Usage: $0 <profileName> <version> <ghcr-tag-base>" >&2
    echo "Example: $0 2-555 2.555 ghcr.io/varroaci/varroa-jenkins" >&2
    exit 1
fi

PROFILE_NAME="$1"
VERSION="$2"
GHCR_TAG_BASE="$3"

# --- yq v4 prerequisite ------------------------------------------------
if ! command -v yq >/dev/null 2>&1; then
    echo "ERROR: yq v4 is required but not found on PATH. Install mikefarah/yq v4." >&2
    exit 1
fi
if ! yq --version 2>&1 | grep -q ' v4\.'; then
    echo "ERROR: yq v4 is required (found: $(yq --version 2>&1)). Install mikefarah/yq v4." >&2
    exit 1
fi

# --- varroactl prerequisite -------------------------------------------
if [ ! -x "$VARROACTL" ]; then
    echo "ERROR: varroactl binary not found at $VARROACTL — run 'make build-cli' first" >&2
    exit 1
fi

if [ ! -f "$LOCKFILE" ]; then
    echo "ERROR: lockfile not found: $LOCKFILE" >&2
    exit 1
fi

# --- Parse the lock file for the requested version --------------------
# Extract core: and plugins: entries for the given version from
# lock.yaml's sets.<version> subtree.
set_entry="$(yq eval ".sets.\"${VERSION}\"" "$LOCKFILE")"
if [ -z "$set_entry" ] || [ "$set_entry" = "null" ]; then
    echo "ERROR: version ${VERSION} not found in lock.yaml sets" >&2
    exit 1
fi

# --- Write temp plugins-file ------------------------------------------
PLUGINS_FILE="$(mktemp)"
trap 'rm -f "$PLUGINS_FILE"' EXIT

# Deliberately NO core: section.
#
# `varroactl export plugins` treats an explicit `core:` list as "export exactly
# these"; omitting it exports every entry under `plugins:`. `sets.<v>.core` is the
# 15 top-level plugins we asked for, while `sets.<v>.plugins` is their resolved
# 98-plugin dependency closure — so writing core: produced a pack holding 15 of the
# 98 plugins a controller actually needs.
#
# A pack exists to seed an update center, and a seeded update center is what lets a
# controller install its plugins without reaching the internet. Covering only the
# top-level 15 leaves the other 83 to pull-through at request time, so every
# controller still depends on live upstream egress. That is the failure mode the
# scale campaign hit: 400 controllers x ~196 plugins from one NAT egress IP was
# throttled by the mirror network down to ~3.2 plugins/min.
#
# Aged pins add a second dependency. Measured against the weekly on 2026-08-22,
# three closure members had already aged past current metadata (jackson2-api, junit,
# workflow-cps) and none were in core:. Pull-through *can* still serve those — the
# update center shares ucmeta's archive fallback and the chart enables it by default
# (updateCenter.pullThrough.archiveURL) — but only by reaching repo.jenkins-ci.org,
# a different host from updates.jenkins.io that a narrowed egress allowlist may
# block and that archiveURL can switch off outright. Packing them removes that
# dependency instead of relying on it.
{
    echo "plugins:"
} > "$PLUGINS_FILE"

# Write each plugin entry with yq (one line at a time to avoid quoting woes).
while IFS= read -r aid; do
    ver="$(yq eval ".sets.\"${VERSION}\".plugins[] | select(.artifactId == \"${aid}\") | .version" "$LOCKFILE")"
    printf "  - artifactId: %s\n    version: %s\n" "$aid" "$ver" >> "$PLUGINS_FILE"
done < <(yq eval ".sets.\"${VERSION}\".plugins[].artifactId" "$LOCKFILE")

# --- Run varroactl export ---------------------------------------------
"$VARROACTL" export plugins \
    --profile "$PROFILE_NAME" \
    --plugins-file "$PLUGINS_FILE" \
    --to "oci://${GHCR_TAG_BASE}/plugin-pack:${PROFILE_NAME}"
