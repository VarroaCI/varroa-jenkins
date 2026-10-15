#!/usr/bin/env bash
# =============================================================================
# Render-assertion test: the operator carries VARROA_BFF_SERVICE_ACCOUNT
# =============================================================================
# The BFF reads each composed bundle's content ConfigMap outside the release
# namespace, where its ServiceAccount holds no permission. The operator writes
# a name-scoped grant per bundle, and it learns the subject from this variable.
#
# The variable must render in core mode with the BFF full name. It must be
# absent when the deployment runs no BFF: hive mode, or bff.enabled=false.
# Without the absence checks a hive operator would log a warning on every
# reconcile and try to bind a ServiceAccount that does not exist.
# =============================================================================
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

VAR="VARROA_BFF_SERVICE_ACCOUNT"
failures=0

fail() { echo "  FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "  PASS: $*"; }

# operator_doc prints the rendered varroa-operator Deployment document.
operator_doc() {
  awk '
    BEGIN { RS="\n---\n" }
    index($0, "kind: Deployment\n") && index($0, "name: t-varroa-operator\n") { print; exit }
  ' <<< "$1"
}

# env_value prints the value of one env var in a Deployment document, or
# nothing when the variable is absent.
env_value() {
  local var="$1" doc="$2"
  awk -v var="- name: ${var}$" '
    $0 ~ var { f=1; next }
    f && /- name:/ { exit }
    f && /value:/ { sub(/^ *value: /, ""); gsub(/"/, ""); print; exit }
  ' <<< "$doc"
}

echo "Core mode renders the variable with the BFF full name"
core="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
core_op="$(operator_doc "$core")"
[ -n "$core_op" ] || fail "no varroa-operator Deployment rendered in core mode"
value="$(env_value "$VAR" "$core_op")"
if [ "$value" = "t-varroa-bff" ]; then
  pass "$VAR = $value"
else
  fail "$VAR = '${value:-<absent>}', want t-varroa-bff"
fi

echo "Hive mode renders no variable"
# values-hive.yaml carries the settings the chart's mode guard requires: the
# core's external bus URL, and NATS disabled.
hive="$(helm template t "$CHART_DIR" "${COMMON[@]}" -f "$CHART_DIR/values-hive.yaml" \
  --set bus.url=nats://core-nats.example.com:4222)"
hive_op="$(operator_doc "$hive")"
[ -n "$hive_op" ] || fail "no varroa-operator Deployment rendered in hive mode"
if [ -z "$(env_value "$VAR" "$hive_op")" ]; then
  pass "$VAR is absent in hive mode"
else
  fail "$VAR must be absent in hive mode, which deploys no BFF"
fi
# Guard the premise: hive mode really deploys no BFF.
if grep -q "name: t-varroa-bff$" <<< "$hive"; then
  fail "hive mode rendered a BFF, so this test's premise is wrong"
else
  pass "hive mode renders no BFF"
fi

echo "A disabled BFF renders no variable"
nobff="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set bff.enabled=false)"
nobff_op="$(operator_doc "$nobff")"
[ -n "$nobff_op" ] || fail "no varroa-operator Deployment rendered with bff.enabled=false"
if [ -z "$(env_value "$VAR" "$nobff_op")" ]; then
  pass "$VAR is absent when bff.enabled=false"
else
  fail "$VAR must be absent when the chart deploys no BFF"
fi

if [ "$failures" -ne 0 ]; then
  echo "FAIL: $failures assertion(s) failed" >&2
  exit 1
fi
echo "PASS: the operator carries $VAR only where a BFF exists"
