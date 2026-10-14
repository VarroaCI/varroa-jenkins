#!/usr/bin/env bash
# Render-assertion test for updateCenter.pullThrough.prefetch.
#
# prefetch is opt-in and defaults to off, so the default render carries no
# prefetch key. With pullThrough enabled, prefetch=true renders prefetch: true.
# prefetch without pullThrough is a render failure, because the CEL rule on the
# UpdateCenter CRD would reject the object anyway.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com
        --set updateCenter.enabled=true)

fail() { echo "FAIL: $*" >&2; exit 1; }

uc_prefetch() {
  yq eval-all 'select(.kind=="UpdateCenter") | .spec.pullThrough.prefetch' - 2>/dev/null || true
}

# --- Default: no prefetch key is rendered ---
default_out="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
[ "$(echo "$default_out" | uc_prefetch)" = "null" ] \
  || fail "default: expected no prefetch key, got: $(echo "$default_out" | uc_prefetch)"

# --- Enabled with prefetch: the CR carries prefetch: true ---
on_out="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.pullThrough.enabled=true \
  --set updateCenter.pullThrough.prefetch=true)"
[ "$(echo "$on_out" | uc_prefetch)" = "true" ] \
  || fail "enabled+prefetch: expected prefetch: true, got: $(echo "$on_out" | uc_prefetch)"

# --- prefetch without pullThrough: the render fails with the explanation ---
if err="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.pullThrough.prefetch=true 2>&1)"; then
  fail "prefetch without pullThrough rendered; expected a render failure"
fi
echo "$err" | grep -q "pullThrough.prefetch requires updateCenter.pullThrough.enabled" \
  || fail "prefetch without pullThrough: wrong failure message: $err"

echo "PASS: test-updatecenter-prefetch.sh"
