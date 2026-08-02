#!/usr/bin/env bash
# Render-assertion test for the default first-party addon seed ref
# (change add-plugin-addon-packs, task 7.3).
#
# varroa-mcp-tools is not published to updates.jenkins.io, so an enabled update
# center must hold it without the operator having to name it. This asserts the
# chart default actually reaches the rendered UpdateCenter CR, and that an
# explicit override REPLACES it rather than appending to it.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com
        --set updateCenter.enabled=true)

DEFAULT_REF="ghcr.io/varroaci/varroa-jenkins/plugin-addon:varroa-mcp-tools-1.0.3"

fail() { echo "FAIL: $*" >&2; exit 1; }

uc_seed_refs() {
  yq eval-all 'select(.kind=="UpdateCenter") | .spec.seed.refs[]' - 2>/dev/null || true
}

# --- Default: the first-party addon pack is seeded ---
default_refs="$(helm template t "$CHART_DIR" "${COMMON[@]}" | uc_seed_refs)"
echo "$default_refs" | grep -qxF "$DEFAULT_REF" \
  || fail "default: UpdateCenter CR is missing the first-party seed ref $DEFAULT_REF (got: ${default_refs:-none})"
[ "$(echo "$default_refs" | grep -c .)" -eq 1 ] \
  || fail "default: expected exactly one seed ref, got: $default_refs"

# --- Override: an explicit list replaces the default, never appends ---
override_refs="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set 'updateCenter.seed.refs={registry.example.org/plugins/pack:v1}' | uc_seed_refs)"
echo "$override_refs" | grep -qxF "registry.example.org/plugins/pack:v1" \
  || fail "override: explicit seed ref missing (got: ${override_refs:-none})"
if echo "$override_refs" | grep -qxF "$DEFAULT_REF"; then
  fail "override: the chart default must be replaced, not appended to (got: $override_refs)"
fi

# --- Disabled: no UpdateCenter CR at all ---
if helm template t "$CHART_DIR" --set auth.oidc.clientSecret=x --set auth.oidc.clientId=x \
     --set global.domain=example.com --set bff.oidcClientSecret=x \
     --set auth.dashboardUrl=https://example.com \
   | yq eval-all 'select(.kind=="UpdateCenter")' - | grep -q .; then
  fail "disabled: no UpdateCenter CR should render when updateCenter.enabled is false"
fi

echo "PASS: update-center seed default renders and is overridable"
