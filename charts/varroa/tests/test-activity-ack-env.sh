#!/usr/bin/env bash
# =============================================================================
# Golden render test: activity ack mode (VARROA_ACTIVITY_ACK)
# =============================================================================
#   1. recordStore.enabled=true: the operator, the gateway, and the BFF each
#      carry VARROA_ACTIVITY_ACK set to "1".
#   2. recordStore.enabled=false: no Deployment carries VARROA_ACTIVITY_ACK.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
failures=0

check() {
  if [[ "$2" == 0 ]]; then echo "  PASS: $1"; else echo "  FAIL: $1"; failures=$((failures + 1)); fi
}

doc() {
  # doc <kind> <name> <render>: print one rendered object.
  awk -v k="kind: $1" -v n="name: $2" '
    /^---/ { if (hit) print buf; buf=""; hit=0; kind=0; next }
    { buf = buf $0 "\n" }
    $0 == k { kind=1 }
    kind && $0 ~ "^  " n "$" { hit=1 }
    END { if (hit) print buf }' <<< "$3"
}

AUTH=(--set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com)

echo "=== 1. Record store on ==="
R=$(helm template t "$ROOT" "${AUTH[@]}" --set recordStore.enabled=true)
for d in t-varroa-operator t-varroa-gateway t-varroa-bff; do
  D=$(doc Deployment "$d" "$R")
  grep -A1 "name: VARROA_ACTIVITY_ACK" <<< "$D" | grep -q 'value: "1"'; check "$d sets VARROA_ACTIVITY_ACK=1" $?
done

echo "=== 2. Record store off ==="
R=$(helm template t "$ROOT" "${AUTH[@]}")
grep -q "VARROA_ACTIVITY_ACK" <<< "$R" && rc=1 || rc=0; check "no Deployment sets VARROA_ACTIVITY_ACK" $rc

echo ""
if [[ "$failures" -eq 0 ]]; then echo "=== ALL ASSERTIONS PASSED ==="; else echo "=== $failures FAILURE(S) ==="; exit 1; fi
