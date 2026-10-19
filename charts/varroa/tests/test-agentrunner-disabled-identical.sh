#!/usr/bin/env bash
# =============================================================================
# Golden render test: the default render is unchanged by the Team agent runner
# =============================================================================
# With agentRunner.enabled false, the render holds no agent runner object,
# NATS user, grant, or mount. When BASE_CHART names another chart directory
# (for example the chart of main), its masked render must equal this one byte
# for byte. Per-render random values are masked first, as in
# test-assistant-disabled-identical.sh.
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

# mask removes the lines that hold a per-render random value.
mask() {
  grep -v -E '\$2a\$|ca\.(crt|key):|tls\.(crt|key):|password:|secret: "|-password:|key: "?[A-Za-z0-9+/=]{40,}"?$|: "?[A-Za-z0-9+/=]{60,}"?$' || true
}

echo "=== Default render: the agent runner is absent ==="
RENDER=$(helm template "$ROOT" $COMMON 2>/dev/null) || { echo "FAIL: helm template (default) failed"; exit 1; }
MASKED=$(mask <<< "$RENDER")

rc=0; grep -q 'varroa-agent' <<< "$MASKED" && rc=1 || true
check "no varroa-agent string in the masked default render" "$rc"
rc=0; grep -q 'agentrunner' <<< "$MASKED" && rc=1 || true
check "no agentrunner string in the masked default render" "$rc"
rc=0; grep -q 'team-agent' <<< "$MASKED" && rc=1 || true
check "no team-agent string in the masked default render" "$rc"
rc=0; grep -q 'team-agents' <<< "$MASKED" && rc=1 || true
check "no -team-agents flag in the masked default render" "$rc"

if [[ -n "${BASE_CHART:-}" ]]; then
  echo ""
  echo "=== Masked render equals the base chart: $BASE_CHART ==="
  BASE=$(helm template release-name "$BASE_CHART" $COMMON 2>/dev/null) || { echo "FAIL: helm template (base) failed"; exit 1; }
  BASE_MASKED=$(mask <<< "$BASE")
  rc=0
  diff_out=$(mktemp)
  if ! diff <(printf '%s\n' "$BASE_MASKED") <(printf '%s\n' "$MASKED") > "$diff_out" 2>&1; then
    rc=1
    head -40 "$diff_out"
  fi
  rm -f "$diff_out"
  check "masked default render is byte-identical to the base render" "$rc"
fi

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "PASSED"
