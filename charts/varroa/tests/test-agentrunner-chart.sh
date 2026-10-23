#!/usr/bin/env bash
# =============================================================================
# Golden render test: the agentRunner component
# =============================================================================
# Verifies the guard, the Deployment, the RBAC, the signing-key Secret shape,
# the NATS user and grants, and the operator and BFF wiring of the Tenant agent
# runner. The signing key value is never printed: the test checks its key name
# and the decoded length only.
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

# has reports 0 when the render contains the pattern.
has() {
  grep -q -- "$2" <<< "$1"
  echo $?
}

echo "=== agentRunner guard ==="
for case in "assistant.enabled=false" "recordStore.enabled=false" "bff.enabled=false"; do
  rc=0
  helm template "$ROOT" "${COMMON[@]}" "${ENABLED[@]}" --set "$case" > /dev/null 2>&1 && rc=1 || true
  check "agentRunner.enabled refuses $case" "$rc"
done

echo ""
echo "=== agentRunner enabled render ==="
RENDER=$(helm template "$ROOT" "${COMMON[@]}" "${ENABLED[@]}" 2>/dev/null) || { echo "FAIL: helm template (enabled) failed"; exit 1; }

check "Deployment varroa-agent renders" "$(has "$RENDER" 'command: \["/app/varroa-agent"\]')"
check "Deployment reads the NATS agentrunner password" "$(has "$RENDER" 'agentrunner-password')"
check "Deployment mounts the signing key at /etc/varroa/tenant-agent-key" "$(has "$RENDER" 'mountPath: /etc/varroa/tenant-agent-key')"
check "Deployment carries the BFF URL" "$(has "$RENDER" 'VARROA_BFF_URL')"
check "ClusterRole reads tenantagents, tenants, and catalogsources" "$(has "$RENDER" 'resources: \["tenantagents", "tenants", "catalogsources"\]')"
check "ClusterRole updates only the tenantagents status" "$(has "$RENDER" 'resources: \["tenantagents/status"\]')"
check "Role grants the status lease" "$(has "$RENDER" 'resources: \["leases"\]')"
check "operator takes the -tenant-agents flag" "$(has "$RENDER" '"-tenant-agents"')"
check "operator ClusterRole reads tenantagents" "$(has "$RENDER" 'resources: \["tenantagents"\]')"

RBAC_RENDER=$(helm template "$ROOT" "${COMMON[@]}" "${ENABLED[@]}" -s templates/agentrunner/clusterrole.yaml -s templates/agentrunner/role.yaml 2>/dev/null)
rc=0; grep -q 'secrets' <<< "$RBAC_RENDER" && rc=1 || true
check "agent RBAC names no secrets resource" "$rc"

DEPLOY_RENDER=$(helm template "$ROOT" "${COMMON[@]}" "${ENABLED[@]}" -s templates/agentrunner/deployment.yaml 2>/dev/null)
check "Deployment declares requests" "$(has "$DEPLOY_RENDER" 'requests:')"
check "Deployment declares a memory limit" "$(has "$DEPLOY_RENDER" 'memory: 512Mi')"
check "Deployment hardens the container" "$(has "$DEPLOY_RENDER" 'allowPrivilegeEscalation: false')"
check "Deployment runs as non-root" "$(has "$DEPLOY_RENDER" 'runAsNonRoot: true')"

echo ""
echo "=== signing key Secret ==="
KEY_B64=$(awk '/name: varroa-tenant-agent-signing-key/{f=1} f && /^  key: /{print $2; exit}' <<< "$RENDER")
rc=0
[[ -n "$KEY_B64" ]] || rc=1
check "Secret varroa-tenant-agent-signing-key has key 'key'" "$rc"
rc=0
decoded_len=$(printf '%s' "$KEY_B64" | base64 -d 2>/dev/null | wc -c | tr -d ' ') || rc=1
[[ "$decoded_len" == 32 ]] || rc=1
check "signing key decodes to 32 bytes" "$rc"

echo ""
echo "=== NATS grants ==="
check "agentrunner NATS user is rendered" "$(has "$RENDER" 'user: "agentrunner"')"
check "agentrunner may read the activity consumer only" "$(has "$RENDER" '"\$JS.API.CONSUMER.MSG.NEXT.varroa_activity.>"')"
AGENT_USER=$(sed -n "/user: \"agentrunner\"/,/user: \"recorder\"/p" <<< "$RENDER")
rc=0; grep -q "STREAM.CREATE.varroa_activity" <<< "$AGENT_USER" && rc=1 || true
check "agentrunner holds no activity stream write" "$rc"
check "bff may publish the webhook enqueue subject" "$(has "$RENDER" 'varroa.recordstore.webhook.enqueue')"
check "operator may reply into the runner inbox" "$(has "$RENDER" '_INBOX_agentrunner')"
check "recorder publishes activity.>" "$(has "$RENDER" '"activity.>"')"

echo ""
echo "=== disabled render ==="
DEFAULT=$(helm template "$ROOT" "${COMMON[@]}" 2>/dev/null) || { echo "FAIL: helm template (default) failed"; exit 1; }
rc=0; grep -q 'varroa-agent' <<< "$DEFAULT" && rc=1 || true
check "default render has no varroa-agent object" "$rc"
rc=0; grep -q 'tenant-agent-key' <<< "$DEFAULT" && rc=1 || true
check "default render has no signing-key mount" "$rc"

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "PASSED"
