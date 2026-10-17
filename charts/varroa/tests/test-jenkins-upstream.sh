#!/usr/bin/env bash
# Render-assertion test for jenkins.upstreamURL.
#
# The operator reads VARROA_JENKINS_UPSTREAM_URL. The chart value has two
# states that a helm render can express: the default host, and an explicit
# empty string that turns every upstream read off. The empty string must render
# as "", not as an absent variable, because the operator treats an absent
# variable as the built-in host.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

# Prints the VARROA_JENKINS_UPSTREAM_URL value of the Deployment whose name
# matches $1, or the literal "ABSENT" when the variable is not rendered.
upstream_url_of() {
  yq eval-all "
    select(.kind==\"Deployment\" and (.metadata.name | test(\"$1\")))
    | .spec.template.spec.containers[0].env
    | map(select(.name==\"VARROA_JENKINS_UPSTREAM_URL\")) | .[0].value // \"ABSENT\"
  " -
}

# The operator Deployment reads the value; the BFF Deployment reports the state.
upstream_url() { upstream_url_of 'varroa-operator$'; }
bff_upstream_url() { upstream_url_of 'varroa-bff$'; }

# --- Default: the operator reads the built-in updates.jenkins.io host ---
default_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" | upstream_url)"
[ "$default_url" = "https://updates.jenkins.io" ] \
  || fail "default: expected https://updates.jenkins.io, got: ${default_url:-none}"

# --- Explicit mirror: redirected verbatim ---
mirror_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set jenkins.upstreamURL=https://mirror.internal/jenkins | upstream_url)"
[ "$mirror_url" = "https://mirror.internal/jenkins" ] \
  || fail "mirror: expected the internal mirror, got: ${mirror_url:-none}"

# --- Explicit empty: rendered as empty, which the operator reads as off ---
off_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set jenkins.upstreamURL="" | upstream_url)"
[ "$off_url" = "" ] \
  || fail "explicit empty: expected an empty value (upstream reads off), got: '${off_url}'"

# --- The BFF gets the same value, so it reports the same upstream state ---
bff_default="$(helm template t "$CHART_DIR" "${COMMON[@]}" | bff_upstream_url)"
[ "$bff_default" = "https://updates.jenkins.io" ] \
  || fail "bff default: expected https://updates.jenkins.io, got: ${bff_default:-none}"

bff_mirror="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set jenkins.upstreamURL=https://mirror.internal/jenkins | bff_upstream_url)"
[ "$bff_mirror" = "https://mirror.internal/jenkins" ] \
  || fail "bff mirror: expected the internal mirror, got: ${bff_mirror:-none}"

bff_off="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set jenkins.upstreamURL="" | bff_upstream_url)"
[ "$bff_off" = "" ] \
  || fail "bff explicit empty: expected an empty value (upstream reads off), got: '${bff_off}'"

echo "PASS: jenkins.upstreamURL renders the default, a mirror, and an empty value into the operator and the BFF"
