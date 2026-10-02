#!/usr/bin/env bash
# Render-assertion test for the auth.mode guard. The local and ldap modes need
# auth.bffUrl, because the BFF issues the sessions that controllers verify.
# The oidc mode renders without it.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
BFF_URL=(--set auth.bffUrl=http://varroa-varroa-bff.varroa-system.svc.cluster.local:8080)
fail() { echo "FAIL: $*" >&2; exit 1; }

helm template t "$CHART_DIR" "${COMMON[@]}" >/dev/null \
  || fail "oidc without auth.bffUrl must render"

for mode in local ldap; do
  if out="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set auth.mode="$mode" --set dex.enabled=false 2>&1)"; then
    fail "auth.mode=$mode without auth.bffUrl must fail the render"
  fi
  echo "$out" | grep -q "auth.mode \"$mode\" needs auth.bffUrl" \
    || fail "auth.mode=$mode error must name auth.bffUrl: $out"

  render="$(helm template t "$CHART_DIR" "${COMMON[@]}" "${BFF_URL[@]}" --set auth.mode="$mode" --set dex.enabled=false)" \
    || fail "auth.mode=$mode with auth.bffUrl must render"
  bff_env="$(echo "$render" | yq eval-all 'select(.kind=="Deployment" and (.metadata.name|test("varroa-operator$"))) | .spec.template.spec.containers[0].env[] | select(.name=="VARROA_BFF_URL") | .value' -)"
  [ "$bff_env" = "http://varroa-varroa-bff.varroa-system.svc.cluster.local:8080" ] \
    || fail "auth.mode=$mode: operator VARROA_BFF_URL must be auth.bffUrl, got '$bff_env'"
done

echo "PASS: auth.mode local and ldap need auth.bffUrl, and the operator receives it"
