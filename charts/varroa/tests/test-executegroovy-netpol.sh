#!/usr/bin/env bash
# =============================================================================
# Golden render test: executeGroovy NetworkPolicy egress assertions
# =============================================================================
# Verifies that `helm template` produces the correct operator egress rule for
# executeGroovy dispatch (port 8080 to target controller namespaces).
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

echo "=== executeGroovy NetworkPolicy golden render assertions ==="

# 1. networkPolicy.enabled=true, managedNamespaces empty (default)
RENDER_EMPTY=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (empty namespaces) failed"
  exit 1
}

echo "$RENDER_EMPTY" | grep -A8 'executeGroovy' | grep -q 'namespaceSelector: {}'
check "empty managedNamespaces: rule targets all namespaces (namespaceSelector: {})" $?

echo "$RENDER_EMPTY" | grep -A10 'executeGroovy' | grep -q 'port: 8080'
check "empty managedNamespaces: port 8080 present" $?

echo "$RENDER_EMPTY" | grep -A10 'executeGroovy' | grep -q 'protocol: TCP'
check "empty managedNamespaces: protocol TCP" $?

# 2. networkPolicy.enabled=true, managedNamespaces={ns-a,ns-b}
RENDER_SCP=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true \
  --set-string 'managedNamespaces={ns-a,ns-b}' 2>/dev/null) || {
  echo "FAIL: helm template (scoped namespaces) failed"
  exit 1
}

echo "$RENDER_SCP" | grep -A8 'executeGroovy' | grep -q 'kubernetes.io/metadata.name: ns-a'
check "scoped namespaces: rule targets ns-a" $?

echo "$RENDER_SCP" | grep -A8 'executeGroovy' | grep -q 'kubernetes.io/metadata.name: ns-b'
check "scoped namespaces: rule targets ns-b" $?

echo "$RENDER_SCP" | grep -A12 'executeGroovy' | grep -q 'port: 8080'
check "scoped namespaces: port 8080 present" $?

# Assert no bare namespaceSelector: {} in this scoped rule
if echo "$RENDER_SCP" | grep -A15 'executeGroovy' | grep -q 'namespaceSelector: {}'; then
  check "scoped namespaces: NO bare namespaceSelector: {} in executeGroovy rule" 1
else
  check "scoped namespaces: NO bare namespaceSelector: {} in executeGroovy rule" 0
fi

# 3. networkPolicy.enabled=false → NO executeGroovy rule
RENDER_DISABLED=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=false 2>/dev/null) || {
  echo "FAIL: helm template (disabled) failed"
  exit 1
}

groovy_lines=$(echo "$RENDER_DISABLED" | grep -c 'executeGroovy' || true)
if [[ "$groovy_lines" -eq 0 ]]; then
  check "networkPolicy disabled: no executeGroovy rule present" 0
else
  check "networkPolicy disabled: no executeGroovy rule present" 1
fi

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
