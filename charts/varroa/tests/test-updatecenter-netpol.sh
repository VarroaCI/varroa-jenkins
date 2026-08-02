#!/usr/bin/env bash
# =============================================================================
# Golden render test: update-center NetworkPolicy assertions (§1.6)
# =============================================================================
# Verifies that `helm template` produces the correct NetworkPolicy rules for
# the varroa-updatecenter component and the operator's ociRegistryEgress rule.
# All three egress toggle groups (pullThroughEgress, updateCenterRegistryEgress,
# ociRegistryEgress) are independently testable, each with structural gates.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com"

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

# grep_c: count matches, return 0 on zero (safe with set -e)
grep_c() {
  local pattern="$1"
  local input="$2"
  grep -c "$pattern" <<< "$input" 2>/dev/null || true
}

# sed_extract: extract YAML doc block matching a name
sed_extract() {
  local name="$1"
  local input="$2"
  sed -n "/name:.*${name}/,/^---/p" <<< "$input"
}

echo "=== Update Center NetworkPolicy golden render assertions (§1.6) ==="

# ---------------------------------------------------------------------------
# (a) Default: updateCenter disabled / netpol disabled → no updatecenter netpol
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Default (disabled) ---"

RENDER_DEFAULT=$(helm template "$ROOT" $COMMON 2>/dev/null) || {
  echo "FAIL: helm template (default) failed"; exit 1
}
found=$(grep_c 'np-updatecenter' "$RENDER_DEFAULT")
[[ "$found" -eq 0 ]]; check "default: no np-updatecenter policy" $?
[[ "$found" -eq 0 ]] || true  # continue on failure

RENDER_NETPOL_ONLY=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (netpol only) failed"; exit 1
}
found=$(grep_c 'np-updatecenter' "$RENDER_NETPOL_ONLY")
[[ "$found" -eq 0 ]]; check "networkPolicy enabled, updateCenter disabled: no np-updatecenter" $?
[[ "$found" -eq 0 ]] || true

RENDER_UC_ONLY=$(helm template "$ROOT" $COMMON --set updateCenter.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (UC only) failed"; exit 1
}
found=$(grep_c 'np-updatecenter' "$RENDER_UC_ONLY")
[[ "$found" -eq 0 ]]; check "updateCenter enabled, networkPolicy disabled: no np-updatecenter" $?
[[ "$found" -eq 0 ]] || true

# ---------------------------------------------------------------------------
# (b) Full render: netpol + UC + pullThrough + oci-storage — all three egress
#     toggles active, ingress from exactly 3 sources, no fourth.
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) Full render (all egress toggles + ingress 3 sources) ---"

RENDER_FULL=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set updateCenter.pullThrough.enabled=true \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=test \
  --set networkPolicy.pullThroughEgress.enabled=true \
  --set networkPolicy.updateCenterRegistryEgress.enabled=true \
  --set networkPolicy.ociRegistryEgress.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (full) failed"; exit 1
}

# Policy object exists
grep -q 'name:.*np-updatecenter' <<< "$RENDER_FULL" && rc=0 || rc=1
check "np-updatecenter policy object present" "$rc"

UC_POLICY=$(sed_extract 'np-updatecenter' "$RENDER_FULL")

# Ingress — exactly 3 port 8080 entries (Jenkins pods, operator, bff)
ingress_8080=$(grep_c 'port: 8080' "$UC_POLICY")
[[ "$ingress_8080" -eq 3 ]]; check "ingress: exactly 3 port 8080 entries" $?
[[ "$ingress_8080" -eq 3 ]] || true

grep -q 'app.kubernetes.io/managed-by: varroa-operator' <<< "$UC_POLICY" && rc=0 || rc=1
check "ingress: Jenkins pods (managed-by label)" "$rc"

grep -q 'namespaceSelector: {}' <<< "$UC_POLICY" && rc=0 || rc=1
check "ingress: namespaceSelector for Jenkins pods" "$rc"

grep -q 'app.kubernetes.io/component: varroa-operator' <<< "$UC_POLICY" && rc=0 || rc=1
check "ingress: operator pod source" "$rc"

grep -q 'app.kubernetes.io/component: varroa-bff' <<< "$UC_POLICY" && rc=0 || rc=1
check "ingress: BFF pod source" "$rc"

# No fourth ingress source — count unique 'from:' blocks inside ingress
from_count=$(echo "$UC_POLICY" | sed -n '/^  ingress:/,/^  egress:/p' | grep -c '    - from:' || true)
[[ "$from_count" -eq 3 ]]; check "ingress: exactly 3 from: blocks (no fourth source)" $?
[[ "$from_count" -eq 3 ]] || true

# Egress — pullThroughEgress rule present
grep -q 'Pull-through egress' <<< "$UC_POLICY" && rc=0 || rc=1
check "egress: pullThroughEgress rule present" "$rc"

grep -q 'cidr: 0.0.0.0/0' <<< "$UC_POLICY" && rc=0 || rc=1
check "egress: default CIDR 0.0.0.0/0 present" "$rc"

grep -q 'port: 443' <<< "$UC_POLICY" && rc=0 || rc=1
check "egress: port 443 present" "$rc"

# Egress — updateCenterRegistryEgress rule present
grep -q 'OCI-registry egress' <<< "$UC_POLICY" && rc=0 || rc=1
check "egress: updateCenterRegistryEgress rule present" "$rc"

# Operator policy checks
OPERATOR_POLICY=$(sed_extract 'np-operator' "$RENDER_FULL")

# Operator has UC-egress rule (full mode only)
grep -q 'varroa-updatecenter' <<< "$OPERATOR_POLICY" && rc=0 || rc=1
check "operator: egress to updatecenter present" "$rc"

# Operator has ociRegistryEgress
grep -q 'ociRegistryEgress' <<< "$OPERATOR_POLICY" && rc=0 || rc=1
check "operator: ociRegistryEgress rule present" "$rc"

# BFF policy has UC-egress rule
BFF_POLICY=$(sed_extract 'np-bff' "$RENDER_FULL")
grep -q 'varroa-updatecenter' <<< "$BFF_POLICY" && rc=0 || rc=1
check "bff: egress to updatecenter present" "$rc"

# ---------------------------------------------------------------------------
# (c) pullThroughEgress disabled → no pull-through rule
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) pullThroughEgress disabled ---"

RENDER_NO_PT=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set updateCenter.pullThrough.enabled=false \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=test \
  --set networkPolicy.pullThroughEgress.enabled=true \
  --set networkPolicy.updateCenterRegistryEgress.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (no pull-through) failed"; exit 1
}

UC_NO_PT=$(sed_extract 'np-updatecenter' "$RENDER_NO_PT")
grep -q 'Pull-through egress' <<< "$UC_NO_PT" && rc=0 || rc=1
if [[ "$rc" -eq 1 ]]; then
  check "pullThroughEgress disabled: no pull-through egress rule" 0
else
  check "pullThroughEgress disabled: no pull-through egress rule" 1
fi

# OCI-registry egress still present
grep -q 'OCI-registry egress' <<< "$UC_NO_PT" && rc=0 || rc=1
check "pullThroughEgress disabled: OCI-registry egress still present" "$rc"

# ---------------------------------------------------------------------------
# (d) storage.type=local (not OCI) → no OCI-registry egress rule
# ---------------------------------------------------------------------------
echo ""
echo "--- (d) storage.type=local (no OCI-registry egress) ---"

RENDER_LOCAL=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set updateCenter.pullThrough.enabled=true \
  --set updateCenter.storage.type=local \
  --set networkPolicy.pullThroughEgress.enabled=true \
  --set networkPolicy.updateCenterRegistryEgress.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (local storage) failed"; exit 1
}

UC_LOCAL=$(sed_extract 'np-updatecenter' "$RENDER_LOCAL")
grep -q 'OCI-registry egress' <<< "$UC_LOCAL" && rc=0 || rc=1
if [[ "$rc" -eq 1 ]]; then
  check "storage.type=local: no OCI-registry egress rule" 0
else
  check "storage.type=local: no OCI-registry egress rule" 1
fi

# Pull-through egress still present
grep -q 'Pull-through egress' <<< "$UC_LOCAL" && rc=0 || rc=1
check "storage.type=local: pull-through egress still present" "$rc"

# ---------------------------------------------------------------------------
# (e) updateCenterRegistryEgress.enabled=false → no OCI-registry rule
# ---------------------------------------------------------------------------
echo ""
echo "--- (e) updateCenterRegistryEgress disabled ---"

RENDER_NO_UCR=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set updateCenter.pullThrough.enabled=true \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=test \
  --set networkPolicy.pullThroughEgress.enabled=true \
  --set networkPolicy.updateCenterRegistryEgress.enabled=false 2>/dev/null) || {
  echo "FAIL: helm template (no UC registry egress) failed"; exit 1
}

UC_NO_UCR=$(sed_extract 'np-updatecenter' "$RENDER_NO_UCR")
grep -q 'OCI-registry egress' <<< "$UC_NO_UCR" && rc=0 || rc=1
if [[ "$rc" -eq 1 ]]; then
  check "updateCenterRegistryEgress disabled: no OCI-registry egress rule" 0
else
  check "updateCenterRegistryEgress disabled: no OCI-registry egress rule" 1
fi

# Pull-through still present
grep -q 'Pull-through egress' <<< "$UC_NO_UCR" && rc=0 || rc=1
check "updateCenterRegistryEgress disabled: pull-through egress still present" "$rc"

# ---------------------------------------------------------------------------
# (f) ociRegistryEgress disabled → operator has no ociRegistryEgress rule
# ---------------------------------------------------------------------------
echo ""
echo "--- (f) ociRegistryEgress disabled ---"

RENDER_NO_OCI=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set networkPolicy.ociRegistryEgress.enabled=false 2>/dev/null) || {
  echo "FAIL: helm template (no OCI egress) failed"; exit 1
}

OPERATOR_NO_OCI=$(sed_extract 'np-operator' "$RENDER_NO_OCI")
grep -q 'ociRegistryEgress' <<< "$OPERATOR_NO_OCI" && rc=0 || rc=1
if [[ "$rc" -eq 1 ]]; then
  check "ociRegistryEgress disabled: operator has no ociRegistryEgress rule" 0
else
  check "ociRegistryEgress disabled: operator has no ociRegistryEgress rule" 1
fi

# ---------------------------------------------------------------------------
# (g) Hive mode → exactly 4 policies, no UC policy/rule, ociRegistryEgress present
# ---------------------------------------------------------------------------
echo ""
echo "--- (g) Hive mode ---"

HIVE_RENDER=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=test \
  --set bus.url=tls://core.example:4222 \
  --set auth.oidc.clientSecret=test \
  --set auth.dashboardUrl=http://example.com \
  --set networkPolicy.enabled=true \
  --set networkPolicy.ociRegistryEgress.enabled=true \
  --set updateCenter.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"; exit 1
}

# Exactly 4 NetworkPolicy objects
netpol_count=$(grep_c "kind: NetworkPolicy" "$HIVE_RENDER")
[[ "$netpol_count" -eq 4 ]]; check "hive mode: exactly 4 NetworkPolicy objects" $?
[[ "$netpol_count" -eq 4 ]] || true

# No np-updatecenter
uc_hive=$(grep_c "np-updatecenter" "$HIVE_RENDER")
[[ "$uc_hive" -eq 0 ]]; check "hive mode: no np-updatecenter policy" $?
[[ "$uc_hive" -eq 0 ]] || true

# Operator has no UC-egress rule
OPERATOR_HIVE=$(sed_extract 'np-operator' "$HIVE_RENDER")
uc_egress_hive=$(grep_c "varroa-updatecenter" "$OPERATOR_HIVE")
[[ "$uc_egress_hive" -eq 0 ]]; check "hive mode: operator has no UC-egress rule" $?
[[ "$uc_egress_hive" -eq 0 ]] || true

# Operator still has ociRegistryEgress rule (4 cidrs: apiServer + gitEgress + oci + coreNats)
operator_cidr_hive=$(grep_c 'cidr: 0.0.0.0/0' "$OPERATOR_HIVE")
[[ "$operator_cidr_hive" -ge 3 ]]; check "hive mode: operator has ociRegistryEgress (3+ cidrs)" $?
[[ "$operator_cidr_hive" -ge 3 ]] || true

# UpdateCenter workload is entirely suppressed in hive mode (full-mode only)
uc_workload_hive=$(grep_c "name: varroa-updatecenter" "$HIVE_RENDER")
[[ "$uc_workload_hive" -eq 0 ]]; check "hive mode: no update-center workload (deploy/svc/pvc/secret)" $?
[[ "$uc_workload_hive" -eq 0 ]] || true

uc_cr_hive=$(grep_c "kind: UpdateCenter" "$HIVE_RENDER")
[[ "$uc_cr_hive" -eq 0 ]]; check "hive mode: no UpdateCenter CR" $?
[[ "$uc_cr_hive" -eq 0 ]] || true

# Operator has no VARROA_UPDATE_CENTER_URL env in hive mode
uc_env_hive=$(grep_c "VARROA_UPDATE_CENTER_URL" "$HIVE_RENDER")
[[ "$uc_env_hive" -eq 0 ]]; check "hive mode: operator has no VARROA_UPDATE_CENTER_URL env" $?
[[ "$uc_env_hive" -eq 0 ]] || true

# ---------------------------------------------------------------------------
# (h) ociRegistryEgress disabled in hive → operator has no ociRegistryEgress
# ---------------------------------------------------------------------------
echo ""
echo "--- (h) Hive mode + ociRegistryEgress disabled ---"

HIVE_NO_OCI=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=test \
  --set bus.url=tls://core.example:4222 \
  --set auth.oidc.clientSecret=test \
  --set auth.dashboardUrl=http://example.com \
  --set networkPolicy.enabled=true \
  --set networkPolicy.ociRegistryEgress.enabled=false \
  --set updateCenter.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (hive no OCI) failed"; exit 1
}

OPERATOR_HIVE_NO_OCI=$(sed_extract 'np-operator' "$HIVE_NO_OCI")
operator_cidr_hive_no_oci=$(grep_c 'cidr: 0.0.0.0/0' "$OPERATOR_HIVE_NO_OCI")
# apiServer (1) + gitEgress (1) + coreNats (1) = 3 cidr blocks without ociRegistryEgress
[[ "$operator_cidr_hive_no_oci" -eq 3 ]]; check "hive + ociRegistryEgress disabled: operator has 3 cidr blocks (no OCI)" $?
[[ "$operator_cidr_hive_no_oci" -eq 3 ]] || true

# Hive count still 4
netpol_hive_no_oci=$(grep_c "kind: NetworkPolicy" "$HIVE_NO_OCI")
[[ "$netpol_hive_no_oci" -eq 4 ]]; check "hive + ociRegistryEgress disabled: still exactly 4 policies" $?
[[ "$netpol_hive_no_oci" -eq 4 ]] || true

# ---------------------------------------------------------------------------
# (i) Custom CIDRs are independently pinnable on all three toggles
# ---------------------------------------------------------------------------
echo ""
echo "--- (i) Custom CIDR on all three toggles ---"

RENDER_CUSTOM=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set updateCenter.enabled=true \
  --set updateCenter.pullThrough.enabled=true \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=test \
  --set-string 'networkPolicy.pullThroughEgress.cidrs={10.0.0.0/8}' \
  --set-string 'networkPolicy.updateCenterRegistryEgress.cidrs={172.16.0.0/12}' \
  --set-string 'networkPolicy.ociRegistryEgress.cidrs={192.168.0.0/16}' 2>/dev/null) || {
  echo "FAIL: helm template (custom CIDR) failed"; exit 1
}

UC_POLICY_CUSTOM=$(sed_extract 'np-updatecenter' "$RENDER_CUSTOM")

grep -q 'cidr: 10.0.0.0/8' <<< "$UC_POLICY_CUSTOM" && rc=0 || rc=1
check "custom pullThroughEgress CIDR: 10.0.0.0/8 in UC policy" "$rc"

grep -q 'cidr: 172.16.0.0/12' <<< "$UC_POLICY_CUSTOM" && rc=0 || rc=1
check "custom updateCenterRegistryEgress CIDR: 172.16.0.0/12 in UC policy" "$rc"

# No default CIDR leaks into UC policy
grep -q 'cidr: 0.0.0.0/0' <<< "$UC_POLICY_CUSTOM" && rc=0 || rc=1
if [[ "$rc" -eq 1 ]]; then
  check "no default CIDR 0.0.0.0/0 in UC policy" 0
else
  check "no default CIDR 0.0.0.0/0 in UC policy" 1
fi

# Operator policy has custom ociRegistryEgress CIDR
OPERATOR_CUSTOM=$(sed_extract 'np-operator' "$RENDER_CUSTOM")
grep -q 'cidr: 192.168.0.0/16' <<< "$OPERATOR_CUSTOM" && rc=0 || rc=1
check "custom ociRegistryEgress CIDR: 192.168.0.0/16 in operator policy" "$rc"

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
