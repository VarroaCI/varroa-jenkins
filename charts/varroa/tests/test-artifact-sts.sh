#!/usr/bin/env bash
# =============================================================================
# Golden render test: recordStore.blobs.s3.artifactSTS
# =============================================================================
# Verifies the operator environment, the render guard, and the operator
# blob egress rule for operator-issued STS artifact credentials.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test-secret --set auth.dashboardUrl=https://varroa.test"
S3="--set recordStore.enabled=true --set recordStore.blobs.mode=s3 --set recordStore.blobs.s3.url=s3://b?region=us-east-1"
STS="$S3 --set recordStore.blobs.s3.artifactSTS.enabled=true --set recordStore.blobs.s3.artifactSTS.issuerSecret=varroa-artifact-issuer --set recordStore.blobs.s3.artifactSTS.roleArn=arn:minio:iam:::role/varroa-artifacts"

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

expect_fail() {
  local desc="$1"
  local extra="$2"
  local msg_pattern="$3"
  set +e
  local out
  out=$(helm template "$ROOT" $COMMON $extra 2>&1)
  local rc=$?
  set -e
  if [[ "$rc" -ne 0 ]]; then rc=0; else rc=1; fi
  check "$desc: render fails" "$rc"
  grep -q "$msg_pattern" <<< "$out" && rc=0 || rc=1
  check "$desc: error names the field" "$rc"
}

echo "=== artifactSTS assertions ==="

echo ""
echo "--- (a) STS mode renders the operator environment ---"
RENDER=$(helm template "$ROOT" $COMMON $STS)
for env in VARROA_ARTIFACT_STS_ISSUER_SECRET VARROA_ARTIFACT_STS_ROLE_ARN VARROA_ARTIFACT_STS_ENDPOINT VARROA_ARTIFACT_STS_DURATION; do
  grep -q "name: $env" <<< "$RENDER" && rc=0 || rc=1
  check "operator carries $env" "$rc"
done
grep -A1 "name: VARROA_ARTIFACT_STS_DURATION" <<< "$RENDER" | grep -q 'value: "1h"' && rc=0 || rc=1
check "duration defaults to 1h" "$rc"
grep -q "name: VARROA_ARTIFACT_SECRET$" <<< "$RENDER" && rc=1 || rc=0
check "STS mode sets no VARROA_ARTIFACT_SECRET" "$rc"

echo ""
echo "--- (b) static mode renders no STS environment ---"
STATIC=$(helm template "$ROOT" $COMMON $S3 --set recordStore.blobs.s3.artifactSecret=varroa-cell-artifacts)
grep -q "VARROA_ARTIFACT_STS_" <<< "$STATIC" && rc=1 || rc=0
check "static mode sets no VARROA_ARTIFACT_STS_*" "$rc"

echo ""
echo "--- (c) guard ---"
expect_fail "STS beside artifactSecret" "$STS --set recordStore.blobs.s3.artifactSecret=x" "are exclusive"
expect_fail "STS without issuerSecret" "$S3 --set recordStore.blobs.s3.artifactSTS.enabled=true --set recordStore.blobs.s3.artifactSTS.roleArn=arn:x" "artifactSTS.issuerSecret is required"
expect_fail "STS without roleArn" "$S3 --set recordStore.blobs.s3.artifactSTS.enabled=true --set recordStore.blobs.s3.artifactSTS.issuerSecret=i" "artifactSTS.roleArn is required"
expect_fail "STS with postgres blobs" "--set recordStore.enabled=true --set recordStore.blobs.s3.artifactSTS.enabled=true --set recordStore.blobs.s3.artifactSTS.issuerSecret=i --set recordStore.blobs.s3.artifactSTS.roleArn=arn:x" "requires recordStore.blobs.mode=s3"
expect_fail "duration above 12h" "$STS --set recordStore.blobs.s3.artifactSTS.duration=24h" "must be from 15m to 12h"
expect_fail "duration under 15m" "$STS --set recordStore.blobs.s3.artifactSTS.duration=10m" "must be from 15m to 12h"
expect_fail "duration in seconds" "$STS --set recordStore.blobs.s3.artifactSTS.duration=3600s" "whole number of minutes or hours"
for d in 15m 12h 90m; do
  helm template "$ROOT" $COMMON $STS --set recordStore.blobs.s3.artifactSTS.duration=$d > /dev/null 2>&1 && rc=0 || rc=1
  check "duration $d renders" "$rc"
done
expect_fail "endpoint without a scheme" "$STS --set recordStore.blobs.s3.artifactSTS.endpoint=minio:9000" "must be an http or https URL"
expect_fail "issuer is the recorder Secret" "$STS --set recordStore.blobs.s3.existingSecret=varroa-artifact-issuer" "must not be recordStore.blobs.s3.existingSecret"

echo ""
echo "--- (d) operator blob egress follows STS mode ---"
NP_STS=$(helm template "$ROOT" $COMMON $STS --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml)
NP_STATIC=$(helm template "$ROOT" $COMMON $S3 --set recordStore.blobs.s3.artifactSecret=x --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml)
operator_np() {
  awk '/^---/{p=0} /name: .*-np-operator$/{p=1} p' <<< "$1"
}
grep -q "artifactSTS: operator calls STS AssumeRole" <<< "$(operator_np "$NP_STS")" && rc=0 || rc=1
check "STS mode: operator policy carries the blob egress rule" "$rc"
grep -q "artifactSTS: operator calls STS AssumeRole" <<< "$(operator_np "$NP_STATIC")" && rc=1 || rc=0
check "static mode: operator policy has no STS egress rule" "$rc"

echo ""
if [[ "$failures" -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "All artifactSTS assertions passed."
