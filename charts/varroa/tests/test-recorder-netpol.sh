#!/usr/bin/env bash
# =============================================================================
# Golden render test: build-record store NetworkPolicy assertions (D10)
# =============================================================================
# Verifies that `helm template` produces the correct NetworkPolicy rules for
# the varroa-recorder component and the record-store egress additions to the
# bff and gateway policies.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com"

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

grep_c() {
  local pattern="$1"
  local input="$2"
  grep -c "$pattern" <<< "$input" 2>/dev/null || true
}

sed_extract() {
  local name="$1"
  local input="$2"
  sed -n "/name:.*${name}/,/^---/p" <<< "$input"
}

echo "=== Build-record store NetworkPolicy golden render assertions (D10) ==="

# ---------------------------------------------------------------------------
# (a) Default (recordStore disabled): no recorder policy, no record-store
#     rule anywhere.
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Default (disabled) ---"

RENDER_DISABLED=$(helm template "$ROOT" $COMMON --set networkPolicy.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (disabled) failed"; exit 1
}
found=$(grep_c 'np-recorder' "$RENDER_DISABLED")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recordStore disabled: no np-recorder policy" "$rc"

BFF_DISABLED=$(sed_extract 'np-bff' "$RENDER_DISABLED")
found=$(grep_c 'cnpg.io/cluster\|varroa-recordstore' "$BFF_DISABLED")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recordStore disabled: bff has no record-store rule" "$rc"

GATEWAY_DISABLED=$(sed_extract 'np-gateway' "$RENDER_DISABLED")
found=$(grep_c 'cnpg.io/cluster\|varroa-recordstore' "$GATEWAY_DISABLED")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recordStore disabled: gateway has no record-store rule" "$rc"

RENDER_RS_ONLY=$(helm template "$ROOT" $COMMON --set recordStore.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (recordStore only) failed"; exit 1
}
found=$(grep_c 'np-recorder' "$RENDER_RS_ONLY")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recordStore enabled, networkPolicy disabled: no np-recorder" "$rc"

# A whole-render diff between the enabled and disabled renders, minus the
# recorder-owned objects and the record-store env/policy additions, would
# need to strip an open-ended set of per-workload blocks and is brittle
# in bash. Instead: a disabled render must carry zero occurrences of
# every string a record-store addition uses, so no partial addition can
# hide from this test.
for pattern in "recordstore" "recorder" "cnpg" "VARROA_RECORDSTORE_DSN"; do
  found=$(grep -ic "$pattern" <<< "$RENDER_DISABLED" || true)
  if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
  check "recordStore disabled: zero occurrences of \"$pattern\"" "$rc"
done

# ---------------------------------------------------------------------------
# (b) Full render, cnpg mode (default): recorder policy present, Postgres
#     egress selects the CNPG pod label, blob egress absent (postgres blob
#     mode), metrics ingress admits the configured peer.
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) Full render, cnpg + postgres blob mode ---"

RENDER_FULL=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true \
  --set-json 'networkPolicy.metricsIngress=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]' 2>/dev/null) || {
  echo "FAIL: helm template (full, cnpg) failed"; exit 1
}

grep -q 'name:.*np-recorder' <<< "$RENDER_FULL" && rc=0 || rc=1
check "np-recorder policy object present" "$rc"

RECORDER_POLICY=$(sed_extract 'np-recorder' "$RENDER_FULL")

grep -q 'cnpg.io/cluster: varroa-recordstore' <<< "$RECORDER_POLICY" && rc=0 || rc=1
check "recorder egress: empty cidrs selects the CNPG pod label" "$rc"

found=$(grep_c '0.0.0.0/0' "$RECORDER_POLICY")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recorder egress: no blob rule in postgres blob mode" "$rc"

grep -q 'kubernetes.io/metadata.name: monitoring' <<< "$RECORDER_POLICY" && rc=0 || rc=1
check "recorder ingress: configured metricsIngress peer admitted" "$rc"

grep -q 'port: 4222' <<< "$RECORDER_POLICY" && rc=0 || rc=1
check "recorder egress: nats pods on 4222" "$rc"

BFF_FULL=$(sed_extract 'np-bff' "$RENDER_FULL")
grep -q 'cnpg.io/cluster: varroa-recordstore' <<< "$BFF_FULL" && rc=0 || rc=1
check "bff gains the record-store Postgres rule" "$rc"

GATEWAY_FULL=$(sed_extract 'np-gateway' "$RENDER_FULL")
grep -q 'cnpg.io/cluster: varroa-recordstore' <<< "$GATEWAY_FULL" && rc=0 || rc=1
check "gateway gains the Postgres rule in postgres blob mode" "$rc"

# ---------------------------------------------------------------------------
# (c) recorder has no ingress rule when metricsIngress is empty.
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) No metricsIngress peers ---"

RENDER_NO_METRICS=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (no metricsIngress) failed"; exit 1
}
RECORDER_NO_METRICS=$(sed_extract 'np-recorder' "$RENDER_NO_METRICS")
found=$(echo "$RECORDER_NO_METRICS" | sed -n '/^  ingress:/,/^  egress:/p' | grep -c '    - from:' || true)
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recorder: no ingress rule when metricsIngress is empty" "$rc"

# ---------------------------------------------------------------------------
# (d) blobs.mode=s3: blob egress present on recorder/bff/gateway; gateway
#     loses the Postgres rule.
# ---------------------------------------------------------------------------
echo ""
echo "--- (d) blobs.mode=s3 ---"

RENDER_S3=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true \
  --set recordStore.blobs.mode=s3 \
  --set recordStore.blobs.s3.url=s3://bucket 2>/dev/null) || {
  echo "FAIL: helm template (blobs=s3) failed"; exit 1
}

RECORDER_S3=$(sed_extract 'np-recorder' "$RENDER_S3")
grep -q '0.0.0.0/0' <<< "$RECORDER_S3" && rc=0 || rc=1
check "recorder: blob egress present in s3 mode" "$rc"
grep -q 'port: 443' <<< "$RECORDER_S3" && rc=0 || rc=1
check "recorder: blob egress port 443" "$rc"

BFF_S3=$(sed_extract 'np-bff' "$RENDER_S3")
grep -q '0.0.0.0/0' <<< "$BFF_S3" && rc=0 || rc=1
check "bff: blob egress present in s3 mode" "$rc"

GATEWAY_S3=$(sed_extract 'np-gateway' "$RENDER_S3")
grep -q '0.0.0.0/0' <<< "$GATEWAY_S3" && rc=0 || rc=1
check "gateway: blob egress present in s3 mode" "$rc"
found=$(grep_c 'cnpg.io/cluster' "$GATEWAY_S3")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "gateway: no Postgres rule in s3 blob mode" "$rc"

# ---------------------------------------------------------------------------
# (e) External Postgres with an empty cidrs list fails the render.
# ---------------------------------------------------------------------------
echo ""
echo "--- (e) External Postgres, empty cidrs ---"

set +e
ERR_EXTERNAL=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true \
  --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=my-secret 2>&1)
rc=$?
set -e
if [[ "$rc" -ne 0 ]]; then rc=0; else rc=1; fi
check "external Postgres + empty cidrs: render fails" "$rc"
grep -q 'pin the Postgres endpoint' <<< "$ERR_EXTERNAL" && rc=0 || rc=1
check "external Postgres + empty cidrs: message tells the operator to pin the endpoint" "$rc"

# ---------------------------------------------------------------------------
# (f) External Postgres with a pinned CIDR renders an ipBlock rule.
# ---------------------------------------------------------------------------
echo ""
echo "--- (f) External Postgres, pinned cidrs ---"

RENDER_EXT_PINNED=$(helm template "$ROOT" $COMMON \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true \
  --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=my-secret \
  --set-string 'networkPolicy.recordStorePostgresEgress.cidrs={10.20.30.40/32}' 2>/dev/null) || {
  echo "FAIL: helm template (external pinned) failed"; exit 1
}
RECORDER_EXT_PINNED=$(sed_extract 'np-recorder' "$RENDER_EXT_PINNED")
grep -q 'cidr: 10.20.30.40/32' <<< "$RECORDER_EXT_PINNED" && rc=0 || rc=1
check "external Postgres + pinned cidrs: recorder egress uses the ipBlock" "$rc"
found=$(grep_c 'cnpg.io/cluster' "$RECORDER_EXT_PINNED")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "external Postgres + pinned cidrs: no CNPG pod selector" "$rc"

# ---------------------------------------------------------------------------
# (g) Hive mode: exactly four NetworkPolicy objects, none named recorder,
#     no Postgres rule anywhere. A member on s3 blobs writes to the core
#     object store and never reaches a database.
# ---------------------------------------------------------------------------
echo ""
echo "--- (g) Hive mode ---"

HIVE_RENDER=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=test \
  --set bus.url=tls://core.example:4222 \
  --set auth.oidc.clientSecret=test \
  --set auth.dashboardUrl=http://example.com \
  --set networkPolicy.enabled=true \
  --set recordStore.enabled=true \
  --set recordStore.postgres.mode=external \
  --set recordStore.blobs.mode=s3 \
  --set-string 'recordStore.blobs.s3.url=s3://b?endpoint=http://objects.example:9000' 2>/dev/null) || {
  echo "FAIL: helm template (hive) failed"; exit 1
}

netpol_count=$(grep_c "kind: NetworkPolicy" "$HIVE_RENDER")
if [[ "$netpol_count" -eq 4 ]]; then rc=0; else rc=1; fi
check "hive mode: exactly 4 NetworkPolicy objects" "$rc"

recorder_hive=$(grep_c "np-recorder" "$HIVE_RENDER")
if [[ "$recorder_hive" -eq 0 ]]; then rc=0; else rc=1; fi
check "hive mode: no np-recorder policy" "$rc"

# In s3 blob mode no hive workload reads a DSN: a secretKeyRef to a
# missing Secret blocks the pod.
recordstore_env_hive=$(grep_c "VARROA_RECORDSTORE_DSN" "$HIVE_RENDER")
if [[ "$recordstore_env_hive" -eq 0 ]]; then rc=0; else rc=1; fi
check "hive mode: no DSN env on any workload" "$rc"

NETPOLS_ONLY_HIVE=$(awk '/^kind: NetworkPolicy$/{c=1} c{print} /^---$/{c=0}' <<< "$HIVE_RENDER")
recordstore_rule_hive=$(grep_c "varroa-recordstore\|cnpg.io/cluster" "$NETPOLS_ONLY_HIVE")
if [[ "$recordstore_rule_hive" -eq 0 ]]; then rc=0; else rc=1; fi
check "hive mode: no record-store rule on any NetworkPolicy" "$rc"

recorder_source_hive=$(grep_c "app.kubernetes.io/component: varroa-recorder" "$NETPOLS_ONLY_HIVE")
if [[ "$recorder_source_hive" -eq 0 ]]; then rc=0; else rc=1; fi
check "hive mode: no recorder source on any policy" "$rc"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
# --- record-store Postgres pods (cnpg mode) ---------------------------------
echo "Record-store Postgres policy (cnpg mode):"
CNPG_RENDER="$(helm template varroa "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true --set recordStore.postgres.mode=cnpg 2>/dev/null)"
DB_POLICY="$(printf '%s\n' "$CNPG_RENDER" | awk '/name: varroa-np-recordstore-db/,/^---/')"
check "the cnpg instance pods get their own policy" "$([[ -n "$DB_POLICY" ]]; echo $?)"
check "  it selects cnpg.io/cluster: varroa-recordstore" "$(printf '%s\n' "$DB_POLICY" | grep -q 'cnpg.io/cluster: varroa-recordstore'; echo $?)"
check "  ingress 5432 from the recorder" "$(printf '%s\n' "$DB_POLICY" | grep -q 'varroa-recorder'; echo $?)"
check "  ingress 8000 from the CloudNativePG operator namespace" "$(printf '%s\n' "$DB_POLICY" | grep -q 'port: 8000' && printf '%s\n' "$DB_POLICY" | grep -q 'cnpg-system'; echo $?)"
check "  egress to the API server" "$(printf '%s\n' "$DB_POLICY" | grep -q 'port: 6443'; echo $?)"
EXT_RENDER="$(helm template varroa "$ROOT" $COMMON --set networkPolicy.enabled=true --set recordStore.enabled=true --set recordStore.postgres.mode=external --set recordStore.postgres.external.existingSecret=dsn --set 'networkPolicy.recordStorePostgresEgress.cidrs={10.0.0.5/32}' 2>/dev/null)"
check "external mode renders no cnpg pod policy" "$(printf '%s\n' "$EXT_RENDER" | grep -q 'np-recordstore-db'; [[ $? -ne 0 ]]; echo $?)"

if [[ $failures -ne 0 ]]; then echo "FAILURES: $failures"; exit 1; fi
