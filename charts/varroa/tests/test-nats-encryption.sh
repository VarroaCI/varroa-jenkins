#!/usr/bin/env bash
# =============================================================================
# Golden render test: JetStream encryption at rest
# =============================================================================
# Verifies that `helm template` produces:
#   - nats.conf with cipher chachapoly and an unquoted $JS_KEY variable
#   - JS_KEY on the NATS container from the varroa-nats-js-key Secret
#   - the key Secret with the keep policy and a 64-character key
#   - no key in varroa-nats-creds, and no other workload that reads the key
#   - NATS pod annotations that do not change with the key
#   - nats.encryption.createSecret=false: no Secret, JS_KEY still set
#   - nats.encryption=null or an empty createSecret: the Secret still renders
#   - a render without the merge keys or JS_KEY fails
#   - hive mode: no key Secret and no JS_KEY
# The lookup reuse and the missing-key guard need a live cluster, so this
# test does not cover them.
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

RENDER=$(helm template varroa "$ROOT" "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: helm template failed"
  exit 1
}

echo "=== 1. NATS server config ==="
NATS_CONF=$(echo "$RENDER" | yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "varroa-nats-config") | .data["nats.conf"]')
[[ -n "$NATS_CONF" ]]
check "nats.conf renders" $?
grep -q '"cipher": "chachapoly",' <<< "$NATS_CONF"
check "jetstream block sets cipher chachapoly" $?
grep -qE '"key": \$JS_KEY,?$' <<< "$NATS_CONF"
check "jetstream key is the unquoted \$JS_KEY variable" $?
grep -q '"\$JS_KEY"' <<< "$NATS_CONF" && check "\$JS_KEY is not a quoted string" 1 || check "\$JS_KEY is not a quoted string" 0
grep -qE 'prev_(encryption_)?(key|ek)' <<< "$NATS_CONF" && check "no prev_encryption_key" 1 || check "no prev_encryption_key" 0

echo ""
echo "=== 2. NATS container env ==="
JS_REF=$(echo "$RENDER" | yq eval-all 'select(.kind == "StatefulSet" and .metadata.name == "varroa-nats") | .spec.template.spec.containers[] | select(.name == "nats") | .env[] | select(.name == "JS_KEY") | .valueFrom.secretKeyRef.name + "/" + .valueFrom.secretKeyRef.key')
[[ "$JS_REF" == "varroa-nats-js-key/key" ]]
check "JS_KEY comes from secretKeyRef varroa-nats-js-key/key (got '$JS_REF')" $?

echo ""
echo "=== 3. Key Secret ==="
POLICY=$(echo "$RENDER" | yq eval-all 'select(.kind == "Secret" and .metadata.name == "varroa-nats-js-key") | .metadata.annotations["helm.sh/resource-policy"]')
[[ "$POLICY" == "keep" ]]
check "key Secret carries helm.sh/resource-policy: keep" $?
KEY=$(echo "$RENDER" | yq eval-all 'select(.kind == "Secret" and .metadata.name == "varroa-nats-js-key") | .data.key' | base64 -d)
[[ ${#KEY} -eq 64 ]]
check "key is 64 characters (got ${#KEY})" $?

echo ""
echo "=== 4. Key isolation ==="
CREDS_KEYS=$(echo "$RENDER" | yq eval-all 'select(.kind == "Secret" and .metadata.name == "varroa-nats-creds") | .data | keys | .[]')
[[ -n "$CREDS_KEYS" ]]
check "varroa-nats-creds renders" $?
grep -qiE 'js|encrypt' <<< "$CREDS_KEYS" && check "varroa-nats-creds holds no encryption key" 1 || check "varroa-nats-creds holds no encryption key" 0
READERS=$(echo "$RENDER" | yq eval-all 'select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet" or .kind == "Job") | select(. | tostring | test("varroa-nats-js-key")) | .metadata.name')
[[ "$READERS" == "varroa-nats" ]]
check "only the varroa-nats StatefulSet reads the key Secret (got '$(echo $READERS)')" $?
# Each render mints a new key. The NATS pod annotations must not change
# with it, or a key change would roll NATS.
NATS_ANNOTATIONS='select(.kind == "StatefulSet" and .metadata.name == "varroa-nats") | .spec.template.metadata.annotations'
ANN_A=$(echo "$RENDER" | yq eval-all "$NATS_ANNOTATIONS")
RENDER_B=$(helm template varroa "$ROOT" "${BASE[@]}" 2>/dev/null)
KEY_B=$(echo "$RENDER_B" | yq eval-all 'select(.kind == "Secret" and .metadata.name == "varroa-nats-js-key") | .data.key' | base64 -d)
ANN_B=$(echo "$RENDER_B" | yq eval-all "$NATS_ANNOTATIONS")
[[ -n "$KEY_B" && "$KEY" != "$KEY_B" ]]
check "a second render mints a different key" $?
[[ -n "$ANN_A" && "$ANN_A" == "$ANN_B" ]]
check "NATS pod annotations do not change with the key" $?

echo ""
echo "=== 5. createSecret=false ==="
BYO=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.encryption.createSecret=false 2>/dev/null) || {
  echo "FAIL: helm template (createSecret=false) failed"
  exit 1
}
grep -q "^# Source: varroa/templates/secret-nats-js-key.yaml" <<< "$BYO" && check "no key Secret rendered" 1 || check "no key Secret rendered" 0
grep -q "name: JS_KEY" <<< "$BYO"
check "JS_KEY env still rendered" $?

EMPTY=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.encryption.createSecret= 2>&1)
grep -q "^# Source: varroa/templates/secret-nats-js-key.yaml" <<< "$EMPTY"
check "an empty createSecret keeps the default and renders the Secret" $?
NULL=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.encryption=null 2>&1)
grep -q "^# Source: varroa/templates/secret-nats-js-key.yaml" <<< "$NULL"
check "nats.encryption=null keeps the default and renders the Secret" $?

echo ""
echo "=== 6. Encryption guard ==="
# --reuse-values drops new chart defaults. The guard refuses such a render.
OUT=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.config.jetstream.merge=null 2>&1)
grep -q "JetStream encryption is not configured" <<< "$OUT"
check "a render without the merge keys fails" $?
OUT=$(helm template varroa "$ROOT" "${BASE[@]}" --set nats.container.env=null 2>&1)
grep -q "JetStream encryption is not configured" <<< "$OUT"
check "a render without the JS_KEY env fails" $?

echo ""
echo "=== 7. Hive mode ==="
HIVE=$(helm template varroa "$ROOT" -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x --set bus.url=nats://h:4222 "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"
  exit 1
}
grep -q "nats-js-key" <<< "$HIVE" && check "hive renders no key Secret" 1 || check "hive renders no key Secret" 0
grep -q "JS_KEY" <<< "$HIVE" && check "hive renders no JS_KEY" 1 || check "hive renders no JS_KEY" 0

echo ""
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures check(s)"
  exit 1
fi
echo "All checks passed."
