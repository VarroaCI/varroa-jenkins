#!/usr/bin/env bash
# =============================================================================
# Golden render test: cells-guard.yaml (issue #628)
# =============================================================================
# Verifies the chart refuses a cells.maxTenantsPerCell value outside 1 to 64,
# or a cells.warmPool.perProfile value below the floor the operator seeds, and
# that valid values still render.
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

expect_fail() {
  local desc="$1"
  local extra="$2"
  local msg_pattern="$3"
  set +e
  local out
  out=$(helm template "$ROOT" $COMMON $extra 2>&1)
  local rc=$?
  set -e
  if [[ "$rc" -ne 0 ]]; then rc=0; else rc=1; fi
  check "$desc: render fails" "$rc"
  if [[ -n "$msg_pattern" ]]; then
    grep -q "$msg_pattern" <<< "$out" && rc=0 || rc=1
    check "$desc: error names the field" "$rc"
  fi
}

expect_pass() {
  local desc="$1"
  local extra="$2"
  set +e
  helm template "$ROOT" $COMMON $extra > /dev/null 2>&1
  local rc=$?
  set -e
  check "$desc: render succeeds" "$rc"
}

echo "=== cells-guard.yaml assertions (issue #628) ==="

echo ""
echo "--- (a) default render succeeds ---"
expect_pass "default cells values" ""

echo ""
echo "--- (b) cells.maxTenantsPerCell range ---"
expect_fail "maxTenantsPerCell=0" "--set cells.maxTenantsPerCell=0" "cells.maxTenantsPerCell must be at least 1"
expect_fail "maxTenantsPerCell=-1" "--set cells.maxTenantsPerCell=-1" "cells.maxTenantsPerCell must be at least 1"
expect_pass "maxTenantsPerCell=1" "--set cells.maxTenantsPerCell=1"
expect_fail "maxTenantsPerCell=65" "--set cells.maxTenantsPerCell=65" "cells.maxTenantsPerCell must be at most 64"
expect_pass "maxTenantsPerCell=64" "--set cells.maxTenantsPerCell=64"

echo ""
echo "--- (c) cells.warmPool.perProfile floor ---"
expect_fail "warmPool.perProfile=0" "--set cells.warmPool.perProfile=0" "cells.warmPool.perProfile must be at least 1"
expect_pass "warmPool.perProfile=1" "--set cells.warmPool.perProfile=1"

echo ""
echo "--- (d) whole-number type check ---"
expect_fail "maxTenantsPerCell=true" "--set cells.maxTenantsPerCell=true" "must be a whole number"
expect_pass "maxTenantsPerCell=10 as a numeric string" "--set-string cells.maxTenantsPerCell=10"
expect_fail "maxTenantsPerCell=ten as a string" "--set-string cells.maxTenantsPerCell=ten" "must be a whole number"
# A leading zero in a string would read as octal (base 0) in the int64
# conversion, so the regex in the helper refuses that form before the cast
# sees it. A bare 010 in a values file is converted by the YAML loader before
# the chart sees it, so no chart rule can catch that form.
expect_fail "maxTenantsPerCell=010 as a string" "--set-string cells.maxTenantsPerCell=010" "must be a whole number"
expect_fail "maxTenantsPerCell=99999999999999999999 as a string" "--set-string cells.maxTenantsPerCell=99999999999999999999" "must be at most 2147483647"
expect_fail "warmPool.perProfile=true" "--set cells.warmPool.perProfile=true" "must be a whole number"

echo ""
echo "--- (d2) 32-bit ceiling ---"
expect_fail "maxTenantsPerCell=2147483648" "--set cells.maxTenantsPerCell=2147483648" "cells.maxTenantsPerCell must be at most 2147483647"
expect_fail "maxTenantsPerCell=2147483647" "--set cells.maxTenantsPerCell=2147483647" "cells.maxTenantsPerCell must be at most 64"

echo ""
echo "--- (e) values-file path: a YAML integer renders ---"
vf=$(mktemp)
printf 'cells:\n  maxTenantsPerCell: 4\n  warmPool:\n    perProfile: 2\n' > "$vf"
expect_pass "integers from a values file" "-f $vf"
printf 'cells:\n  maxTenantsPerCell: 2.5\n' > "$vf"
expect_fail "maxTenantsPerCell=2.5 from a values file" "-f $vf" "must be a whole number"
printf 'cells:\n  maxTenantsPerCell: 0\n' > "$vf"
expect_fail "maxTenantsPerCell=0 from a values file" "-f $vf" "cells.maxTenantsPerCell must be at least 1"
printf 'cells:\n  warmPool:\n    perProfile: 0\n' > "$vf"
expect_fail "warmPool.perProfile=0 from a values file" "-f $vf" "cells.warmPool.perProfile must be at least 1"
printf 'cells:\n  maxTenantsPerCell: 2147483648\n' > "$vf"
expect_fail "maxTenantsPerCell=2147483648 from a values file" "-f $vf" "must be at most 2147483647"
printf 'cells:\n  maxTenantsPerCell: 1e19\n' > "$vf"
expect_fail "maxTenantsPerCell=1e19 from a values file" "-f $vf" "must be at most 2147483647"
printf 'cells:\n  maxTenantsPerCell: 64\n  warmPool:\n    perProfile: 2147483647\n' > "$vf"
set +e
rendered=$(helm template "$ROOT" $COMMON -f "$vf" 2>&1 |
  grep -A1 -e 'name: VARROA_CELLS_MAX_TENANTS' -e 'name: VARROA_CELLS_WARM_POOL_PER_PROFILE' |
  grep -c -e 'value: "64"' -e 'value: "2147483647"')
set -e
if [[ "$rendered" -eq 3 ]]; then rc=0; else rc=1; fi
check "a large values-file integer renders as a plain integer on every seeded env" "$rc"
rm -f "$vf"

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
