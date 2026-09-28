#!/usr/bin/env bash
# =============================================================================
# Golden render test: a member (hive) release on the core record store, and
# the core member endpoint for the record-store database.
# =============================================================================
#   1. Hive + s3 blobs: the gateway and operator get the blob env, and no
#      recorder, database, retention, or record-store Secret renders.
#   2. Hive + record store: postgres.mode must be external.
#   3. Hive + postgres blobs: the gateway reads the DSN from existingSecret;
#      an empty existingSecret fails; with NetworkPolicy the gateway egress
#      needs pinned Postgres CIDRs.
#   4. Hive + s3 blobs + NetworkPolicy: the gateway egress carries the blob
#      rule, and the render stays at four policies.
#   5. Core member endpoint: Service shape, guards, database ingress rule,
#      and no Service in a hive release.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
failures=0

check() {
  if [[ "$2" == 0 ]]; then echo "  PASS: $1"; else echo "  FAIL: $1"; failures=$((failures + 1)); fi
}

doc() {
  # doc <kind> <name> <render>: print one rendered object.
  awk -v k="kind: $1" -v n="name: $2" '
    /^---/ { if (hit) print buf; buf=""; hit=0; kind=0; next }
    { buf = buf $0 "\n" }
    $0 == k { kind=1 }
    kind && $0 ~ "^  " n "$" { hit=1 }
    END { if (hit) print buf }' <<< "$3"
}

AUTH=(--set auth.oidc.clientSecret=s --set auth.dashboardUrl=https://example.com)
HIVE=(-f "$ROOT/values-hive.yaml" --set cluster.name=member --set bus.url=tls://core.example:4222 "${AUTH[@]}")
S3_URL='s3://varroa-blobs?region=us-east-1&endpoint=http://objects.example:9000'
MEMBER=(--set recordStore.enabled=true --set recordStore.postgres.mode=external
  --set recordStore.blobs.mode=s3 --set-string "recordStore.blobs.s3.url=$S3_URL"
  --set recordStore.blobs.s3.existingSecret=member-s3
  --set recordStore.blobs.s3.artifactSecret=member-artifacts)

echo "=== 1. Hive + s3 blobs ==="
R=$(helm template t "$ROOT" "${HIVE[@]}" "${MEMBER[@]}")
GW=$(doc Deployment t-varroa-gateway "$R")
OP=$(doc Deployment t-varroa-operator "$R")
grep -q "value: \"$S3_URL\"" <<< "$GW"; check "gateway carries VARROA_BLOBSTORE_URL" $?
grep -q "name: member-s3" <<< "$GW"; check "gateway reads the s3 credential Secret" $?
grep -q "VARROA_RECORDSTORE_DSN" <<< "$GW" && rc=1 || rc=0; check "gateway carries no DSN in s3 blob mode" $rc
grep -q "value: \"$S3_URL\"" <<< "$OP"; check "operator carries VARROA_BLOBSTORE_URL" $?
grep -A1 "name: VARROA_ARTIFACT_SECRET" <<< "$OP" | grep -q 'value: "member-artifacts"'; check "operator carries VARROA_ARTIFACT_SECRET" $?
grep -A1 "name: VARROA_CLAIM_GATE" <<< "$OP" | grep -q 'value: "1"'; check "operator claim gate is on" $?
grep -q "^# Source: varroa/templates/recorder/" <<< "$R" && rc=1 || rc=0; check "no recorder template renders" $rc
grep -q "kind: Cluster$" <<< "$R" && rc=1 || rc=0; check "no CNPG Cluster renders" $rc
grep -q "varroa-recordstore-" <<< "$R" && rc=1 || rc=0; check "no record-store role Secret or reference renders" $rc
grep -q "VARROA_RETENTION_" <<< "$R" && rc=1 || rc=0; check "no retention env renders" $rc
grep -q "name: varroa-recorder" <<< "$R" && rc=1 || rc=0; check "no recorder PDB or workload renders" $rc

echo "=== 2. Hive + record store requires external Postgres ==="
OUT=$(helm template t "$ROOT" "${HIVE[@]}" --set recordStore.enabled=true 2>&1 || true)
grep -q "requires recordStore.postgres.mode=external" <<< "$OUT"; check "hive + cnpg fails" $?

echo "=== 3. Hive + postgres blobs ==="
OUT=$(helm template t "$ROOT" "${HIVE[@]}" --set recordStore.enabled=true --set recordStore.postgres.mode=external 2>&1 || true)
grep -q "existingSecret is required" <<< "$OUT"; check "hive + postgres blobs without existingSecret fails" $?
R=$(helm template t "$ROOT" "${HIVE[@]}" --set recordStore.enabled=true --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=member-dsn)
GW=$(doc Deployment t-varroa-gateway "$R")
grep -A4 "name: VARROA_RECORDSTORE_DSN" <<< "$GW" | grep -q "name: member-dsn"; check "gateway reads the DSN from existingSecret" $?
grep -A1 "name: VARROA_BLOBSTORE_URL" <<< "$GW" | grep -q 'value: "pgblob:"'; check "gateway blob URL is pgblob:" $?
OUT=$(helm template t "$ROOT" "${HIVE[@]}" --set recordStore.enabled=true --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=member-dsn --set networkPolicy.enabled=true 2>&1 || true)
grep -q "recordStorePostgresEgress.cidrs must be set" <<< "$OUT"; check "hive + postgres blobs + NetworkPolicy needs pinned CIDRs" $?
R=$(helm template t "$ROOT" "${HIVE[@]}" --set recordStore.enabled=true --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=member-dsn --set networkPolicy.enabled=true \
  --set-string 'networkPolicy.recordStorePostgresEgress.cidrs={10.1.2.3/32}')
NPGW=$(doc NetworkPolicy t-np-gateway "$R")
grep -q "cidr: 10.1.2.3/32" <<< "$NPGW"; check "gateway egress admits the pinned Postgres CIDR" $?

echo "=== 4. Hive + s3 blobs + NetworkPolicy ==="
R=$(helm template t "$ROOT" "${HIVE[@]}" "${MEMBER[@]}" --set networkPolicy.enabled=true \
  --set-string 'networkPolicy.recordStoreBlobEgress.cidrs={10.9.9.9/32}' --set 'networkPolicy.recordStoreBlobEgress.ports={9000}')
NPGW=$(doc NetworkPolicy t-np-gateway "$R")
grep -q "cidr: 10.9.9.9/32" <<< "$NPGW"; check "gateway egress carries the blob rule" $?
[[ "$(grep -c '^kind: NetworkPolicy$' <<< "$R")" -eq 4 ]]; check "hive render stays at four policies" $?

echo "=== 5. Core member endpoint ==="
CORE=("${AUTH[@]}" --set recordStore.enabled=true --set recordStore.postgres.memberEndpoint.enabled=true)
R=$(helm template t "$ROOT" "${CORE[@]}" --set recordStore.postgres.memberEndpoint.host=pg.example \
  --set recordStore.postgres.memberEndpoint.serviceType=NodePort --set recordStore.postgres.memberEndpoint.nodePort=30543 \
  --set networkPolicy.enabled=true --set-string 'recordStore.postgres.memberEndpoint.allowedCIDRs={192.0.2.0/24}')
SVC=$(doc Service t-recordstore-member "$R")
grep -q "type: NodePort" <<< "$SVC"; check "member endpoint Service is a NodePort" $?
grep -q "nodePort: 30543" <<< "$SVC"; check "member endpoint Service pins the nodePort" $?
grep -q "cnpg.io/instanceRole: primary" <<< "$SVC"; check "member endpoint selects the primary" $?
NPDB=$(doc NetworkPolicy t-np-recordstore-db "$R")
grep -q "cidr: 192.0.2.0/24" <<< "$NPDB"; check "database admits the member CIDR on 5432" $?
OUT=$(helm template t "$ROOT" "${CORE[@]}" 2>&1 || true)
grep -q "memberEndpoint.enabled requires recordStore.postgres.memberEndpoint.host" <<< "$OUT"; check "member endpoint without host fails" $?
OUT=$(helm template t "$ROOT" "${CORE[@]}" --set recordStore.postgres.memberEndpoint.host=pg.example \
  --set networkPolicy.enabled=true 2>&1 || true)
grep -q "memberEndpoint.allowedCIDRs must list the source CIDRs" <<< "$OUT"; check "member endpoint + NetworkPolicy without allowedCIDRs fails" $?
OUT=$(helm template t "$ROOT" "${CORE[@]}" --set recordStore.postgres.memberEndpoint.host=pg.example \
  --set recordStore.postgres.mode=external --set recordStore.postgres.external.existingSecret=x 2>&1 || true)
grep -q "memberEndpoint requires recordStore.postgres.mode=cnpg" <<< "$OUT"; check "member endpoint with external Postgres fails" $?
R=$(helm template t "$ROOT" "${AUTH[@]}" --set recordStore.enabled=true)
grep -q "recordstore-member" <<< "$R" && rc=1 || rc=0; check "member endpoint is off by default" $rc
R=$(helm template t "$ROOT" "${HIVE[@]}" "${MEMBER[@]}" --set recordStore.postgres.memberEndpoint.enabled=true)
grep -q "recordstore-member" <<< "$R" && rc=1 || rc=0; check "a hive release ignores the member endpoint" $rc

echo ""
if [[ "$failures" -eq 0 ]]; then echo "=== ALL ASSERTIONS PASSED ==="; else echo "=== $failures FAILURE(S) ==="; exit 1; fi
