#!/usr/bin/env bash
# Render-assertion test for the gateway operator guard. The gateway reads the
# varroa-ca Secret, and only the operator creates it, so the render fails
# without the operator in both full and hive mode.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
HIVE=(-f "$CHART_DIR/values-hive.yaml" --set cluster.name=x --set bus.url=nats://h:4222 --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com)
fail() { echo "FAIL: $*" >&2; exit 1; }

for set in COMMON HIVE; do
  declare -n args="$set"
  if out="$(helm template t "$CHART_DIR" "${args[@]}" --set operator.enabled=false --set bff.enabled=false 2>&1)"; then
    fail "$set: gateway.enabled without operator.enabled must fail the render"
  fi
  echo "$out" | grep -q "gateway.enabled needs operator.enabled" \
    || fail "$set: error must name gateway.enabled and operator.enabled: $out"
  helm template t "$CHART_DIR" "${args[@]}" >/dev/null \
    || fail "$set: default render must succeed"
done

echo "PASS: a gateway render needs the operator in full and hive mode"
