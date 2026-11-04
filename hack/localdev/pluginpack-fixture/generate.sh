#!/usr/bin/env bash
# =============================================================================
# Generate the checked-in OCI-layout fixture for offline localdev seeding.
# Needs Go and network access to updates.jenkins.io. An old pin can also need
# repo.jenkins-ci.org for its checksum.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

FIXTURE=hack/localdev/pluginpack-fixture
LOCKFILE=internal/controller/pluginlock/lock.yaml

# The pack names the profile of the lock baseline, because the sample
# controller sets no version and runs the baseline. Keep this parse equal to
# baseline_pack_tag in hack/localdev/localdev.sh.
baseline=$(sed -n 's/^baseline:[[:space:]]*"\{0,1\}\([0-9][0-9.]*\).*/\1/p' "$LOCKFILE")
if [ -z "$baseline" ]; then
  echo "ERROR: cannot read baseline from $LOCKFILE" >&2
  exit 1
fi
profile="jenkins-version-${baseline//./-}"

# Build varroactl from this tree, so the pack format matches the code.
echo ">>> Building bin/varroactl"
make build-cli

# An export into an existing layout keeps the old manifest in index.json.
# Remove the old layout first.
rm -rf "$FIXTURE/blobs" "$FIXTURE/index.json" "$FIXTURE/oci-layout"

# The destination path is also the manifest ref name. localdev.sh imports
# the same relative path, so run the export from the repository root.
echo ">>> Exporting plugin pack $profile to $FIXTURE/"
bin/varroactl export plugins \
  --profile "$profile" \
  --plugins-file hack/localdev/pluginpack-fixture-plugins.yaml \
  --to "dir://$FIXTURE"

echo ">>> Validating $FIXTURE/"
go run ./hack/localdev/cmd/validate-oci-layout --dir "$FIXTURE"

echo ">>> Done. OCI layout written to $FIXTURE/"
