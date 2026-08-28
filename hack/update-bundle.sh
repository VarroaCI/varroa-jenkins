#!/bin/bash
# Update a Varroa bundle and push the change to a running controller.
# Usage: ./hack/update-bundle.sh <controller-name> <bundle-dir>
set -euo pipefail

CONTROLLER="${1:?controller name required}"
BUNDLE_DIR="${2:?bundle directory in repo required}"
NAMESPACE="${NAMESPACE:-varroa}"
BUNDLE_REPO="${BUNDLE_REPO:?bundle repo git URL required (export BUNDLE_REPO=git@github.com:your-org/your-casc-bundle-repo.git)}"

WORKDIR="$(mktemp -d)"
trap "rm -rf $WORKDIR" EXIT

echo "==> Cloning bundle repo"
git clone "$BUNDLE_REPO" "$WORKDIR"
cd "$WORKDIR"

# Apply user edits from stdin if provided
if [ ! -t 0 ]; then
  echo "==> Applying edits from stdin"
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    # Format: "file:content" or "file<<EOF ... EOF"
    file="${line%%:*}"
    content="${line#*:}"
    if [ "$file" != "$line" ]; then
      echo "$content" >> "$file"
    fi
  done
fi

# If files were passed as env vars, use them
if [ -n "${JENKINS_YAML:-}" ]; then
  echo "${JENKINS_YAML}" > "${BUNDLE_DIR}/jenkins.yaml"
fi
if [ -n "${PLUGINS_YAML:-}" ]; then
  echo "${PLUGINS_YAML}" > "${BUNDLE_DIR}/plugins.yaml"
fi
if [ -n "${VARIABLES_YAML:-}" ]; then
  echo "${VARIABLES_YAML}" > "${BUNDLE_DIR}/variables.yaml"
fi

echo "==> Committing and pushing"
git add "${BUNDLE_DIR}/"
if git diff --cached --quiet; then
  echo "No changes to commit"
  exit 0
fi
git commit -m "update ${BUNDLE_DIR} bundle"
git push origin main
NEW_SHA=$(git rev-parse HEAD)
echo "==> Pushed: ${NEW_SHA}"

echo "==> Patching controller ${CONTROLLER} to revision ${NEW_SHA}"
PREV_HASH=$(kubectl get controller -n "$NAMESPACE" "$CONTROLLER" -o jsonpath='{.status.configHash}')
kubectl patch controller -n "$NAMESPACE" "$CONTROLLER" --type=merge \
  -p "{\"spec\":{\"bundleRef\":{\"revision\":\"${NEW_SHA}\"}}}"

echo "==> Previous configHash: ${PREV_HASH}"
echo "==> Waiting for configHash change..."
for i in $(seq 1 30); do
  NEW_HASH=$(kubectl get controller -n "$NAMESPACE" "$CONTROLLER" -o jsonpath='{.status.configHash}')
  PHASE=$(kubectl get controller -n "$NAMESPACE" "$CONTROLLER" -o jsonpath='{.status.phase}')
  if [ -n "$NEW_HASH" ] && [ "$NEW_HASH" != "$PREV_HASH" ]; then
    echo "==> Bundle updated! ${PREV_HASH} -> ${NEW_HASH} (phase=${PHASE})"
    exit 0
  fi
  sleep 10
done
echo "==> Timeout waiting for configHash change"
exit 1
