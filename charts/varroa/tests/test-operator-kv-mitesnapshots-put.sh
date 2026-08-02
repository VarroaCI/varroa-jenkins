#!/usr/bin/env bash
# Render-assertion test for the R29 NATS ACL grant: the operator must hold
# publish permission on $KV.mite_snapshots.> so it can persist classified
# plugin inventories via PutPluginClassification.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"

# Extract the auth.conf from the rendered Secret.
auth_conf="$(echo "$render" | yq eval-all '
  select(.kind=="Secret" and .metadata.name=="varroa-nats-auth-config")
  | .stringData."auth.conf"' -)"

if [ -z "$auth_conf" ]; then
  fail "nats-auth-config Secret not rendered"
fi

# Check that the operator's publish list includes $KV.mite_snapshots.>.
# The operator user block starts with "user: \"operator\"" and its publish
# list contains "$KV.mite_snapshots.>".
if ! echo "$auth_conf" | grep -q '"\$KV.mite_snapshots.>"'; then
  fail "operator publish list does not contain \$KV.mite_snapshots.>"
fi

echo "PASS: operator publish list contains \$KV.mite_snapshots.>"
