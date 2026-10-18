#!/usr/bin/env bash
# =============================================================================
# Golden render test: operator Deployment carries the eight cell env vars (D3)
# =============================================================================
# Verifies VARROA_CELLS_WARM_POOL_ENABLED, VARROA_CELLS_WARM_POOL_PER_PROFILE,
# VARROA_CELLS_MAX_TENANTS, VARROA_CELLS_UNREADY_GRACE,
# VARROA_CELLS_CELL_TEMPLATE,
# VARROA_NETWORKPOLICY_ENABLED, VARROA_NETWORKPOLICY_APISERVER_EGRESS, and
# VARROA_CELL_ISOLATION on the varroa-operator Deployment: the two JSON-valued
# vars parse as valid JSON, pool auto-creation stays off on a default render,
# the cell template carries the default numbers and renders {} when null,
# and the two NetworkPolicy vars flip on a render with
# networkPolicy.enabled=true networkPolicy.cellIsolation.enabled=true.
#
# The five VARROA_CELLS_* values seed a CellPool the operator creates. They
# configure no live pool: an operator scales a live pool by editing the
# CellPool object.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test-secret --set auth.dashboardUrl=https://varroa.test"

failures=0

check() {
  local desc="$1"
  local result="$2"
  if [[ "$result" == 0 ]]; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc"
    failures=$((failures + 1))
  fi
}

# extract_doc prints the one rendered YAML document whose kind and
# metadata.name match, out of a multi-document helm template render.
# It uses exact line matches (a trailing newline in the search text), so
# "kind: Deployment" never matches a document of another kind.
extract_doc() {
  local kind="$1" name="$2" input="$3"
  awk -v kindline="kind: ${kind}" -v nameline="name: ${name}" '
    BEGIN { RS="\n---\n" }
    index($0, kindline "\n") && index($0, nameline "\n") { print; exit }
  ' <<< "$input"
}

# env_block prints one env var entry (its name line through the next env
# var's name line, or the end of the document) out of a document.
env_block() {
  local var="$1" doc="$2"
  awk -v var="- name: ${var}$" '
    $0 ~ var { f=1; print; next }
    f && /- name:/ { exit }
    f { print }
  ' <<< "$doc"
}

# raw_value prints the "value: ..." line of an env_block, exactly as
# rendered (still YAML-quoted).
raw_value() {
  local block="$1"
  grep -m1 'value:' <<< "$block" | sed -e 's/^ *value: //'
}

# unquoted_value strips the surrounding YAML double quotes from a scalar
# env value, leaving a plain string like "false" or "5m".
unquoted_value() {
  local v="$1"
  v="${v#\"}"
  v="${v%\"}"
  echo "$v"
}

# json_value strips the surrounding YAML double quotes AND unescapes the
# backslash-escaped inner quotes, turning a rendered
# value: "{\"cidrs\":[\"0.0.0.0/0\"],\"ports\":[443,6443]}" line into the raw JSON text
# {"cidrs":["0.0.0.0/0"],"ports":[443,6443]}.
json_value() {
  local v="$1"
  v="${v#\"}"
  v="${v%\"}"
  v="${v//\\\"/\"}"
  echo "$v"
}

assert_valid_json() {
  local desc="$1"
  local json="$2"
  if python3 -c 'import json,sys; json.load(sys.stdin)' <<< "$json" > /dev/null 2>&1; then rc=0; else rc=1; fi
  check "$desc" "$rc"
}

echo "=== Cell operator env chart golden render assertions (D3, task 3.2) ==="

# ---------------------------------------------------------------------------
# (a) Default render: all eight vars present, auto-creation off, JSON valid.
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Default render ---"

RENDER_DEFAULT=$(helm template "$ROOT" $COMMON 2>/dev/null) || {
  echo "FAIL: helm template (default) failed"; exit 1
}

OPERATOR_DEFAULT=$(extract_doc "Deployment" "release-name-varroa-operator" "$RENDER_DEFAULT")
[[ -n "$OPERATOR_DEFAULT" ]] && rc=0 || rc=1
check "operator Deployment renders" "$rc"

WARM_ENABLED=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_WARM_POOL_ENABLED" "$OPERATOR_DEFAULT")")")
[[ "$WARM_ENABLED" == "false" ]] && rc=0 || rc=1
check "VARROA_CELLS_WARM_POOL_ENABLED defaults to false (the operator auto-creates no pool)" "$rc"

WARM_PER_PROFILE=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_WARM_POOL_PER_PROFILE" "$OPERATOR_DEFAULT")")")
[[ "$WARM_PER_PROFILE" == "1" ]] && rc=0 || rc=1
check "VARROA_CELLS_WARM_POOL_PER_PROFILE seeds a new pool with 1" "$rc"

MAX_TENANTS=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_MAX_TENANTS" "$OPERATOR_DEFAULT")")")
[[ "$MAX_TENANTS" == "10" ]] && rc=0 || rc=1
check "VARROA_CELLS_MAX_TENANTS seeds a new pool with 10" "$rc"

# The BFF resolves the effective tenant cap of a cell usage read from the
# same value. Both processes agree only while one chart value renders both
# variables.
BFF_DEFAULT=$(extract_doc "Deployment" "release-name-varroa-bff" "$RENDER_DEFAULT")
[[ -n "$BFF_DEFAULT" ]] && rc=0 || rc=1
check "bff Deployment renders" "$rc"

BFF_MAX_TENANTS=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_MAX_TENANTS" "$BFF_DEFAULT")")")
[[ "$BFF_MAX_TENANTS" == "10" ]] && rc=0 || rc=1
check "the bff container carries VARROA_CELLS_MAX_TENANTS at 10" "$rc"

UNREADY_GRACE=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_UNREADY_GRACE" "$OPERATOR_DEFAULT")")")
[[ "$UNREADY_GRACE" == "5m" ]] && rc=0 || rc=1
check "VARROA_CELLS_UNREADY_GRACE seeds a new pool with 5m" "$rc"

CELL_TEMPLATE_JSON_DEFAULT=$(json_value "$(raw_value "$(env_block "VARROA_CELLS_CELL_TEMPLATE" "$OPERATOR_DEFAULT")")")
assert_valid_json "VARROA_CELLS_CELL_TEMPLATE is valid JSON (default render)" "$CELL_TEMPLATE_JSON_DEFAULT"
if python3 -c '
import json, sys
t = json.load(sys.stdin)
assert t["resources"] == {"requests": {"cpu": "1", "memory": "3Gi"}, "limits": {"memory": "6Gi"}}
assert t["miteResources"] == {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"memory": "128Mi"}}
po = t["podOverrides"]
assert po["jvmOpts"] == "-XX:MaxRAMPercentage=50"
assert "podLabels" not in po
assert po["affinity"] == {"podAntiAffinity": {"preferredDuringSchedulingIgnoredDuringExecution": [{
    "weight": 100,
    "podAffinityTerm": {
        "topologyKey": "kubernetes.io/hostname",
        "labelSelector": {"matchExpressions": [{"key": "varroa.dev/cell-pool", "operator": "Exists"}]},
    },
}]}}
' <<< "$CELL_TEMPLATE_JSON_DEFAULT" > /dev/null 2>&1; then rc=0; else rc=1; fi
check "VARROA_CELLS_CELL_TEMPLATE carries the default requests, limits, jvmOpts, and anti-affinity" "$rc"

NETPOL_ENABLED_DEFAULT=$(unquoted_value "$(raw_value "$(env_block "VARROA_NETWORKPOLICY_ENABLED" "$OPERATOR_DEFAULT")")")
[[ "$NETPOL_ENABLED_DEFAULT" == "false" ]] && rc=0 || rc=1
check "VARROA_NETWORKPOLICY_ENABLED default is false" "$rc"

APISERVER_EGRESS_JSON_DEFAULT=$(json_value "$(raw_value "$(env_block "VARROA_NETWORKPOLICY_APISERVER_EGRESS" "$OPERATOR_DEFAULT")")")
assert_valid_json "VARROA_NETWORKPOLICY_APISERVER_EGRESS is valid JSON (default render)" "$APISERVER_EGRESS_JSON_DEFAULT"
grep -q '"cidrs":\["0.0.0.0/0"\]' <<< "$APISERVER_EGRESS_JSON_DEFAULT" && rc=0 || rc=1
check 'VARROA_NETWORKPOLICY_APISERVER_EGRESS default carries cidrs ["0.0.0.0/0"]' "$rc"
grep -q '"ports":\[443,6443\]' <<< "$APISERVER_EGRESS_JSON_DEFAULT" && rc=0 || rc=1
check "VARROA_NETWORKPOLICY_APISERVER_EGRESS default carries ports [443,6443]" "$rc"

CELL_ISOLATION_JSON_DEFAULT=$(json_value "$(raw_value "$(env_block "VARROA_CELL_ISOLATION" "$OPERATOR_DEFAULT")")")
assert_valid_json "VARROA_CELL_ISOLATION is valid JSON (default render)" "$CELL_ISOLATION_JSON_DEFAULT"
grep -q '"enabled":false' <<< "$CELL_ISOLATION_JSON_DEFAULT" && rc=0 || rc=1
check "VARROA_CELL_ISOLATION default carries enabled:false" "$rc"

# ---------------------------------------------------------------------------
# (b) networkPolicy.enabled=true + networkPolicy.cellIsolation.enabled=true.
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) networkPolicy.enabled=true, networkPolicy.cellIsolation.enabled=true ---"

RENDER_NETPOL=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set networkPolicy.cellIsolation.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (networkPolicy.enabled) failed"; exit 1
}

OPERATOR_NETPOL=$(extract_doc "Deployment" "release-name-varroa-operator" "$RENDER_NETPOL")
[[ -n "$OPERATOR_NETPOL" ]] && rc=0 || rc=1
check "operator Deployment renders with networkPolicy.enabled=true" "$rc"

NETPOL_ENABLED=$(unquoted_value "$(raw_value "$(env_block "VARROA_NETWORKPOLICY_ENABLED" "$OPERATOR_NETPOL")")")
[[ "$NETPOL_ENABLED" == "true" ]] && rc=0 || rc=1
check "VARROA_NETWORKPOLICY_ENABLED is true" "$rc"

APISERVER_EGRESS_JSON=$(json_value "$(raw_value "$(env_block "VARROA_NETWORKPOLICY_APISERVER_EGRESS" "$OPERATOR_NETPOL")")")
assert_valid_json "VARROA_NETWORKPOLICY_APISERVER_EGRESS is valid JSON (networkPolicy.enabled=true render)" "$APISERVER_EGRESS_JSON"

CELL_ISOLATION_JSON=$(json_value "$(raw_value "$(env_block "VARROA_CELL_ISOLATION" "$OPERATOR_NETPOL")")")
assert_valid_json "VARROA_CELL_ISOLATION is valid JSON (cellIsolation.enabled=true render)" "$CELL_ISOLATION_JSON"
grep -q '"enabled":true' <<< "$CELL_ISOLATION_JSON" && rc=0 || rc=1
check "VARROA_CELL_ISOLATION reflects cellIsolation.enabled=true" "$rc"

# Pool auto-creation stays off: this render never touched cells.warmPool.
WARM_ENABLED_NETPOL=$(unquoted_value "$(raw_value "$(env_block "VARROA_CELLS_WARM_POOL_ENABLED" "$OPERATOR_NETPOL")")")
[[ "$WARM_ENABLED_NETPOL" == "false" ]] && rc=0 || rc=1
check "VARROA_CELLS_WARM_POOL_ENABLED stays false when only networkPolicy flags change" "$rc"

# ---------------------------------------------------------------------------
# (c) cells.cellTemplate=null: an explicit opt-out renders {}. A {} override
# does not opt out, because Helm merges it into the default map.
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) cells.cellTemplate=null ---"

RENDER_NULL=$(helm template "$ROOT" $COMMON --set cells.cellTemplate=null 2>/dev/null) || {
  echo "FAIL: helm template (cells.cellTemplate=null) failed"; exit 1
}
OPERATOR_NULL=$(extract_doc "Deployment" "release-name-varroa-operator" "$RENDER_NULL")
CELL_TEMPLATE_JSON_NULL=$(json_value "$(raw_value "$(env_block "VARROA_CELLS_CELL_TEMPLATE" "$OPERATOR_NULL")")")
[[ "$CELL_TEMPLATE_JSON_NULL" == "{}" ]] && rc=0 || rc=1
check "VARROA_CELLS_CELL_TEMPLATE renders {} for cells.cellTemplate=null (got: $CELL_TEMPLATE_JSON_NULL)" "$rc"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
