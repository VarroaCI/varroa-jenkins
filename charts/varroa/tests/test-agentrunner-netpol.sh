#!/usr/bin/env bash
# =============================================================================
# Golden render test: the agentRunner NetworkPolicy
# =============================================================================
# The varroa-np-agentrunner policy reaches NATS, the BFF, and the record store.
# It holds no git or forge egress: the operator owns every forge call. The BFF
# policy admits the runner on 8080.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test)
ENABLED=(
  --set assistant.enabled=true
  --set recordStore.enabled=true
  --set agentRunner.enabled=true
  --set assistant.providers[0].name=smoke
  --set assistant.providers[0].kind=openai-compatible
  --set assistant.providers[0].baseURL=http://192.0.2.10:8082/v1
  --set assistant.providers[0].trust=internal
  --set assistant.providers[0].models[0]=test-model
  --set assistant.providers[0].default=true
  --set networkPolicy.assistantEgress.cidrs[0]=192.0.2.10/32
  --set networkPolicy.assistantEgress.ports[0]=8082
  --set networkPolicy.enabled=true
  --set networkPolicy.gitEgress.enabled=true
  --set networkPolicy.gitEgress.cidrs[0]=192.0.2.20/32
  --set networkPolicy.gitEgress.ports[0]=443
)

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

echo "=== agentRunner NetworkPolicy ==="
RENDER=$(helm template "$ROOT" "${COMMON[@]}" "${ENABLED[@]}" 2>/dev/null) || { echo "FAIL: helm template failed"; exit 1; }
POLICY=$(sed -n '/name: release-name-np-agentrunner/,/^---/p' <<< "$RENDER")

rc=0; [[ -n "$POLICY" ]] || rc=1
check "varroa-np-agentrunner renders" "$rc"

rc=0; grep -q 'app.kubernetes.io/component: varroa-agent' <<< "$POLICY" || rc=1
check "the policy selects the varroa-agent pods" "$rc"

rc=0; grep -q 'app.kubernetes.io/component: varroa-bff' <<< "$POLICY" || rc=1
check "the policy reaches the BFF" "$rc"

rc=0; grep -q 'cidr: 192.0.2.20/32' <<< "$POLICY" && rc=1 || true
check "the policy holds no git or forge egress" "$rc"

rc=0; grep -q 'cidr: 192.0.2.10/32' <<< "$POLICY" || rc=1
check "the policy reaches the assistant provider" "$rc"

BFF_POLICY=$(sed -n '/name: release-name-np-bff/,/^---/p' <<< "$RENDER")
rc=0; grep -q 'app.kubernetes.io/component: varroa-agent' <<< "$BFF_POLICY" || rc=1
check "the BFF policy admits the runner" "$rc"

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "PASSED"
