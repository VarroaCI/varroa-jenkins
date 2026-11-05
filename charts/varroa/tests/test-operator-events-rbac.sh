#!/usr/bin/env bash
# Render-assertion test for the operator's read-only event list. The
# provisioning budget reads the Pulling events of the Jenkins pod, so the
# operator needs the events list verb in default mode (ClusterRole) and in
# scoped mode (per-namespace Role).
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

default_render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
scoped_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set 'managedNamespaces={tenant-a,tenant-b}')"

# --- Default mode ---
# The operator ClusterRole grants events list.
echo "$default_render" | yq eval-all '
  select(.kind=="ClusterRole" and (.metadata.name|test("operator")))
  | .rules[] | select(.resources[]=="events") | .verbs[]' - \
  | grep -qx list || fail "default: operator ClusterRole missing events list"

# --- Scoped mode ---
# No ClusterRole may grant events list when managedNamespaces is set.
if echo "$scoped_render" | yq eval-all '
  select(.kind=="ClusterRole") | .rules[] | select(.resources[]=="events") | .verbs[]' - \
  | grep -qx list; then
  fail "scoped: no ClusterRole may grant events list when managedNamespaces is set"
fi
# The operator Role in each managed namespace and in the release namespace grants it.
for ns in default tenant-a tenant-b; do
  echo "$scoped_render" | yq eval-all "
    select(.kind==\"Role\" and .metadata.namespace==\"$ns\" and (.metadata.name|test(\"operator\")))
    | .rules[] | select(.resources[]==\"events\") | .verbs[]" - \
    | grep -qx list || fail "scoped: operator Role missing events list in ns=$ns"
done

echo "PASS: operator events list RBAC renders correctly in default and scoped modes"
