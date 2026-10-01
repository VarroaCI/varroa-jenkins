#!/usr/bin/env bash
# Render-assertion test for auth.localAccounts (local accounts beside OIDC).
# Verifies the guard refuses a mode other than oidc and a missing bffUrl, and
# that the operator and the BFF both get VARROA_LOCAL_ACCOUNTS.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
fail() { echo "FAIL: $*" >&2; exit 1; }

env_value() { # $1=render $2=deployment-name-suffix $3=env name
  echo "$1" | yq eval-all "select(.kind==\"Deployment\" and (.metadata.name|test(\"$2\$\"))) | .spec.template.spec.containers[0].env[] | select(.name==\"$3\") | .value" -
}

default_render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
[ "$(env_value "$default_render" varroa-operator VARROA_LOCAL_ACCOUNTS)" = "false" ] \
  || fail "default: operator VARROA_LOCAL_ACCOUNTS must be false"
[ "$(env_value "$default_render" varroa-bff VARROA_LOCAL_ACCOUNTS)" = "false" ] \
  || fail "default: bff VARROA_LOCAL_ACCOUNTS must be false"

enabled_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set auth.localAccounts.enabled=true --set auth.bffUrl=https://app.example.com)"
[ "$(env_value "$enabled_render" varroa-operator VARROA_LOCAL_ACCOUNTS)" = "true" ] \
  || fail "enabled: operator VARROA_LOCAL_ACCOUNTS must be true"
[ "$(env_value "$enabled_render" varroa-bff VARROA_LOCAL_ACCOUNTS)" = "true" ] \
  || fail "enabled: bff VARROA_LOCAL_ACCOUNTS must be true"

if out="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set auth.localAccounts.enabled=true 2>&1)"; then
  fail "enabled without auth.bffUrl must fail the render"
fi
echo "$out" | grep -q "auth.bffUrl" || fail "missing bffUrl error must name auth.bffUrl: $out"

if out="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set auth.mode=local \
  --set auth.localAccounts.enabled=true --set auth.bffUrl=https://app.example.com 2>&1)"; then
  fail "enabled with auth.mode=local must fail the render"
fi
echo "$out" | grep -q 'needs auth.mode "oidc"' || fail "mode error must name auth.mode: $out"

echo "PASS: auth.localAccounts is off by default, needs oidc and bffUrl, and reaches the operator and the BFF"
