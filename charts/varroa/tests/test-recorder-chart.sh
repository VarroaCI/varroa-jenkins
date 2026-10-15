#!/usr/bin/env bash
# =============================================================================
# Golden render test: build-record store chart shape (D9)
# =============================================================================
# Verifies the recorder workload, the CNPG Cluster and role Secrets, the DSN
# and blob wiring, the retention defaults, the recorder PDB, the NATS
# credential gating, and every recordstore-guard.yaml failure mode.
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

# extract_doc prints the one rendered YAML document whose kind and
# metadata.name match, out of a multi-document helm template render.
# It uses exact line matches (a trailing newline in the search text), so
# "kind: Cluster" never matches a document of kind "ClusterRole".
extract_doc() {
  local kind="$1" name="$2" input="$3"
  awk -v kindline="kind: ${kind}" -v nameline="name: ${name}" '
    BEGIN { RS="\n---\n" }
    index($0, kindline "\n") && index($0, nameline "\n") { print; exit }
  ' <<< "$input"
}

# env_block prints one env var entry (its name line through the next env
# var's name line, or the end of the document) out of a document.
env_block() {
  local var="$1" doc="$2"
  awk -v var="- name: ${var}$" '
    $0 ~ var { f=1; print; next }
    f && /- name:/ { exit }
    f { print }
  ' <<< "$doc"
}

# deployment_name maps a component label to its rendered Deployment
# metadata.name: the recorder keeps a fixed name, while bff and gateway
# go through varroa.fullname and pick up the release-name prefix.
deployment_name() {
  case "$1" in
    varroa-recorder) echo "varroa-recorder" ;;
    varroa-bff) echo "release-name-varroa-bff" ;;
    varroa-gateway) echo "release-name-varroa-gateway" ;;
    varroa-operator) echo "release-name-varroa-operator" ;;
  esac
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
  if [[ -n "$msg_pattern" ]]; then
    grep -q "$msg_pattern" <<< "$out" && rc=0 || rc=1
    check "$desc: message mentions $msg_pattern" "$rc"
  fi
}

echo "=== Build-record store chart golden render assertions (D9) ==="

# ---------------------------------------------------------------------------
# (a) Full render, cnpg + postgres blob mode: recorder workload shape.
# ---------------------------------------------------------------------------
echo ""
echo "--- (a) Recorder Deployment, Service, ServiceAccount ---"

RENDER_FULL=$(helm template "$ROOT" $COMMON --set recordStore.enabled=true 2>/dev/null) || {
  echo "FAIL: helm template (full) failed"; exit 1
}

DEPLOY=$(extract_doc "Deployment" "varroa-recorder" "$RENDER_FULL")
if grep -q -A1 'name: VARROA_RETENTION_RESTART_SOURCE' <<< "$DEPLOY" && grep -A1 'name: VARROA_RETENTION_RESTART_SOURCE' <<< "$DEPLOY" | grep -q 'value: "7d"'; then rc=0; else rc=1; fi
check "recorder renders VARROA_RETENTION_RESTART_SOURCE as 7d" "$rc"
failureThreshold=$(grep -o 'failureThreshold: [0-9]*' <<< "$DEPLOY" | head -1 | grep -o '[0-9]*')
periodSeconds=$(grep -B1 "failureThreshold: $failureThreshold" <<< "$DEPLOY" | grep -o 'periodSeconds: [0-9]*' | head -1 | grep -o '[0-9]*')
if [[ -n "$failureThreshold" && -n "$periodSeconds" && $((failureThreshold * periodSeconds)) -eq 240 ]]; then rc=0; else rc=1; fi
check "recorder startup probe budget = 240s (failureThreshold x periodSeconds)" "$rc"

SERVICE=$(extract_doc "Service" "varroa-recorder" "$RENDER_FULL")
[[ -n "$SERVICE" ]] && rc=0 || rc=1
check "recorder Service exists" "$rc"

SA=$(extract_doc "ServiceAccount" "varroa-recorder" "$RENDER_FULL")
[[ -n "$SA" ]] && rc=0 || rc=1
check "recorder ServiceAccount exists" "$rc"

found=$(grep_c "varroa-recorder" "$(awk '/^kind: Role$/{c=1} /^kind: RoleBinding$/{c=1} c{print} /^---$/{c=0}' <<< "$RENDER_FULL")")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "recorder has no Role or RoleBinding" "$rc"

# ---------------------------------------------------------------------------
# (b) CNPG Cluster shape.
# ---------------------------------------------------------------------------
echo ""
echo "--- (b) CNPG Cluster ---"

CLUSTER=$(extract_doc "Cluster" "varroa-recordstore" "$RENDER_FULL")
grep -q "^apiVersion: postgresql.cnpg.io/v1$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster apiVersion is postgresql.cnpg.io/v1" "$rc"
[[ -n "$CLUSTER" ]] && rc=0 || rc=1
check "Cluster name is varroa-recordstore" "$rc"
grep -q "  instances:" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster carries spec.instances" "$rc"
grep -q "database: varroa$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster bootstrap.initdb database is varroa" "$rc"
grep -q "owner: varroa_recorder$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster bootstrap.initdb owner is varroa_recorder" "$rc"
grep -q "name: varroa_client$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster managed.roles[0].name is varroa_client" "$rc"
grep -q "name: varroa_gateway$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster managed.roles carries varroa_gateway" "$rc"
grep -q "name: varroa-recordstore-gateway$" <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster varroa_gateway role points at varroa-recordstore-gateway" "$rc"
grep -q 'helm.sh/resource-policy: keep' <<< "$CLUSTER" && rc=0 || rc=1
check "Cluster carries the keep annotation" "$rc"

# ---------------------------------------------------------------------------
# (c) Role Secrets.
# ---------------------------------------------------------------------------
echo ""
echo "--- (c) Role Secrets ---"

for role in recorder client gateway; do
  SECRET=$(extract_doc "Secret" "varroa-recordstore-${role}" "$RENDER_FULL")
  grep -q "^type: kubernetes.io/basic-auth$" <<< "$SECRET" && rc=0 || rc=1
  check "varroa-recordstore-${role} Secret type is kubernetes.io/basic-auth" "$rc"
  for key in username password dsn; do
    grep -q "  ${key}:" <<< "$SECRET" && rc=0 || rc=1
    check "varroa-recordstore-${role} Secret carries key ${key}" "$rc"
  done
  grep -q 'helm.sh/resource-policy: keep' <<< "$SECRET" && rc=0 || rc=1
  check "varroa-recordstore-${role} Secret carries the keep annotation" "$rc"
done

# ---------------------------------------------------------------------------
# (d) DSN cross-references: cnpg mode.
# ---------------------------------------------------------------------------
echo ""
echo "--- (d) DSN cross-references, cnpg mode ---"

for want in "varroa-recorder:varroa-recordstore-recorder" "varroa-bff:varroa-recordstore-client" "varroa-gateway:varroa-recordstore-gateway"; do
  component="${want%%:*}"
  secret="${want##*:}"
  BLOCK=$(extract_doc "Deployment" "$(deployment_name "$component")" "$RENDER_FULL")
  DSNENV=$(env_block "VARROA_RECORDSTORE_DSN" "$BLOCK")
  grep -q "name: ${secret}$" <<< "$DSNENV" && rc=0 || rc=1
  check "${component} VARROA_RECORDSTORE_DSN names ${secret}, which this render creates" "$rc"
  found=$([[ -n "$(extract_doc "Secret" "$secret" "$RENDER_FULL")" ]] && echo 1 || echo 0)
  if [[ "$found" -eq 1 ]]; then rc=0; else rc=1; fi
  check "${secret} Secret is present in the same render" "$rc"
done

# ---------------------------------------------------------------------------
# (e) DSN cross-references: external mode.
# ---------------------------------------------------------------------------
echo ""
echo "--- (e) DSN cross-references, external mode ---"

RENDER_EXTERNAL=$(helm template "$ROOT" $COMMON \
  --set recordStore.enabled=true \
  --set recordStore.postgres.mode=external \
  --set recordStore.postgres.external.existingSecret=my-external-secret \
  --set recordStore.postgres.external.dsnKey=my-dns-key \
  --set networkPolicy.enabled=true \
  --set-string 'networkPolicy.recordStorePostgresEgress.cidrs={10.0.0.0/8}' 2>/dev/null) || {
  echo "FAIL: helm template (external) failed"; exit 1
}
for component in varroa-recorder varroa-bff varroa-gateway; do
  BLOCK=$(extract_doc "Deployment" "$(deployment_name "$component")" "$RENDER_EXTERNAL")
  DSNENV=$(env_block "VARROA_RECORDSTORE_DSN" "$BLOCK")
  grep -q "name: my-external-secret$" <<< "$DSNENV" && rc=0 || rc=1
  check "${component} VARROA_RECORDSTORE_DSN names external existingSecret" "$rc"
  grep -q "key: my-dns-key$" <<< "$DSNENV" && rc=0 || rc=1
  check "${component} VARROA_RECORDSTORE_DSN uses external dsnKey" "$rc"
done
found=$(grep_c "^kind: Cluster$" "$RENDER_EXTERNAL")
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "external mode: no CNPG Cluster rendered" "$rc"
found=$([[ -n "$(extract_doc "Secret" "varroa-recordstore-gateway" "$RENDER_EXTERNAL")" ]] && echo 1 || echo 0)
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "external mode: no varroa_gateway role Secret rendered" "$rc"

# ---------------------------------------------------------------------------
# (f) Retention env defaults.
# ---------------------------------------------------------------------------
echo ""
echo "--- (f) Retention env defaults ---"

RECORDER_DEPLOY=$(extract_doc "Deployment" "varroa-recorder" "$RENDER_FULL")
grep -A1 '\- name: VARROA_RETENTION_RUNS' <<< "$RECORDER_DEPLOY" | grep -q 'value: "365d"' && rc=0 || rc=1
check "VARROA_RETENTION_RUNS default is 365d" "$rc"
grep -A1 '\- name: VARROA_RETENTION_LOGS' <<< "$RECORDER_DEPLOY" | grep -q 'value: "90d"' && rc=0 || rc=1
check "VARROA_RETENTION_LOGS default is 90d" "$rc"
grep -A1 '\- name: VARROA_RETENTION_ARTIFACTS' <<< "$RECORDER_DEPLOY" | grep -q 'value: "30d"' && rc=0 || rc=1
check "VARROA_RETENTION_ARTIFACTS default is 30d" "$rc"
grep -A1 '\- name: VARROA_RETENTION_ACTIVITY' <<< "$RECORDER_DEPLOY" | grep -q 'value: "30d"' && rc=0 || rc=1
check "VARROA_RETENTION_ACTIVITY default is 30d" "$rc"
grep -A1 '\- name: VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE' <<< "$RECORDER_DEPLOY" | grep -q 'value: "30s"' && rc=0 || rc=1
check "VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE default is 30s" "$rc"

# ---------------------------------------------------------------------------
# (f2) OTel metrics bridge wiring (issue #666): the recorder must carry the
# same OTEL_* env set as operator/gateway/bff, so its counters reach
# /metrics through the shared telemetry.InitTelemetry path.
# ---------------------------------------------------------------------------
echo ""
echo "--- (f2) OTel metrics bridge wiring ---"

grep -A1 '\- name: OTEL_METRICS_ENABLED' <<< "$RECORDER_DEPLOY" | grep -q 'value: "true"' && rc=0 || rc=1
check "recorder OTEL_METRICS_ENABLED default is true" "$rc"
grep -A1 '\- name: OTEL_TRACES_ENABLED' <<< "$RECORDER_DEPLOY" | grep -q 'value: "true"' && rc=0 || rc=1
check "recorder OTEL_TRACES_ENABLED default is true" "$rc"
grep -A1 '\- name: OTEL_LOGS_ENABLED' <<< "$RECORDER_DEPLOY" | grep -q 'value: "true"' && rc=0 || rc=1
check "recorder OTEL_LOGS_ENABLED default is true" "$rc"
grep -q '\- name: OTEL_EXPORTER_OTLP_ENDPOINT' <<< "$RECORDER_DEPLOY" && rc=0 || rc=1
check "recorder carries OTEL_EXPORTER_OTLP_ENDPOINT" "$rc"

RENDER_METRICS_OFF=$(helm template "$ROOT" $COMMON \
  --set recordStore.enabled=true \
  --set telemetry.services.recorder.metrics.enabled=false 2>/dev/null) || {
  echo "FAIL: helm template (metrics off) failed"; failures=$((failures + 1)); RENDER_METRICS_OFF=""
}
RECORDER_DEPLOY_METRICS_OFF=$(extract_doc "Deployment" "varroa-recorder" "$RENDER_METRICS_OFF")
grep -A1 '\- name: OTEL_METRICS_ENABLED' <<< "$RECORDER_DEPLOY_METRICS_OFF" | grep -q 'value: "false"' && rc=0 || rc=1
check "telemetry.services.recorder.metrics.enabled=false turns off OTEL_METRICS_ENABLED" "$rc"

# ---------------------------------------------------------------------------
# (g) S3 credential mapping on all three workloads.
# ---------------------------------------------------------------------------
echo ""
echo "--- (g) S3 credential mapping ---"

RENDER_S3=$(helm template "$ROOT" $COMMON \
  --set recordStore.enabled=true \
  --set recordStore.blobs.mode=s3 \
  --set recordStore.blobs.s3.url=s3://bucket \
  --set recordStore.blobs.s3.existingSecret=my-s3-secret 2>/dev/null) || {
  echo "FAIL: helm template (s3) failed"; exit 1
}
for component in varroa-recorder varroa-bff varroa-gateway; do
  BLOCK=$(extract_doc "Deployment" "$(deployment_name "$component")" "$RENDER_S3")
  for var in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN; do
    ENVVAR=$(env_block "$var" "$BLOCK")
    [[ -n "$ENVVAR" ]] && rc=0 || rc=1
    check "${component} maps ${var} from the S3 existingSecret" "$rc"
    grep -q "name: my-s3-secret$" <<< "$ENVVAR" && rc=0 || rc=1
    check "${component} ${var} reads my-s3-secret" "$rc"
  done
done

# ---------------------------------------------------------------------------
# (g2) Operator VARROA_BLOBSTORE_URL and VARROA_ARTIFACT_SECRET.
# ---------------------------------------------------------------------------
echo ""
echo "--- (g2) Operator blobstore URL and artifact Secret ---"

OPERATOR_S3=$(extract_doc "Deployment" "$(deployment_name "varroa-operator")" "$RENDER_S3")
GATEWAY_S3=$(extract_doc "Deployment" "$(deployment_name "varroa-gateway")" "$RENDER_S3")
OPERATOR_URL=$(env_block "VARROA_BLOBSTORE_URL" "$OPERATOR_S3")
GATEWAY_URL=$(env_block "VARROA_BLOBSTORE_URL" "$GATEWAY_S3")
[[ -n "$OPERATOR_URL" ]] && rc=0 || rc=1
check "operator carries VARROA_BLOBSTORE_URL" "$rc"
[[ "$OPERATOR_URL" == "$GATEWAY_URL" ]] && rc=0 || rc=1
check "operator VARROA_BLOBSTORE_URL matches the gateway's value" "$rc"

RENDER_ARTIFACT_SECRET=$(helm template "$ROOT" $COMMON \
  --set recordStore.enabled=true \
  --set recordStore.blobs.mode=s3 \
  --set recordStore.blobs.s3.url=s3://bucket \
  --set recordStore.blobs.s3.artifactSecret=my-artifact-base 2>/dev/null) || {
  echo "FAIL: helm template (artifactSecret) failed"; exit 1
}
OPERATOR_ARTIFACT=$(extract_doc "Deployment" "$(deployment_name "varroa-operator")" "$RENDER_ARTIFACT_SECRET")
ARTIFACTENV=$(env_block "VARROA_ARTIFACT_SECRET" "$OPERATOR_ARTIFACT")
grep -q 'value: "my-artifact-base"' <<< "$ARTIFACTENV" && rc=0 || rc=1
check "operator VARROA_ARTIFACT_SECRET is set from recordStore.blobs.s3.artifactSecret" "$rc"

OPERATOR_NO_ARTIFACT=$(extract_doc "Deployment" "$(deployment_name "varroa-operator")" "$RENDER_S3")
found=$([[ -n "$(env_block "VARROA_ARTIFACT_SECRET" "$OPERATOR_NO_ARTIFACT")" ]] && echo 1 || echo 0)
if [[ "$found" -eq 0 ]]; then rc=0; else rc=1; fi
check "empty recordStore.blobs.s3.artifactSecret: operator carries no VARROA_ARTIFACT_SECRET" "$rc"

# ---------------------------------------------------------------------------
# (h) Recorder PDB only at recorder.replicas=2.
# ---------------------------------------------------------------------------
echo ""
echo "--- (h) Recorder PodDisruptionBudget ---"

PDB=$(extract_doc "PodDisruptionBudget" "varroa-recorder" "$RENDER_FULL")
[[ -z "$PDB" ]] && rc=0 || rc=1
check "recorder.replicas=1 (default): no recorder PDB" "$rc"

RENDER_TWO_REPLICAS=$(helm template "$ROOT" $COMMON --set recordStore.enabled=true --set recorder.replicas=2 2>/dev/null) || {
  echo "FAIL: helm template (recorder.replicas=2) failed"; exit 1
}
PDB_TWO=$(extract_doc "PodDisruptionBudget" "varroa-recorder" "$RENDER_TWO_REPLICAS")
[[ -n "$PDB_TWO" ]] && rc=0 || rc=1
check "recorder.replicas=2: recorder PDB present" "$rc"

# ---------------------------------------------------------------------------
# (i) recorder-password key gated on recordStore.enabled.
# ---------------------------------------------------------------------------
echo ""
echo "--- (i) NATS recorder-password gating ---"

RENDER_DISABLED=$(helm template "$ROOT" $COMMON 2>/dev/null) || {
  echo "FAIL: helm template (disabled) failed"; exit 1
}
CREDS_DISABLED=$(extract_doc "Secret" "release-name-nats-creds" "$RENDER_DISABLED")
grep -q "recorder-password:" <<< "$CREDS_DISABLED" && rc=1 || rc=0
check "recordStore disabled: secret-nats-creds has no recorder-password key" "$rc"

CREDS_ENABLED=$(extract_doc "Secret" "release-name-nats-creds" "$RENDER_FULL")
grep -q "recorder-password:" <<< "$CREDS_ENABLED" && rc=0 || rc=1
check "recordStore enabled: secret-nats-creds carries the recorder-password key" "$rc"

# ---------------------------------------------------------------------------
# (j) recordstore-guard.yaml failure modes.
# ---------------------------------------------------------------------------
echo ""
echo "--- (j) recordstore-guard.yaml failure modes ---"

expect_fail "blobs.mode=file" "--set recordStore.enabled=true --set recordStore.blobs.mode=file" "recordStore.blobs.mode must be one of"
expect_fail "blobs.mode=s3 with empty url" "--set recordStore.enabled=true --set recordStore.blobs.mode=s3" "recordStore.blobs.s3.url is required"
expect_fail "postgres.mode=bogus" "--set recordStore.enabled=true --set recordStore.postgres.mode=bogus" "recordStore.postgres.mode must be one of"
expect_fail "postgres.mode=external with empty existingSecret" "--set recordStore.enabled=true --set recordStore.postgres.mode=external" "recordStore.postgres.external.existingSecret is required"
expect_fail "retention.runs=0d" "--set recordStore.enabled=true --set recordStore.retention.runs=0d" "recordStore.retention.runs must match"
expect_fail "retention.runs=3651d" "--set recordStore.enabled=true --set recordStore.retention.runs=3651d" "recordStore.retention.runs must be at most 3650 days"
expect_fail "retention.runs=999999999999999999999d" "--set recordStore.enabled=true --set recordStore.retention.runs=999999999999999999999d" "recordStore.retention.runs must match"
expect_fail "retention.activity=0d" "--set recordStore.enabled=true --set recordStore.retention.activity=0d" "recordStore.retention.activity must match"
expect_fail "retention.restartSource=0d" "--set recordStore.enabled=true --set recordStore.retention.restartSource=0d" "recordStore.retention.restartSource must match"
expect_fail "restartSource longer than runs" "--set recordStore.enabled=true --set recordStore.retention.runs=5d --set recordStore.retention.restartSource=7d" "must not be longer than recordStore.retention.runs"
expect_fail "the retired activity values block" "--set activity.retention=7d" "the \"activity\" values block no longer exists"

# The guard also runs in hive mode: a stray blob mode still fails there.
set +e
OUT=$(helm template "$ROOT" \
  -f "$ROOT/values-hive.yaml" \
  --set cluster.name=x \
  --set bus.url=nats://h:4222 \
  $COMMON \
  --set recordStore.enabled=true \
  --set recordStore.postgres.mode=external \
  --set recordStore.blobs.mode=file 2>&1)
rc=$?
set -e
if [[ "$rc" -ne 0 ]] && grep -q "recordStore.blobs.mode must be one of" <<< "$OUT"; then rc=0; else rc=1; fi
check "hive mode with recordStore.enabled=true and blobs.mode=file: render fails (guard runs)" "$rc"

# ---------------------------------------------------------------------------
# (k) Task 10.2: hardened contexts, placement, whole-volume creds, no
#     password env; task 10.4 external mode: empty dsnKey fails.
# ---------------------------------------------------------------------------
echo ""
echo "--- (k) Recorder Deployment hardening and credential mount ---"
RECORDER_DEPLOY=$(extract_doc Deployment varroa-recorder "$RENDER_FULL")
[[ $(grep_c "runAsNonRoot: true" "$RECORDER_DEPLOY") -ge 1 ]] && rc=0 || rc=1
check "recorder pod runs as non-root" "$rc"
[[ $(grep_c "readOnlyRootFilesystem: true" "$RECORDER_DEPLOY") -ge 1 ]] && rc=0 || rc=1
check "recorder container has a read-only root filesystem" "$rc"
[[ $(grep_c "allowPrivilegeEscalation: false" "$RECORDER_DEPLOY") -ge 1 ]] && rc=0 || rc=1
check "recorder container denies privilege escalation" "$rc"
[[ $(grep_c "mountPath: /etc/nats-creds" "$RECORDER_DEPLOY") -ge 1 ]] && rc=0 || rc=1
check "recorder mounts the NATS credentials volume" "$rc"
[[ $(grep_c "bus-pass-file=/etc/nats-creds/recorder-password" "$RECORDER_DEPLOY") -eq 1 ]] && rc=0 || rc=1
check "recorder reads its NATS password from the mounted file" "$rc"
[[ $(grep_c "name: BUS_PASSWORD" "$RECORDER_DEPLOY") -eq 0 ]] && rc=0 || rc=1
check "recorder has no NATS password environment variable" "$rc"
RENDER_PLACED=$(helm template "$ROOT" $COMMON --set recordStore.enabled=true --set recorder.nodeSelector.disk=ssd 2>/dev/null) || {
  echo "FAIL: helm template (placement) failed"; failures=$((failures + 1)); RENDER_PLACED=""
}
PLACED_DEPLOY=$(extract_doc Deployment varroa-recorder "$RENDER_PLACED")
[[ $(grep_c "disk: ssd" "$PLACED_DEPLOY") -eq 1 ]] && rc=0 || rc=1
check "recorder.nodeSelector reaches the recorder pod through varroa.podPlacement" "$rc"
expect_fail "external mode with empty dsnKey" \
  "--set recordStore.enabled=true --set recordStore.postgres.mode=external --set recordStore.postgres.external.existingSecret=s --set recordStore.postgres.external.dsnKey=" \
  "dsnKey is required"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
