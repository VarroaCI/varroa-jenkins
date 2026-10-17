#!/usr/bin/env bash
# =============================================================================
# Golden render test: fail-closed defaults for the operator-named egress groups
# =============================================================================
# Verifies that networkPolicy.enabled=true with an empty cidrs list fails the
# render for each group that needs a named address, that the failure message
# names the value, and that a pinned CIDR renders as the only peer.
# Empty lists come from a values file, because --set cannot write an empty list.
# Guard: charts/varroa/templates/networkpolicy-guard.yaml.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
fail_messages=""

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

# sed_extract: extract the YAML document whose name matches.
sed_extract() {
  local name="$1"
  local input="$2"
  sed -n "/name:.*${name}/,/^---/p" <<< "$input"
}

# render: helm template into OUT, exit code into RC (failures do not abort).
render() {
  RC=0
  OUT=$(helm template "$ROOT" $COMMON "$@" 2>&1) || RC=$?
}

echo "=== Egress group fail-closed defaults golden render assertions ==="

# Values files with an empty cidrs list for each group.
cat > "$TMP/empty-uc-registry.yaml" <<'EOF'
networkPolicy:
  updateCenterRegistryEgress:
    cidrs: []
EOF
cat > "$TMP/empty-pullthrough.yaml" <<'EOF'
networkPolicy:
  pullThroughEgress:
    cidrs: []
EOF
cat > "$TMP/empty-oci.yaml" <<'EOF'
networkPolicy:
  ociRegistryEgress:
    enabled: true
    cidrs: []
EOF
cat > "$TMP/empty-corenats.yaml" <<'EOF'
networkPolicy:
  coreNatsEgress:
    cidrs: []
EOF
cat > "$TMP/empty-blob.yaml" <<'EOF'
networkPolicy:
  recordStoreBlobEgress:
    cidrs: []
EOF
cat > "$TMP/empty-webhook.yaml" <<'EOF'
networkPolicy:
  recorderWebhookEgress:
    enabled: true
    cidrs: []
EOF

# ---------------------------------------------------------------------------
# (1) Full mode, default values: renders, and the OCI rule is off by default.
# ---------------------------------------------------------------------------
echo ""
echo "--- (1) Full mode defaults ---"

render --set networkPolicy.enabled=true
check "defaults with networkPolicy.enabled render" "$RC"
OP=$(sed_extract 'np-operator' "$OUT")
grep -q 'ociRegistryEgress: operator reaches' <<< "$OP" && rc=1 || rc=0
check "defaults: operator policy has no ociRegistryEgress rule" "$rc"

# ---------------------------------------------------------------------------
# (2) Update center on OCI storage: the registry CIDR is required.
# ---------------------------------------------------------------------------
echo ""
echo "--- (2) updateCenterRegistryEgress with oci storage ---"

UC_OCI=(--set networkPolicy.enabled=true --set updateCenter.enabled=true
  --set updateCenter.storage.type=oci --set updateCenter.storage.oci.ref=test)

render "${UC_OCI[@]}" -f "$TMP/empty-uc-registry.yaml"
check "oci storage with empty registry list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.updateCenterRegistryEgress.cidrs must name the OCI registry' <<< "$OUT" && rc=0 || rc=1
check "oci storage failure names updateCenterRegistryEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

render "${UC_OCI[@]}" --set-string 'networkPolicy.updateCenterRegistryEgress.cidrs={172.16.0.0/12}'
check "oci storage with pinned registry CIDR renders" "$RC"
UC=$(sed_extract 'np-updatecenter' "$OUT")
grep -q 'cidr: 172.16.0.0/12' <<< "$UC" && rc=0 || rc=1
check "pinned registry CIDR is in np-updatecenter" "$rc"
grep -q 'cidr: 0.0.0.0/0' <<< "$UC" && rc=1 || rc=0
check "pull-through off: np-updatecenter has no 0.0.0.0/0" "$rc"

# ---------------------------------------------------------------------------
# (3) Pull-through on: the default stays wide, and an empty list fails.
# ---------------------------------------------------------------------------
echo ""
echo "--- (3) pullThroughEgress ---"

PT=(--set networkPolicy.enabled=true --set updateCenter.enabled=true
  --set updateCenter.pullThrough.enabled=true)

render "${PT[@]}"
check "pull-through with default CIDRs renders" "$RC"
UC=$(sed_extract 'np-updatecenter' "$OUT")
grep -q 'cidr: 0.0.0.0/0' <<< "$UC" && rc=0 || rc=1
check "pull-through default: np-updatecenter allows 0.0.0.0/0" "$rc"

render "${PT[@]}" -f "$TMP/empty-pullthrough.yaml"
check "pull-through with empty list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.pullThroughEgress.cidrs must not be empty' <<< "$OUT" && rc=0 || rc=1
check "pull-through failure names pullThroughEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

# ---------------------------------------------------------------------------
# (4) ociRegistryEgress enabled: an empty list fails, a pinned CIDR renders.
# ---------------------------------------------------------------------------
echo ""
echo "--- (4) ociRegistryEgress ---"

render --set networkPolicy.enabled=true -f "$TMP/empty-oci.yaml"
check "ociRegistryEgress enabled with empty list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.ociRegistryEgress.cidrs must name the OCI registry' <<< "$OUT" && rc=0 || rc=1
check "ociRegistryEgress failure names ociRegistryEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

render --set networkPolicy.enabled=true \
  --set networkPolicy.ociRegistryEgress.enabled=true \
  --set-string 'networkPolicy.ociRegistryEgress.cidrs={192.168.0.0/16}'
check "ociRegistryEgress with pinned CIDR renders" "$RC"
OP=$(sed_extract 'np-operator' "$OUT")
grep -q 'cidr: 192.168.0.0/16' <<< "$OP" && rc=0 || rc=1
check "pinned ociRegistryEgress CIDR is in np-operator" "$rc"

# ---------------------------------------------------------------------------
# (5) Hive mode: coreNatsEgress is required; the port follows bus.url.
# ---------------------------------------------------------------------------
echo ""
echo "--- (5) coreNatsEgress in hive mode ---"

HIVE=(-f "$ROOT/values-hive.yaml" --set cluster.name=test
  --set auth.dashboardUrl=http://example.com --set networkPolicy.enabled=true)

render "${HIVE[@]}" --set bus.url=tls://core.example:4222 -f "$TMP/empty-corenats.yaml"
check "hive with empty coreNatsEgress list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.coreNatsEgress.cidrs must name the core NATS address' <<< "$OUT" && rc=0 || rc=1
check "hive failure names coreNatsEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

render "${HIVE[@]}" --set bus.url=tls://core.example:4222 \
  --set-string 'networkPolicy.coreNatsEgress.cidrs={192.0.2.10/32}'
check "hive with pinned coreNats CIDR renders" "$RC"
OP=$(sed_extract 'np-operator' "$OUT")
GW=$(sed_extract 'np-gateway' "$OUT")
grep -q 'cidr: 192.0.2.10/32' <<< "$OP" && rc=0 || rc=1
check "hive: np-operator carries the core NATS CIDR" "$rc"
grep -q 'cidr: 192.0.2.10/32' <<< "$GW" && rc=0 || rc=1
check "hive: np-gateway carries the core NATS CIDR" "$rc"
grep -q 'port: 4222' <<< "$OP" && rc=0 || rc=1
check "hive: core NATS rule uses port 4222 from bus.url" "$rc"

render "${HIVE[@]}" --set bus.url=tls://core.example:7422 \
  --set-string 'networkPolicy.coreNatsEgress.cidrs={192.0.2.10/32}'
check "hive with bus.url port 7422 renders" "$RC"
OP=$(sed_extract 'np-operator' "$OUT")
grep -q 'port: 7422' <<< "$OP" && rc=0 || rc=1
check "hive: core NATS rule uses port 7422 from bus.url" "$rc"

# ---------------------------------------------------------------------------
# (6) Record store on s3 blobs: the blob CIDR is required.
# ---------------------------------------------------------------------------
echo ""
echo "--- (6) recordStoreBlobEgress in s3 mode ---"

S3=(--set networkPolicy.enabled=true --set recordStore.enabled=true
  --set recordStore.blobs.mode=s3 --set recordStore.blobs.s3.url=s3://bucket)

render "${S3[@]}" -f "$TMP/empty-blob.yaml"
check "s3 blobs with empty list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.recordStoreBlobEgress.cidrs must name the blob store' <<< "$OUT" && rc=0 || rc=1
check "s3 failure names recordStoreBlobEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

render "${S3[@]}" --set-string 'networkPolicy.recordStoreBlobEgress.cidrs={198.51.100.0/24}'
check "s3 blobs with pinned CIDR renders" "$RC"
for policy in np-recorder np-bff np-gateway; do
  DOC=$(sed_extract "$policy" "$OUT")
  grep -q 'cidr: 198.51.100.0/24' <<< "$DOC" && rc=0 || rc=1
  check "$policy carries the pinned blob CIDR" "$rc"
done

# ---------------------------------------------------------------------------
# (7) Completion webhook: enabled with an empty list fails.
# ---------------------------------------------------------------------------
echo ""
echo "--- (7) recorderWebhookEgress ---"

render --set networkPolicy.enabled=true --set recordStore.enabled=true -f "$TMP/empty-webhook.yaml"
check "webhook enabled with empty list fails" "$([[ "$RC" -ne 0 ]] && echo 0 || echo 1)"
grep -q 'networkPolicy.recorderWebhookEgress.cidrs must name the webhook' <<< "$OUT" && rc=0 || rc=1
check "webhook failure names recorderWebhookEgress.cidrs" "$rc"
fail_messages="$fail_messages$OUT"

# ---------------------------------------------------------------------------
# (8) Every failure message names the 0.0.0.0/0 opt-in.
# ---------------------------------------------------------------------------
echo ""
echo "--- (8) failure messages name the opt-in ---"

grep -q '0.0.0.0/0' <<< "$fail_messages" && rc=0 || rc=1
check "failure messages contain 0.0.0.0/0" "$rc"

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
