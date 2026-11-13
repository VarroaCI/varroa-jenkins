#!/usr/bin/env bash
# =============================================================================
# Golden render test: the agent assistant chart surface
# =============================================================================
# Verifies that the assistant is off by default (no config, no bus grants),
# that enabling it needs the record store, that the BFF receives the config
# without any Secret name, that the provider key Secret mounts as an optional
# file, and that the model-provider egress rule renders only with its CIDRs.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test"
PROVIDER="--set assistant.providers[0].name=smoke --set assistant.providers[0].kind=openai-compatible --set assistant.providers[0].baseURL=http://192.168.1.99:8082/v1 --set assistant.providers[0].models[0]=varroa-smoke --set assistant.providers[0].trust=internal --set assistant.providers[0].default=true"
ON="$COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true --set assistant.enabled=true $PROVIDER --set networkPolicy.assistantEgress.cidrs[0]=192.168.1.99/32 --set networkPolicy.assistantEgress.ports[0]=8082"

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

# np_bff prints the np-bff document from a render.
np_bff() {
  awk '/name: .*-np-bff$/{p=1} p&&/^---/{exit} p' <<< "$1"
}

echo "=== Assistant chart golden render assertions ==="

# (a) Default: the assistant renders nothing.
echo ""
echo "--- (a) Default (off) ---"
RENDER_DEFAULT=$(helm template "$ROOT" $COMMON 2>/dev/null) || { echo "FAIL: helm template (default) failed"; exit 1; }
rc=0; grep -q 'VARROA_ASSISTANT_CONFIG' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no VARROA_ASSISTANT_CONFIG in the default render" "$rc"
rc=0; grep -q 'agentsessions' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no agentsessions grant in the default render" "$rc"

# (b) Enabled without the record store fails the render.
echo ""
echo "--- (b) Enabled without recordStore ---"
rc=0
helm template "$ROOT" $COMMON --set assistant.enabled=true $PROVIDER >/dev/null 2>&1 && rc=1 || true
check "assistant.enabled without recordStore.enabled fails" "$rc"

# (c) Enabled: the config reaches the BFF without a Secret name.
echo ""
echo "--- (c) Enabled with a provider ---"
RENDER_ON=$(helm template "$ROOT" $ON 2>/dev/null) || { echo "FAIL: helm template (enabled) failed"; exit 1; }
rc=0; grep -q 'VARROA_ASSISTANT_CONFIG' <<< "$RENDER_ON" || rc=1
check "BFF carries VARROA_ASSISTANT_CONFIG" "$rc"
rc=0; grep -q 'varroa-smoke' <<< "$RENDER_ON" || rc=1
check "the config names the provider model" "$rc"
rc=0; grep -q 'agentsessions' <<< "$RENDER_ON" || rc=1
check "the bff NATS user holds the agentsessions grant" "$rc"
rc=0; grep -q 'cidr: 192.168.1.99/32' <<< "$(np_bff "$RENDER_ON")" || rc=1
check "np-bff carries the assistant egress CIDR" "$rc"
rc=0; grep -q 'port: 8082' <<< "$(np_bff "$RENDER_ON")" || rc=1
check "np-bff carries the assistant egress port" "$rc"

# (d) Enabled with networkPolicy on and no CIDRs fails the render.
echo ""
echo "--- (d) Enabled with empty assistantEgress.cidrs ---"
rc=0
helm template "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true \
  --set assistant.enabled=true $PROVIDER >/dev/null 2>&1 && rc=1 || true
check "empty assistantEgress.cidrs fails when the assistant is on" "$rc"

# (e) A provider Secret mounts as an optional apiKey file, never a subPath.
echo ""
echo "--- (e) Provider key Secret ---"
RENDER_KEY=$(helm template "$ROOT" $ON --set assistant.providers[0].secretRef=smoke-key 2>/dev/null) || { echo "FAIL: helm template (secretRef) failed"; exit 1; }
rc=0; grep -q 'secretName: smoke-key' <<< "$RENDER_KEY" || rc=1
check "the key Secret is a volume source" "$rc"
rc=0; grep -q 'optional: true' <<< "$RENDER_KEY" || rc=1
check "the key Secret volume is optional" "$rc"
rc=0; grep -q 'mountPath: /etc/varroa/assistant/smoke' <<< "$RENDER_KEY" || rc=1
check "the key mounts under the provider name" "$rc"
rc=0; grep -A3 'name: assistant-key-0' <<< "$RENDER_KEY" | grep -q 'subPath' && rc=1 || true
check "the key mount has no subPath" "$rc"
rc=0; grep 'VARROA_ASSISTANT_CONFIG' -A1 <<< "$RENDER_KEY" | grep -q 'smoke-key' && rc=1 || true
check "the Secret name never reaches the config env" "$rc"

# (f) The AssistantConfig write route: the operator flag and its RBAC render
# only with the assistant on. Section (g) checks the hive member.
echo ""
echo "--- (f) AssistantConfig route ---"
rc=0; grep -q -- '- "-assistant"' <<< "$RENDER_DEFAULT" && rc=1 || true
check "the operator carries no -assistant flag by default" "$rc"
rc=0; grep -q -- '- "-assistant"' <<< "$RENDER_ON" || rc=1
check "the operator carries -assistant when the assistant is on" "$rc"
rc=0; grep -q 'assistantconfigs' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no assistantconfigs RBAC in the default render" "$rc"
rc=0; grep -q 'assistantconfigs' <<< "$RENDER_ON" || rc=1
check "assistantconfigs RBAC renders when the assistant is on" "$rc"

# (g) Hive member: the operator gets no -assistant flag and no AssistantConfig
# write rule. The hive render needs the same guard inputs as test-nats-server-ping.sh.
echo ""
echo "--- (g) Hive member ---"
RENDER_HIVE=$(helm template varroa "$ROOT" -f "$ROOT/values-hive.yaml" \
  --set cluster.name=member-b --set bus.url=nats://h:4222 $COMMON \
  --set recordStore.enabled=true --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=rs-dsn \
  --set assistant.enabled=true 2>/dev/null) || { echo "FAIL: helm template (hive) failed"; exit 1; }
rc=0; grep -q -- '- "-assistant"' <<< "$RENDER_HIVE" && rc=1 || true
check "a hive member carries no -assistant flag" "$rc"
rc=0; grep -q 'operator applies the AssistantConfig singleton' <<< "$RENDER_HIVE" && rc=1 || true
check "a hive member carries no operator AssistantConfig write rule" "$rc"

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "PASSED"
