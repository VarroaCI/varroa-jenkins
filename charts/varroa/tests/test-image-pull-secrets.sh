#!/usr/bin/env bash
# =============================================================================
# Golden render test: one pull Secret list for every pod of the release
# =============================================================================
# Verifies that:
#   - global.image.pullSecretNames reaches every Deployment and StatefulSet,
#     the NATS StatefulSet and the nats-box Deployment included
#   - the operator gets the same names in VARROA_IMAGE_PULL_SECRETS
#   - an empty list renders no imagePullSecrets and no env var
#   - the retired key global.imagePullSecrets fails the render
#   - an entry that is not a Secret name fails the render
# =============================================================================
# No errexit: each assertion reads $? through check().
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE=(--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test
  --set recordStore.enabled=true --set updateCenter.enabled=true)

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

# workloads prints "<kind>/<name> <comma-separated pull Secret names>" for
# each Deployment and StatefulSet of a render.
workloads() {
  yq -N 'select(.kind == "Deployment" or .kind == "StatefulSet")
    | .kind + "/" + .metadata.name + " " + (.spec.template.spec.imagePullSecrets // [] | map(.name) | join(","))' \
    | grep -v '^$'
}

echo "=== 1. The air-gapped values reach every pod ==="
RENDER=$(helm template varroa "$ROOT" "${BASE[@]}" \
  --set 'global.image.pullSecretNames={registry-credentials,mirror-credentials}' 2>/dev/null) || {
  echo "FAIL: helm template failed"
  exit 1
}
ROWS=$(workloads <<<"$RENDER")
echo "$ROWS" | sed 's/^/    /'

for w in StatefulSet/varroa-nats Deployment/varroa-nats-box Deployment/varroa-varroa-operator \
  Deployment/varroa-varroa-gateway Deployment/varroa-varroa-bff Deployment/varroa-frontend \
  Deployment/varroa-dex Deployment/varroa-recorder Deployment/varroa-updatecenter; do
  grep -qx "$w registry-credentials,mirror-credentials" <<<"$ROWS"
  check "$w carries both pull Secrets" $?
done

! grep -qv ' registry-credentials,mirror-credentials$' <<<"$ROWS"
check "no workload omits the pull Secrets" $?

OPERATOR_ENV=$(yq -N 'select(.kind == "Deployment" and .metadata.name == "varroa-varroa-operator")
  | .spec.template.spec.containers[0].env[] | select(.name == "VARROA_IMAGE_PULL_SECRETS") | .value' <<<"$RENDER")
[[ "$OPERATOR_ENV" == "registry-credentials,mirror-credentials" ]]
check "operator VARROA_IMAGE_PULL_SECRETS names both Secrets (got '$OPERATOR_ENV')" $?

echo "=== 2. An empty list renders no pull Secrets ==="
DEFAULT=$(helm template varroa "$ROOT" "${BASE[@]}" 2>/dev/null) || {
  echo "FAIL: default helm template failed"
  exit 1
}
! grep -q 'imagePullSecrets:' <<<"$DEFAULT"
check "default render has no imagePullSecrets" $?
! grep -q 'VARROA_IMAGE_PULL_SECRETS' <<<"$DEFAULT"
check "default render has no VARROA_IMAGE_PULL_SECRETS" $?

echo "=== 3. The retired key fails the render ==="
if OUT=$(helm template varroa "$ROOT" "${BASE[@]}" \
  --set 'global.imagePullSecrets[0].name=registry-credentials' 2>&1); then
  check "global.imagePullSecrets fails (render must fail)" 1
else
  grep -qF "set global.image.pullSecretNames" <<<"$OUT"
  check "global.imagePullSecrets fails and names the new key" $?
fi

if OUT=$(helm template varroa "$ROOT" "${BASE[@]}" \
  --set 'global.image.pullSecretNames[0].name=registry-credentials' 2>&1); then
  check "a {name: x} entry fails (render must fail)" 1
else
  grep -qF "must hold Secret names as strings" <<<"$OUT"
  check "a {name: x} entry fails and names the shape" $?
fi

# The NATS subchart can fail first on a scalar, so assert only the failure.
helm template varroa "$ROOT" "${BASE[@]}" \
  --set-string global.image.pullSecretNames=registry-credentials >/dev/null 2>&1
[[ $? -ne 0 ]]
check "a scalar pullSecretNames fails the render" $?

! grep -rq 'global\.imagePullSecrets' "$ROOT/templates" --include='*deployment.yaml'
check "no Deployment template reads the retired key" $?

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
