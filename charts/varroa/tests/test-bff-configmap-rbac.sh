#!/usr/bin/env bash
# Render-assertion test for the BFF's ConfigMap read grant (issue #416).
#
# The BFF reads a version profile's materialized plugin set by
# (status.contentRef, OPERATOR_NAMESPACE), and OPERATOR_NAMESPACE is fixed to
# .Release.Namespace. Without a `get` on configmaps that read always fails and
# profile plugin resolution silently returns nothing. The grant must be
# namespaced: a ClusterRole rule would let the internet-facing BFF read every
# ConfigMap in the cluster to satisfy a read confined to one namespace.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"

# The release-namespace BFF Role must grant get/create/update on core-group
# configmaps: promotion's synchronous re-materialization step always attempts
# a create first on the derived pluginset ConfigMap (falling back to
# get+update only on AlreadyExists), so create is required even when the
# ConfigMap already exists.
configmap_verbs="$(echo "$render" | yq eval-all '
  select(.kind=="Role" and .metadata.namespace=="default" and (.metadata.name|test("bff")))
  | .rules[] | select(.apiGroups[]=="" and .resources[]=="configmaps") | .verbs[]' -)"
for v in get create update; do
  echo "$configmap_verbs" | grep -qx "$v" || fail "BFF release-namespace Role is missing $v on configmaps"
done

# A RoleBinding must bind that Role to the BFF ServiceAccount in the same namespace.
role_name="$(echo "$render" | yq eval-all '
  select(.kind=="Role" and .metadata.namespace=="default" and (.metadata.name|test("bff")))
  | select(.rules[].resources[]=="configmaps") | .metadata.name' - | head -1)"
[ -n "$role_name" ] || fail "could not identify the BFF Role carrying the configmaps rule"

echo "$render" | yq eval-all "
  select(.kind==\"RoleBinding\" and .metadata.namespace==\"default\" and .roleRef.name==\"$role_name\")
  | .subjects[] | select(.kind==\"ServiceAccount\") | .namespace" - \
  | grep -qx default || fail "no RoleBinding binds $role_name to the BFF ServiceAccount in the release namespace"

# No ClusterRole may grant cluster-wide configmap read to satisfy this.
if echo "$render" | yq eval-all '
  select(.kind=="ClusterRole" and (.metadata.name|test("bff")))
  | .rules[] | select(.resources[]=="configmaps")' - | grep -q .; then
  fail "the BFF ClusterRole must NOT grant cluster-wide configmap access"
fi

echo "PASS: BFF holds a namespace-scoped configmaps get grant"
