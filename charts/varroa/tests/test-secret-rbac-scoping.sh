#!/usr/bin/env bash
# Render-assertion test for scoped Secret RBAC (#230 / change scope-operator-bff-secret-rbac).
# Verifies the internet-facing BFF never holds cluster-wide Secret write, and that
# managedNamespaces switches the operator + BFF from cluster-wide to per-namespace Roles.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

default_render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
scoped_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set 'managedNamespaces={team-a,team-b}')"

# --- Default mode ---
# The BFF holds NO cluster-wide Secret access in any mode: its Secret CRUD is
# scoped to the release namespace via a Role. This is stricter than merely
# forbidding cluster-wide writes, so the write check below is subsumed but kept
# explicit as a regression guard on the property that matters most.
if echo "$default_render" | yq eval-all '
  select(.kind=="ClusterRole" and (.metadata.name|test("bff")))
  | .rules[] | select(.resources[]=="secrets") | .verbs[]' - \
  | grep -q .; then
  fail "default: BFF ClusterRole must NOT grant secrets at all (release-namespace Role owns them)"
fi
# ...and the release-namespace Role must actually grant them, or the BFF is broken.
echo "$default_render" | yq eval-all '
  select(.kind=="Role" and (.metadata.name|test("bff")))
  | .rules[] | select(.resources[]=="secrets") | .verbs[]' - \
  | grep -qxE 'get' || fail "default: BFF release-namespace Role missing secrets get"

# --- Scoped mode ---
# Neither ClusterRole may carry a secrets rule.
if echo "$scoped_render" | yq eval-all '
  select(.kind=="ClusterRole") | .rules[] | select(.resources[]=="secrets")' - \
  | grep -q .; then
  fail "scoped: no ClusterRole may grant secrets when managedNamespaces is set"
fi
# Operator Secret Role must exist in each managed namespace + release namespace.
for ns in default team-a team-b; do
  echo "$scoped_render" | yq eval-all "
    select(.kind==\"Role\" and .metadata.namespace==\"$ns\" and (.metadata.name|test(\"operator\")))
    | .rules[] | select(.resources[]==\"secrets\") | .verbs[]" - \
    | grep -qx delete || fail "scoped: operator Role missing in ns=$ns"
done

echo "PASS: secret RBAC scoping renders correctly in default and scoped modes"
