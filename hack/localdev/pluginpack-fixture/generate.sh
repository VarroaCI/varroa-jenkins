#!/usr/bin/env bash
# =============================================================================
# Generate the checked-in OCI-layout fixture for offline localdev seeding.
# Needs network access to updates.jenkins.io for plugin metadata resolution.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# Build bin/varroactl from the backend image if absent.
if [ ! -f bin/varroactl ]; then
  echo ">>> bin/varroactl not found — building backend image and extracting..."
  mkdir -p bin
  docker build --provenance=false --sbom=false -t varroa-jenkins:build "$REPO_ROOT" >/dev/null
  container_id=$(docker create varroa-jenkins:build)
  docker cp "$container_id:/app/varroactl" bin/varroactl
  docker rm "$container_id" >/dev/null
  chmod +x bin/varroactl
fi

echo ">>> Exporting plugin pack to hack/localdev/pluginpack-fixture/"
bin/varroactl export plugins \
  --profile jenkins-version-2-555 \
  --plugins-file hack/localdev/pluginpack-fixture-plugins.yaml \
  --to "dir://hack/localdev/pluginpack-fixture"

echo ">>> Done. OCI layout written to hack/localdev/pluginpack-fixture/"
