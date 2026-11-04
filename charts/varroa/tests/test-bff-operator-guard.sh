#!/usr/bin/env bash
# Render-assertion test for the BFF operator guard. A full-mode BFF reads the
# varroa-mite-signing-key Secret, and only the operator creates it, so the
# render fails without the operator. A hive render renders no BFF, so the
# check does not apply there.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
HIVE=(-f "$CHART_DIR/values-hive.yaml" --set cluster.name=x --set bus.url=nats://h:4222 --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com)
fail() { echo "FAIL: $*" >&2; exit 1; }
deployment() { yq eval-all "select(.kind==\"Deployment\" and (.metadata.name|test(\"$1\$\"))) | .metadata.name" -; }

if out="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set operator.enabled=false --set gateway.enabled=false 2>&1)"; then
  fail "bff.enabled with operator.enabled=false must fail the render"
fi
echo "$out" | grep -q "bff.enabled needs operator.enabled" \
  || fail "error must name bff.enabled and operator.enabled: $out"
echo "$out" | grep -q "varroa-mite-signing-key" \
  || fail "error must name the varroa-mite-signing-key Secret: $out"

render="$(helm template t "$CHART_DIR" "${COMMON[@]}")" \
  || fail "default full render must succeed"
[ -n "$(echo "$render" | deployment varroa-operator)" ] || fail "default full render needs a varroa-operator Deployment"
[ -n "$(echo "$render" | deployment varroa-bff)" ] || fail "default full render needs a varroa-bff Deployment"

render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set operator.enabled=false --set gateway.enabled=false --set bff.enabled=false)" \
  || fail "full render with bff.enabled=false and no operator must succeed"
[ -z "$(echo "$render" | deployment varroa-bff)" ] || fail "bff.enabled=false must render no varroa-bff Deployment"

render="$(helm template t "$CHART_DIR" "${HIVE[@]}" --set bff.enabled=true --set operator.enabled=false --set gateway.enabled=false)" \
  || fail "hive render with bff.enabled=true and no operator must succeed"
[ -z "$(echo "$render" | deployment varroa-bff)" ] || fail "hive render must render no varroa-bff Deployment"

echo "PASS: a full-mode BFF needs the operator, and a hive render skips the check"
