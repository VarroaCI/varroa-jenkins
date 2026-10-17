#!/usr/bin/env bash
# =============================================================================
# Golden render test: completion webhook egress on the recorder NetworkPolicy
# =============================================================================
# Verifies that networkPolicy.recorderWebhookEgress is off by default, that
# enabling it adds the configured CIDR and port to np-recorder, that enabling
# it with an empty cidrs list fails the render, and that the recorder receives
# VARROA_DASHBOARD_URL from auth.dashboardUrl.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test"

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

# recorder_policy prints the np-recorder document from a render.
recorder_policy() {
  awk '/name: .*-np-recorder$/{p=1} p&&/^---/{exit} p' <<< "$1"
}

echo "=== Recorder completion webhook egress golden render assertions ==="

# ---------------------------------------------------------------------------
# (a) Default: the webhook egress rule is absent.
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Default (off) ---"

RENDER_DEFAULT=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (default) failed"; exit 1
}
REC_DEFAULT=$(recorder_policy "$RENDER_DEFAULT")
rc=0
grep -q 'cidr: 0.0.0.0/0' <<< "$REC_DEFAULT" && rc=1 || true
check "webhook egress off by default: np-recorder has no open rule" "$rc"

# ---------------------------------------------------------------------------
# (b) Enabled with a pinned CIDR: the rule carries that CIDR and port 8443.
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) Enabled with a pinned CIDR ---"

RENDER_ON=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true \
  --set networkPolicy.recorderWebhookEgress.enabled=true \
  --set 'networkPolicy.recorderWebhookEgress.cidrs[0]=10.42.8.0/24' \
  --set 'networkPolicy.recorderWebhookEgress.ports[0]=8443' 2>/dev/null) || {
  echo "FAIL: helm template (enabled) failed"; exit 1
}
REC_ON=$(recorder_policy "$RENDER_ON")
rc=0
grep -q 'cidr: 10.42.8.0/24' <<< "$REC_ON" || rc=1
check "enabled: np-recorder carries the configured CIDR" "$rc"
rc=0
grep -q 'port: 8443' <<< "$REC_ON" || rc=1
check "enabled: np-recorder carries the configured port" "$rc"

# ---------------------------------------------------------------------------
# (c) Enabled with no CIDR: the render fails and names the value.
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) Enabled with no CIDR ---"

rc=0
OUT_OPEN=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true \
  --set networkPolicy.recorderWebhookEgress.enabled=true 2>&1) && rc=1 || rc=0
check "enabled with no cidrs: helm template fails" "$rc"
rc=0
grep -q 'networkPolicy.recorderWebhookEgress.cidrs must name' <<< "$OUT_OPEN" || rc=1
check "enabled with no cidrs: failure names recorderWebhookEgress.cidrs" "$rc"
rc=0
grep -q '0.0.0.0/0' <<< "$OUT_OPEN" || rc=1
check "enabled with no cidrs: failure names the 0.0.0.0/0 opt-in" "$rc"

# ---------------------------------------------------------------------------
# (d) The recorder receives the dashboard URL.
# ---------------------------------------------------------------------------
echo ""
echo "--- (d) Recorder env ---"

REC_DEPLOY=$(awk 'BEGIN{RS="\n---"} /kind: Deployment/ && /component: varroa-recorder/' <<< "$RENDER_DEFAULT")
rc=0
grep -q 'name: VARROA_DASHBOARD_URL' <<< "$REC_DEPLOY" || rc=1
check "recorder deployment sets VARROA_DASHBOARD_URL" "$rc"
rc=0
grep -q 'value: "https://varroa.example.test"' <<< "$REC_DEPLOY" || rc=1
check "VARROA_DASHBOARD_URL carries auth.dashboardUrl" "$rc"

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "All recorder webhook egress assertions passed."
