#!/usr/bin/env bash
# Render-assertion test for the update-center seed contract (seed is opt-in).
#
# updateCenter.seed.refs defaults to an empty list and seed.secretRef to "", so
# an enabled update center must render with NO `seed:` block until the operator
# sets one of them. This asserts the default render stays seed-free, that an
# explicit override is the sole source of seed refs, and that secretRef renders
# independently of refs — the `seed:` block used to be guarded on refs alone,
# which silently dropped a secretRef-only configuration into an anonymous pull.
#
# It also pins the rest of the update-center chart contract that no other script
# covers: the storage/uploads replica topology, the disabled-render surface, and
# the pull-through defaults.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com
        --set updateCenter.enabled=true)

fail() { echo "FAIL: $*" >&2; exit 1; }

uc_seed_refs() {
  yq eval-all 'select(.kind=="UpdateCenter") | .spec.seed.refs[]' - 2>/dev/null || true
}

uc_seed_secret_ref() {
  yq eval-all 'select(.kind=="UpdateCenter") | .spec.seed.secretRef' - 2>/dev/null || true
}

uc_replicas_and_strategy() {
  yq eval-all 'select(.kind=="Deployment" and .metadata.name=="varroa-updatecenter") | (.spec.replicas | tostring) + " " + (.spec.strategy.type // "RollingUpdate")' -
}

# --- Default: an enabled update center renders with NO seed block ---
default_out="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
echo "$default_out" | yq eval-all 'select(.kind=="UpdateCenter") | .metadata.name' - | grep -q . \
  || fail "default: UpdateCenter CR did not render when updateCenter.enabled=true"
default_seed="$(echo "$default_out" | yq eval-all 'select(.kind=="UpdateCenter") | .spec.seed' -)"
[ "$default_seed" = "null" ] \
  || fail "default: expected no seed block, got: ${default_seed:-none}"

# --- Override: an explicit list is the sole source of seed refs ---
override_refs="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set 'updateCenter.seed.refs={registry.example.org/plugins/pack:v1}' | uc_seed_refs)"
echo "$override_refs" | grep -qxF "registry.example.org/plugins/pack:v1" \
  || fail "override: explicit seed ref missing (got: ${override_refs:-none})"
[ "$(echo "$override_refs" | grep -c .)" -eq 1 ] \
  || fail "override: expected exactly one seed ref, got: $override_refs"

# --- secretRef only: the seed block must still render (issue #494) ---
secret_only="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.seed.secretRef=seed-creds)"
[ "$(echo "$secret_only" | uc_seed_secret_ref)" = "seed-creds" ] \
  || fail "secretRef-only: spec.seed.secretRef missing; the seed block is still guarded on refs"
[ -z "$(echo "$secret_only" | uc_seed_refs)" ] \
  || fail "secretRef-only: expected no seed refs, got: $(echo "$secret_only" | uc_seed_refs)"

# --- refs only: no secretRef key is emitted ---
refs_only="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set 'updateCenter.seed.refs={registry.example.org/plugins/pack:v1}')"
[ "$(echo "$refs_only" | uc_seed_secret_ref)" = "null" ] \
  || fail "refs-only: expected no secretRef, got: $(echo "$refs_only" | uc_seed_secret_ref)"

# --- both: refs and secretRef render together ---
both="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set 'updateCenter.seed.refs={registry.example.org/plugins/pack:v1}' \
  --set updateCenter.seed.secretRef=seed-creds)"
[ "$(echo "$both" | uc_seed_secret_ref)" = "seed-creds" ] \
  || fail "both: spec.seed.secretRef missing"
echo "$both" | uc_seed_refs | grep -qxF "registry.example.org/plugins/pack:v1" \
  || fail "both: spec.seed.refs missing"

# --- Local storage forces a single writer regardless of replicas ---
local_topo="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.storage.type=local \
  --set updateCenter.replicas=3 | uc_replicas_and_strategy)"
[ "$local_topo" = "1 Recreate" ] \
  || fail "local storage: expected replicas=1/Recreate even with replicas=3, got: ${local_topo:-none}"

# --- OCI storage honors replicas, but only with uploads disabled: uploads ---
# --- default to true and force a single writer regardless of storage.type ---
oci_out="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=ghcr.io/example/uc:latest \
  --set updateCenter.uploads.enabled=false \
  --set updateCenter.replicas=3)"
oci_topo="$(echo "$oci_out" | uc_replicas_and_strategy)"
[ "$oci_topo" = "3 RollingUpdate" ] \
  || fail "oci storage: expected replicas=3/RollingUpdate, got: ${oci_topo:-none}"
if echo "$oci_out" | yq eval-all 'select(.kind=="PersistentVolumeClaim")' - | grep -q .; then
  fail "oci storage: no PVC should render when storage.type=oci"
fi

# --- Uploads alone impose the single-writer topology, on OCI storage too ---
uploads_topo="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.storage.type=oci \
  --set updateCenter.storage.oci.ref=ghcr.io/example/uc:latest \
  --set updateCenter.replicas=3 | uc_replicas_and_strategy)"
[ "$uploads_topo" = "1 Recreate" ] \
  || fail "uploads enabled: expected replicas=1/Recreate on OCI storage, got: ${uploads_topo:-none}"

# --- Pull-through defaults: disabled, with upstream URLs configured ---
default_pt="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  | yq eval-all 'select(.kind=="UpdateCenter") | .spec.pullThrough' -)"
[ "$default_pt" = "null" ] \
  || fail "default: expected pullThrough.enabled false (no pullThrough block), got: ${default_pt:-none}"
pt_urls="$(yq eval '.updateCenter.pullThrough.upstreamURL + " " + .updateCenter.pullThrough.downloadURL' "$CHART_DIR/values.yaml")"
[ "$pt_urls" = "https://updates.jenkins.io https://updates.jenkins.io/download" ] \
  || fail "default: unexpected pull-through URLs: $pt_urls"

# --- Disabled: no update-center object at all, and no consumer wiring ---
disabled_out="$(helm template t "$CHART_DIR" --set auth.oidc.clientSecret=x --set auth.oidc.clientId=x \
     --set global.domain=example.com --set bff.oidcClientSecret=x \
     --set auth.dashboardUrl=https://example.com)"
if echo "$disabled_out" | yq eval-all 'select(.kind=="UpdateCenter")' - | grep -q .; then
  fail "disabled: no UpdateCenter CR should render when updateCenter.enabled is false"
fi
if echo "$disabled_out" | yq eval-all 'select(.metadata.name=="varroa-updatecenter") | .kind' - | grep -q .; then
  fail "disabled: no varroa-updatecenter object should render when updateCenter.enabled is false"
fi
if echo "$disabled_out" | grep -q "VARROA_UPDATE_CENTER_URL"; then
  fail "disabled: operator/BFF must not be wired to an update center that does not render"
fi

echo "PASS: update-center seed default renders, secretRef is independent of refs, and the topology/disabled contracts hold"
