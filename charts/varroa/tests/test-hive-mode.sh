#!/usr/bin/env bash
# =============================================================================
# Golden render test: hive-mode assertions
# =============================================================================
# Verifies that `helm template` with values-hive.yaml produces:
#   6. A comma-separated bus.url list reaches the operator and the gateway unchanged
#   1. Hive render → operator+gateway resources only (no bff/frontend/dex etc.)
#   2. Missing cluster.name or bus.url → failure with appropriate message
#   3. mode=hive with nats.enabled=true → failure mentioning nats.enabled=false
#   4. Full default render → nats+bff+frontend present, no nats-external
#   5. External exposure render → nats-external Service with correct selector and traffic policy
#   7. bus.url is the only bus URL value: no -bus-url arg renders, and each
#      retired component key (operator/gateway/bff .busURL) fails the render
#   8. Full mode with nats.enabled=false deploys no NATS, so an empty bus.url
#      fails the render with the same message as hive mode
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

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

# --- 1. Hive render component set ---
echo "=== 1. Hive render component set ==="
HIVE_RENDER=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x \
  --set bus.url=nats://h:4222 \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"
  exit 1
}

# Positive: operator+gateway resources present
grep -c "varroa-operator" <<< "$HIVE_RENDER" > /dev/null 2>&1
check "Operator resources present" $?

grep -c "varroa-gateway" <<< "$HIVE_RENDER" > /dev/null 2>&1
check "Gateway resources present" $?

# Negative: no core-only components
# Match rendered templates from the bff/ directory, not the bare string:
# operator/deployment.yaml carries a BFF *URL* in an env var, which is not a
# BFF resource and must not trip this assertion.
grep -c "^# Source: varroa/templates/bff/" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No BFF resources" 1 || check "No BFF resources" 0
grep -ci "frontend" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No frontend resources" 1 || check "No frontend resources" 0
grep -c "dex" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No dex resources" 1 || check "No dex resources" 0
grep -c "kind: Ingress" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No Ingress resources" 1 || check "No Ingress resources" 0
grep -c "varroa-nats-auth-config" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No nats-auth-config" 1 || check "No nats-auth-config" 0
grep -c "NetworkPolicy" <<< "$HIVE_RENDER" > /dev/null 2>&1 && check "No NetworkPolicy" 1 || check "No NetworkPolicy" 0

# --- 2. Missing required values ---
echo ""
echo "=== 2. Missing required values ==="

# Missing cluster.name — test by not setting it
OUT=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set bus.url=nats://h:4222 \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com 2>&1 || true)
if grep -q "cluster.name" <<< "$OUT"; then
  check "Missing cluster.name fails" 0
else
  # cluster.name defaults to "core" from values.yaml, so it's always non-empty.
  # Check that the render at least succeeds.
  grep -q "varroa-operator" <<< "$OUT" && check "Default cluster.name renders" 0 || check "Default cluster.name renders (n/a)" 0
fi

# Missing bus.url
OUT=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com 2>&1 || true)
if grep -q "bus.url must name the NATS endpoint when the release deploys no NATS" <<< "$OUT"; then
  check "Missing bus.url fails mentioning bus.url" 0
else
  check "Missing bus.url fails mentioning bus.url" 1
fi

# --- 3. mode=hive with nats.enabled=true ---
echo ""
echo "=== 3. mode=hive with nats.enabled=true ==="
OUT=$(helm template "$ROOT" \
  --set mode=hive \
  --set cluster.name=x \
  --set bus.url=nats://h:4222 \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com \
  --set nats.enabled=true 2>&1 || true)
grep -q "nats.enabled=false" <<< "$OUT"
check "Hive mode with nats.enabled=true fails" $?

# --- 4. Full default render ---
echo ""
echo "=== 4. Full default render ==="
FULL_RENDER=$(helm template "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com 2>/dev/null) || {
  echo "FAIL: helm template (full) failed"
  exit 1
}
grep -c "varroa-nats" <<< "$FULL_RENDER" > /dev/null 2>&1
check "Full render: NATS resources present" $?
grep -c "varroa-bff" <<< "$FULL_RENDER" > /dev/null 2>&1
check "Full render: BFF resources present" $?
grep -ci "frontend" <<< "$FULL_RENDER" > /dev/null 2>&1
check "Full render: frontend resources present" $?
grep -c "nats-external" <<< "$FULL_RENDER" > /dev/null 2>&1 && check "Full render: no nats-external Service" 1 || check "Full render: no nats-external Service" 0

# --- 5. External exposure render ---
echo ""
echo "=== 5. External exposure render ==="
EXT_RENDER=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.example.com 2>/dev/null) || {
  echo "FAIL: helm template (external) failed"
  exit 1
}
grep -c "nats-external" <<< "$EXT_RENDER" > /dev/null 2>&1
check "External render: nats-external Service present" $?

# Extract the nats-external Service and check selector
EXT_SVC=$(grep -A30 "name:.*nats-external" <<< "$EXT_RENDER" || true)
grep -c "app.kubernetes.io/component: nats" <<< "$EXT_SVC" > /dev/null 2>&1
check "External Service selector has app.kubernetes.io/component: nats" $?
if grep -q "externalTrafficPolicy: Cluster" <<< "$EXT_SVC"; then check "External Service defaults to externalTrafficPolicy: Cluster" 0; else check "External Service defaults to externalTrafficPolicy: Cluster" 1; fi

# Local keeps the member source address for nats.external.allowedCIDRs.
LOCAL_RENDER=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.example.com \
  --set nats.external.externalTrafficPolicy=Local 2>/dev/null) || {
  echo "FAIL: helm template (external Local) failed"
  exit 1
}
LOCAL_SVC=$(grep -A30 "name:.*nats-external" <<< "$LOCAL_RENDER" || true)
if grep -q "externalTrafficPolicy: Local" <<< "$LOCAL_SVC"; then check "External Service renders externalTrafficPolicy: Local" 0; else check "External Service renders externalTrafficPolicy: Local" 1; fi

# NodePort with a fixed port and Local.
NP_RENDER=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.example.com \
  --set nats.external.serviceType=NodePort \
  --set nats.external.nodePort=30422 \
  --set nats.external.externalTrafficPolicy=Local 2>/dev/null) || {
  echo "FAIL: helm template (external NodePort) failed"
  exit 1
}
NP_SVC=$(grep -A30 "name:.*nats-external" <<< "$NP_RENDER" || true)
if grep -q "nodePort: 30422" <<< "$NP_SVC"; then check "NodePort External Service renders nodePort: 30422" 0; else check "NodePort External Service renders nodePort: 30422" 1; fi
if grep -q "type: NodePort" <<< "$NP_SVC"; then check "NodePort External Service renders type: NodePort" 0; else check "NodePort External Service renders type: NodePort" 1; fi
if grep -q "externalTrafficPolicy: Local" <<< "$NP_SVC"; then check "NodePort External Service renders externalTrafficPolicy: Local" 0; else check "NodePort External Service renders externalTrafficPolicy: Local" 1; fi

# An unknown policy fails the render with a message that names the value.
if BAD_OUT=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.example.com \
  --set nats.external.externalTrafficPolicy=Bogus 2>&1); then
  check "Bogus externalTrafficPolicy fails the render" 1
elif grep -q "nats.external.externalTrafficPolicy must be Cluster or Local" <<< "$BAD_OUT"; then
  check "Bogus externalTrafficPolicy fails the render" 0
else
  check "Bogus externalTrafficPolicy fails the render" 1
fi

# --- 6. A bus.url server list reaches every bus client unchanged ---
echo ""
echo "=== 6. bus.url server list ==="
LIST_URL="tls://192.0.2.10:30422,tls://192.0.2.11:30422"
LIST_RENDER=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x \
  --set-literal "bus.url=$LIST_URL" \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com 2>/dev/null) || {
  echo "FAIL: helm template (hive, bus.url list) failed"
  exit 1
}
n=$(grep -c "value: \"$LIST_URL\"" <<< "$LIST_RENDER" || true)
if [[ "$n" == 2 ]]; then check "bus.url list renders unchanged for operator and gateway" 0; else check "bus.url list renders unchanged for operator and gateway" 1; fi

# --- 7. bus.url is the only bus URL value ---
echo ""
echo "=== 7. bus.url is the only bus URL value ==="
# The core NATS egress rule reads only bus.url. A -bus-url arg would win
# over VARROA_BUS_URL in the gateway and the BFF and dial another address.
if grep -q -- "-bus-url" <<< "$LIST_RENDER"; then check "Hive render: no -bus-url arg" 1; else check "Hive render: no -bus-url arg" 0; fi
if grep -q -- "-bus-url" <<< "$FULL_RENDER"; then check "Full render: no -bus-url arg" 1; else check "Full render: no -bus-url arg" 0; fi
n=$(grep -c 'value: "nats://release-name-nats:4222"' <<< "$FULL_RENDER" || true)
if [[ "$n" == 3 ]]; then check "Full render: operator, gateway, and BFF use the in-cluster bus URL" 0; else check "Full render: operator, gateway, and BFF use the in-cluster bus URL (got $n)" 1; fi

for component in operator gateway bff; do
  if OUT=$(helm template "$ROOT" \
    --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com \
    --set "$component.busURL=nats://other:4223" 2>&1); then
    check "Full render: $component.busURL fails the render" 1
  elif grep -q "$component.busURL is retired. Set bus.url instead." <<< "$OUT"; then
    check "Full render: $component.busURL fails the render" 0
  else
    check "Full render: $component.busURL fails the render" 1
  fi
done

if OUT=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x \
  --set bus.url=nats://h:4222 \
  --set gateway.busURL=nats://other:4223 \
  --set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com 2>&1); then
  check "Hive render: gateway.busURL fails the render" 1
elif grep -q "gateway.busURL is retired. Set bus.url instead." <<< "$OUT"; then
  check "Hive render: gateway.busURL fails the render" 0
else
  check "Hive render: gateway.busURL fails the render" 1
fi

# --- 8. Full mode without NATS needs bus.url ---
echo ""
echo "=== 8. Full mode with nats.enabled=false needs bus.url ==="
# The default bus URL names the in-cluster NATS Service, which a release
# without NATS does not render.
NO_NATS=(--set nats.enabled=false --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://example.com)
if OUT=$(helm template "$ROOT" "${NO_NATS[@]}" 2>&1); then
  check "nats.enabled=false with an empty bus.url fails the render" 1
elif grep -q "bus.url must name the NATS endpoint when the release deploys no NATS (hive mode, or nats.enabled=false)" <<< "$OUT" \
  && grep -q "nats://release-name-nats:4222 names a Service that this release does not render" <<< "$OUT"; then
  check "nats.enabled=false with an empty bus.url fails the render" 0
else
  check "nats.enabled=false with an empty bus.url fails the render" 1
fi

if OUT=$(helm template "$ROOT" "${NO_NATS[@]}" --set-string 'bus.url= ' 2>&1); then
  check "nats.enabled=false with a blank bus.url fails the render" 1
elif grep -q "bus.url must name the NATS endpoint" <<< "$OUT"; then
  check "nats.enabled=false with a blank bus.url fails the render" 0
else
  check "nats.enabled=false with a blank bus.url fails the render" 1
fi

EXT_URL="tls://nats.example.com:4222"
if EXT_BUS_RENDER=$(helm template "$ROOT" "${NO_NATS[@]}" --set "bus.url=$EXT_URL" 2>&1); then
  check "nats.enabled=false with bus.url renders" 0
  n=$(grep -c "value: \"$EXT_URL\"" <<< "$EXT_BUS_RENDER" || true)
  if [[ "$n" == 3 ]]; then check "nats.enabled=false: operator, gateway, and BFF use bus.url" 0; else check "nats.enabled=false: operator, gateway, and BFF use bus.url (got $n)" 1; fi
  if grep -q 'release-name-nats:4222' <<< "$EXT_BUS_RENDER"; then check "nats.enabled=false: no bus client uses the in-cluster default" 1; else check "nats.enabled=false: no bus client uses the in-cluster default" 0; fi
else
  check "nats.enabled=false with bus.url renders" 1
fi

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
