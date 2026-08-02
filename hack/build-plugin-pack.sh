#!/usr/bin/env bash
# build-plugin-pack.sh — build and publish an OCI plugin pack for a profile
#
# Requirements: yq v4, the ALREADY-BUILT varroactl binary (make build-cli),
#               network access to updates.jenkins.io (for plugin downloads).
#
# Reads internal/controller/pluginlock/lock.yaml for the version profile's
# resolved plugin set, writes a temporary --plugins-file in the YAML shape
# varroactl export plugins expects (core:/plugins:), then invokes varroactl
# to download, verify, and push the pack as an OCI artifact.
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

# Write core: section
{
    echo "core:"
    yq eval ".sets.\"${VERSION}\".core[] | \"  - \" + ." "$LOCKFILE"

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
