#!/usr/bin/env bash
# =============================================================================
# Golden render test: NATS server ping settings
# =============================================================================
# Verifies that:
#   - the default nats.conf sets ping_interval "20s" and ping_max 2
#   - the cluster block sets no ping_interval, so routes use the top value
#   - the auth include stays the last entry of nats.conf
#   - nats.config.merge.ping_interval and .ping_max reach nats.conf
#   - a change to the ping settings changes the NATS config checksum
#   - hive mode renders no NATS server config
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

# nats_conf <helm args...> prints nats.conf of the varroa-nats-config ConfigMap.
nats_conf() {
  helm template varroa "$ROOT" "${BASE[@]}" "$@" 2>/dev/null |
    yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "varroa-nats-config") | .data["nats.conf"]'
}

# nats_checksum <helm args...> prints the config checksum of the NATS pods.
nats_checksum() {
  helm template varroa "$ROOT" "${BASE[@]}" "$@" 2>/dev/null |
    yq eval-all 'select(.kind == "StatefulSet" and .metadata.name == "varroa-nats") | .spec.template.metadata.annotations["checksum/config"]'
}

echo "=== 1. Default ping settings ==="
CONF=$(nats_conf)
[[ -n "$CONF" ]]
check "nats.conf renders" $?
grep -qx '  "ping_interval": "20s",' <<<"$CONF"
check "ping_interval is 20s" $?
grep -qx '  "ping_max": 2,' <<<"$CONF"
check "ping_max is 2" $?
[[ $(grep -c '"ping_interval"' <<<"$CONF") -eq 1 ]]
check "only the top level sets ping_interval (routes follow it)" $?
[[ $(grep -c '"ping_max"' <<<"$CONF") -eq 1 ]]
check "only the top level sets ping_max (routes follow it)" $?

echo ""
echo "=== 2. Auth include stays last ==="
LAST=$(grep -v '^[[:space:]]*$' <<<"$CONF" | tail -n 2 | head -n 1)
[[ "$LAST" == "  include ../nats-auth/auth.conf;" ]]
check "include ../nats-auth/auth.conf is the last entry (got '$LAST')" $?

echo ""
echo "=== 3. Overrides reach nats.conf ==="
OVR=$(nats_conf --set nats.config.merge.ping_interval=45s --set nats.config.merge.ping_max=3)
grep -qx '  "ping_interval": "45s",' <<<"$OVR"
check "nats.config.merge.ping_interval reaches nats.conf" $?
grep -qx '  "ping_max": 3,' <<<"$OVR"
check "nats.config.merge.ping_max reaches nats.conf" $?
[[ "$(nats_checksum)" != "$(nats_checksum --set nats.config.merge.ping_interval=45s)" ]]
check "a ping change changes checksum/config, so the NATS pods roll" $?

echo ""
echo "=== 4. Hive mode ==="
HIVE=$(helm template varroa "$ROOT" -f "$ROOT/values-hive.yaml" \
  --set cluster.name=member-b --set bus.url=nats://h:4222 "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"
  exit 1
}
grep -q "varroa-nats-config" <<<"$HIVE" && check "hive renders no NATS server config" 1 || check "hive renders no NATS server config" 0

echo ""
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures check(s)"
  exit 1
fi
echo "All checks passed."
