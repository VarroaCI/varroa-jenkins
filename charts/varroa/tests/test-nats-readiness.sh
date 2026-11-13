#!/usr/bin/env bash
# =============================================================================
# Golden render test: NATS pod probes
# =============================================================================
# Verifies that:
#   - the readiness probe checks JetStream meta health (js-meta-only)
#   - the startup probe keeps the full /healthz check
#   - the liveness probe keeps js-enabled-only
#   - the readiness path holds in single-server mode (cluster disabled)
#   - hive mode renders no varroa-nats StatefulSet
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

# nats_container <helm args...> prints the nats container of the varroa-nats StatefulSet.
nats_container() {
  helm template varroa "$ROOT" "${BASE[@]}" "$@" 2>/dev/null |
    yq eval-all 'select(.kind == "StatefulSet" and .metadata.name == "varroa-nats") | .spec.template.spec.containers[] | select(.name == "nats")'
}

# probe_path <container yaml> <probe name> prints the httpGet path of one probe.
probe_path() {
  yq eval ".$2.httpGet.path" - <<<"$1"
}

echo "=== 1. Default probes ==="
NATS=$(nats_container)
[[ -n "$NATS" ]]
check "varroa-nats StatefulSet renders a nats container" $?
[[ "$(probe_path "$NATS" readinessProbe)" == "/healthz?js-meta-only=true" ]]
check "readiness checks JetStream meta health (js-meta-only)" $?
[[ "$(yq eval '.readinessProbe.httpGet.port' - <<<"$NATS")" == "monitor" ]]
check "readiness probes the monitor port" $?
[[ "$(probe_path "$NATS" startupProbe)" == "/healthz" ]]
check "startup keeps the full /healthz check" $?
[[ "$(probe_path "$NATS" livenessProbe)" == "/healthz?js-enabled-only=true" ]]
check "liveness keeps js-enabled-only" $?

echo ""
echo "=== 2. Single-server mode ==="
SINGLE=$(nats_container --set nats.config.cluster.enabled=false)
[[ "$(probe_path "$SINGLE" readinessProbe)" == "/healthz?js-meta-only=true" ]]
check "readiness keeps js-meta-only with the cluster disabled" $?

echo ""
echo "=== 3. Hive mode ==="
HIVE=$(helm template varroa "$ROOT" -f "$ROOT/values-hive.yaml" \
  --set cluster.name=member-b --set bus.url=nats://h:4222 "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"
  exit 1
}
HIVE_SS=$(yq eval-all 'select(.kind == "StatefulSet" and .metadata.name == "varroa-nats")' <<<"$HIVE")
[[ -z "$HIVE_SS" ]]
check "hive renders no varroa-nats StatefulSet" $?

echo ""
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures check(s)"
  exit 1
fi
echo "All checks passed."
