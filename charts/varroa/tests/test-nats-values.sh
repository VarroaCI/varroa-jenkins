#!/usr/bin/env bash
# =============================================================================
# Golden render test: the NATS values the subchart reads
# =============================================================================
# Verifies that:
#   - the default render runs the pinned server image nats:2.10.16-alpine
#   - nats.container.image.tag reaches the NATS StatefulSet
#   - nats.config.jetstream.fileStore.pvc.size reaches the JetStream claim
#   - each retired key (nats.image, nats.persistence, nats.service.port,
#     nats.service.monitorPort, nats.service.type) fails the render and
#     names the key to set instead
#   - a client port other than 4222 or a monitor port other than 8222 fails
#   - no template reads a retired key (NOTES.txt needs a cluster to render)
# =============================================================================
# No errexit: each assertion reads $? through check().
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE=(--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test)

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

# expect_fail <desc> <message pattern> <helm args...>
expect_fail() {
  local desc="$1"
  local pattern="$2"
  shift 2
  local out
  if out=$(helm template varroa "$ROOT" "${BASE[@]}" "$@" 2>&1); then
    check "$desc (render must fail)" 1
    return
  fi
  grep -qF -- "$pattern" <<<"$out"
  check "$desc" $?
}

echo "=== 1. Server image ==="
RENDER=$(helm template varroa "$ROOT" "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: helm template failed"
  exit 1
}
grep -q 'image: nats:2.10.16-alpine$' <<<"$RENDER"
check "default render runs nats:2.10.16-alpine" $?

OVERRIDE=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.container.image.tag=9.9.9-alpine 2>/dev/null)
grep -q 'image: nats:9.9.9-alpine$' <<<"$OVERRIDE"
check "nats.container.image.tag reaches the StatefulSet" $?

echo "=== 2. JetStream volume ==="
grep -q 'storage: 10Gi$' <<<"$RENDER"
check "default JetStream claim is 10Gi" $?

PVC=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.config.jetstream.fileStore.pvc.size=33Gi 2>/dev/null)
grep -q 'storage: 33Gi$' <<<"$PVC"
check "nats.config.jetstream.fileStore.pvc.size reaches the claim" $?

echo "=== 3. Retired keys fail the render ==="
expect_fail "nats.image fails" "set nats.container.image instead" --set nats.image.tag=x
expect_fail "nats.persistence fails" "set nats.config.jetstream.fileStore.pvc.size" --set nats.persistence.size=1Gi
expect_fail "nats.service.port fails" "nats.service.port is not read by the NATS subchart; the chart fixes the client port at 4222" --set nats.service.port=5555
expect_fail "nats.service.monitorPort fails" "nats.service.monitorPort is not read" --set nats.service.monitorPort=8222
expect_fail "nats.service.type fails and names nats.external" "set nats.external.enabled" --set nats.service.type=NodePort

expect_fail "nats.config.nats.port other than 4222 fails" "nats.config.nats.port must stay 4222" --set nats.config.nats.port=5555
expect_fail "nats.config.monitor.port other than 8222 fails" "nats.config.monitor.port must stay 8222" --set nats.config.monitor.port=9999

echo "=== 4. No template reads a retired key ==="
! grep -rqE '\.Values\.nats\.(image|persistence|service)\b' "$ROOT/templates"
check "templates/ reads no retired nats key" $?

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
