#!/usr/bin/env bash
# =============================================================================
# Golden render test: the agent assistant chart surface
# =============================================================================
# Verifies that the assistant is off by default, that enabling it needs the
# record store, that the BFF gets only the enable flag (the providers live in
# the AssistantConfig object), that the provider key Secret mounts as one
# optional volume, that the retired values fail the render, and that the
# model-provider egress rule renders only with its CIDRs.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test"
ON="$COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true --set assistant.enabled=true"

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
rc=0; grep -q 'VARROA_ASSISTANT_ENABLED' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no VARROA_ASSISTANT_ENABLED in the default render" "$rc"
rc=0; grep -q 'varroa-assistant-keys' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no provider key volume in the default render" "$rc"
rc=0; grep -q 'agentsessions' <<< "$RENDER_DEFAULT" && rc=1 || true
check "no agentsessions grant in the default render" "$rc"

# (b) Enabled without the record store fails the render.
echo ""
echo "--- (b) Enabled without recordStore ---"
rc=0
helm template "$ROOT" $COMMON --set assistant.enabled=true >/dev/null 2>&1 && rc=1 || true
check "assistant.enabled without recordStore.enabled fails" "$rc"

# (c) Enabled: the BFF gets the flag, and the key Secret mounts once.
echo ""
echo "--- (c) Enabled ---"
RENDER_ON=$(helm template "$ROOT" $ON 2>/dev/null) || { echo "FAIL: helm template (enabled) failed"; exit 1; }
rc=0; grep -q 'VARROA_ASSISTANT_ENABLED' <<< "$RENDER_ON" || rc=1
check "BFF carries VARROA_ASSISTANT_ENABLED" "$rc"
rc=0; grep -q 'VARROA_ASSISTANT_CONFIG' <<< "$RENDER_ON" && rc=1 || true
check "no VARROA_ASSISTANT_CONFIG in the enabled render" "$rc"
rc=0; grep -q 'agentsessions' <<< "$RENDER_ON" || rc=1
check "the bff NATS user holds the agentsessions grant" "$rc"
rc=0; grep -q 'cidr: 192.168.1.99/32' <<< "$(np_bff "$RENDER_ON")" && rc=1 || true
check "np-bff carries no assistant egress CIDR (the operator renders it)" "$rc"
rc=0; grep -q 'varroa-assistant-egress' <<< "$RENDER_ON" && rc=1 || true
check "the chart renders no varroa-assistant-egress policy" "$rc"
rc=0; grep -q 'secretName: varroa-assistant-keys' <<< "$RENDER_ON" || rc=1
check "the key Secret is a volume source" "$rc"
rc=0; grep -q 'optional: true' <<< "$RENDER_ON" || rc=1
check "the key Secret volume is optional" "$rc"
rc=0; grep -q 'mountPath: /etc/varroa/assistant$' <<< "$RENDER_ON" || rc=1
check "the key volume mounts at /etc/varroa/assistant" "$rc"
rc=0; grep -A3 'name: assistant-keys' <<< "$RENDER_ON" | grep -q 'subPath' && rc=1 || true
check "the key mount has no subPath" "$rc"

# (d) The retired networkPolicy.assistantEgress values fail the render.
echo ""
echo "--- (d) Retired networkPolicy.assistantEgress ---"
rc=0
helm template "$ROOT" $ON --set networkPolicy.assistantEgress.ports[0]=443 >/dev/null 2>&1 && rc=1 || true
check "networkPolicy.assistantEgress fails the render" "$rc"

# (e) The retired values fail the render and name the object.
echo ""
echo "--- (e) Retired values ---"
for key in "assistant.providers[0].name=smoke" "assistant.budgets.session=1" \
  "assistant.approvalExpiry=1h" "assistant.externalDataSharing=logs"; do
  rc=0
  helm template "$ROOT" $ON --set "$key" >/dev/null 2>&1 && rc=1 || true
  check "$key fails the render" "$rc"
done

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
