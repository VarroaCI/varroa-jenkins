#!/usr/bin/env bash
# =============================================================================
# Golden render test: cell-pool RBAC and the four pool-seed env vars
# =============================================================================
# Verifies that the operator ClusterRole carries cellpools and cellpools/status
# with the full verb set, that the BFF ClusterRole carries cellpools with the
# read and write verbs but neither create nor delete, and that the four
# VARROA_CELLS_* environment variables reach the operator Deployment. Those
# four values seed a CellPool the operator creates; they configure no live
# pool.
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
extract_doc() {
  local kind="$1" name="$2" input="$3"
  awk -v kindline="kind: ${kind}" -v nameline="name: ${name}" '
    BEGIN { RS="\n---\n" }
    index($0, kindline "\n") && index($0, nameline "\n") { print; exit }
  ' <<< "$input"
}

# rule_verbs prints the verbs line of the first rule whose resources line
# matches exactly, so a rule naming cellpools is never confused with one
# naming cellpools/status.
rule_verbs() {
  local resources="$1" doc="$2"
  awk -v want="resources: ${resources}" '
    index($0, want) { f=1; next }
    f && /verbs:/ { print; exit }
  ' <<< "$doc"
}

# env_block prints one env var entry out of a document.
env_block() {
  local var="$1" doc="$2"
  awk -v var="- name: ${var}$" '
    $0 ~ var { f=1; print; next }
    f && /- name:/ { exit }
    f { print }
  ' <<< "$doc"
}

# unquoted_value prints an env entry's scalar value without its YAML quotes.
unquoted_value() {
  local block="$1" v
  v="$(grep -m1 'value:' <<< "$block" | sed -e 's/^ *value: //')"
  v="${v#\"}"
  v="${v%\"}"
  echo "$v"
}

echo "=== Cell-pool RBAC and pool-seed env chart golden render assertions ==="

RENDER=$(helm template "$ROOT" $COMMON 2>/dev/null) || {
  echo "FAIL: helm template failed"; exit 1
}

# ---------------------------------------------------------------------------
# (a) Operator ClusterRole
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Operator ClusterRole ---"

OPERATOR_ROLE=$(extract_doc "ClusterRole" "release-name-varroa-operator" "$RENDER")
[[ -n "$OPERATOR_ROLE" ]] && rc=0 || rc=1
check "operator ClusterRole renders" "$rc"

OPERATOR_VERBS=$(rule_verbs '["cellpools", "cellpools/status"]' "$OPERATOR_ROLE")
[[ -n "$OPERATOR_VERBS" ]] && rc=0 || rc=1
check "operator ClusterRole carries a cellpools and cellpools/status rule" "$rc"

for verb in get list watch create update patch delete; do
  grep -q "\"$verb\"" <<< "$OPERATOR_VERBS" && rc=0 || rc=1
  check "operator cellpools rule grants $verb" "$rc"
done

# ---------------------------------------------------------------------------
# (b) BFF ClusterRole
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) BFF ClusterRole ---"

BFF_ROLE=$(extract_doc "ClusterRole" "release-name-varroa-bff" "$RENDER")
[[ -n "$BFF_ROLE" ]] && rc=0 || rc=1
check "BFF ClusterRole renders" "$rc"

BFF_VERBS=$(rule_verbs '["cellpools"]' "$BFF_ROLE")
[[ -n "$BFF_VERBS" ]] && rc=0 || rc=1
check "BFF ClusterRole carries a cellpools rule" "$rc"

for verb in get list watch update patch; do
  grep -q "\"$verb\"" <<< "$BFF_VERBS" && rc=0 || rc=1
  check "BFF cellpools rule grants $verb" "$rc"
done

# The BFF never creates or deletes a pool: an operator does that with kubectl
# or through GitOps.
for verb in create delete; do
  grep -q "\"$verb\"" <<< "$BFF_VERBS" && rc=1 || rc=0
  check "BFF cellpools rule does not grant $verb" "$rc"
done

# ---------------------------------------------------------------------------
# (c) The four pool-seed env vars on the operator Deployment
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) Pool-seed environment ---"

OPERATOR_DEPLOY=$(extract_doc "Deployment" "release-name-varroa-operator" "$RENDER")
[[ -n "$OPERATOR_DEPLOY" ]] && rc=0 || rc=1
check "operator Deployment renders" "$rc"

assert_env() {
  local var="$1" want="$2"
  local got
  got="$(unquoted_value "$(env_block "$var" "$OPERATOR_DEPLOY")")"
  [[ "$got" == "$want" ]] && rc=0 || rc=1
  check "$var seeds a new pool with $want (got '$got')" "$rc"
}

assert_env VARROA_CELLS_WARM_POOL_ENABLED false
assert_env VARROA_CELLS_WARM_POOL_PER_PROFILE 1
assert_env VARROA_CELLS_MAX_TENANTS 10
assert_env VARROA_CELLS_UNREADY_GRACE 5m

# ---------------------------------------------------------------------------
# (d) The CRD ships with the chart
# ---------------------------------------------------------------------------
echo ""
echo "--- (d) Packaged CRD ---"

[[ -f "$ROOT/crds/varroa.dev_cellpools.yaml" ]] && rc=0 || rc=1
check "charts/varroa/crds/varroa.dev_cellpools.yaml ships with the chart" "$rc"

grep -q "name: cellpools.varroa.dev" "$ROOT/crds/varroa.dev_cellpools.yaml" && rc=0 || rc=1
check "the packaged CRD is named cellpools.varroa.dev" "$rc"

grep -q "scope: Cluster" "$ROOT/crds/varroa.dev_cellpools.yaml" && rc=0 || rc=1
check "the packaged CRD is cluster-scoped" "$rc"

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
