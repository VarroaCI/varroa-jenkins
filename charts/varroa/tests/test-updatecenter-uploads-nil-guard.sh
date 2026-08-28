#!/usr/bin/env bash
# Render-assertion test for the updateCenter.uploads nil-guard (issue #434).
#
# `helm upgrade --reuse-values` can produce a values tree where an optional,
# previously-unset nested block like `updateCenter.uploads` is nil (an
# explicit `--set updateCenter.uploads=null` reproduces the same shape
# locally, since a from-scratch `helm template` always coalesces the
# chart's own values.yaml defaults back in for a key that is merely
# *absent*). The deployment template must render VARROA_UC_SINGLE_WRITER
# and the replica/rollout topology from the documented default
# (uploads.enabled: true) instead of erroring on a nil map, and must not
# let that fallback clobber an explicit `enabled: false`.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com
        --set updateCenter.enabled=true)

fail() { echo "FAIL: $*" >&2; exit 1; }

uc_single_writer() {
  yq eval-all 'select(.kind=="Deployment" and .metadata.name=="varroa-updatecenter") | .spec.template.spec.containers[0].env[] | select(.name=="VARROA_UC_SINGLE_WRITER") | .value' -
}

uc_replicas_and_strategy() {
  yq eval-all 'select(.kind=="Deployment" and .metadata.name=="varroa-updatecenter") | (.spec.replicas | tostring) + " " + (.spec.strategy.type // "RollingUpdate")' -
}

# --- Default: uploads.enabled defaults true, single-writer topology ---
default_writer="$(helm template t "$CHART_DIR" "${COMMON[@]}" | uc_single_writer)"
[ "$default_writer" = "true" ] \
  || fail "default: expected VARROA_UC_SINGLE_WRITER=true, got: ${default_writer:-none}"
default_topo="$(helm template t "$CHART_DIR" "${COMMON[@]}" | uc_replicas_and_strategy)"
[ "$default_topo" = "1 Recreate" ] \
  || fail "default: expected replicas=1/Recreate, got: ${default_topo:-none}"

# --- Nil uploads block (the reuse-values reproduction): must not error, ---
# --- and must fall back to the documented enabled:true default.         ---
nil_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set 'updateCenter.uploads=null' 2>&1)" \
  || fail "nil uploads: helm template errored instead of falling back to the default (issue #434):\n$nil_render"
nil_writer="$(echo "$nil_render" | uc_single_writer)"
[ "$nil_writer" = "true" ] \
  || fail "nil uploads: expected VARROA_UC_SINGLE_WRITER to fall back to true, got: ${nil_writer:-none}"
nil_topo="$(echo "$nil_render" | uc_replicas_and_strategy)"
[ "$nil_topo" = "1 Recreate" ] \
  || fail "nil uploads: expected replicas=1/Recreate fallback, got: ${nil_topo:-none}"

# --- Explicit enabled: false must NOT be clobbered back to true by the ---
# --- nil-fallback (the sprig `default` gotcha for booleans).           ---
false_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=ghcr.io/x/y:z \
  --set-json 'updateCenter.uploads={"enabled":false}')"
false_writer="$(echo "$false_render" | uc_single_writer)"
[ "$false_writer" = "false" ] \
  || fail "explicit false: expected VARROA_UC_SINGLE_WRITER=false to be preserved, got: ${false_writer:-none}"
false_topo="$(echo "$false_render" | uc_replicas_and_strategy)"
[ "$false_topo" = "2 RollingUpdate" ] \
  || fail "explicit false: expected replicas=2/RollingUpdate (multi-writer OCI mode), got: ${false_topo:-none}"

echo "PASS: updateCenter.uploads nil-guard falls back to the default without clobbering an explicit false"
