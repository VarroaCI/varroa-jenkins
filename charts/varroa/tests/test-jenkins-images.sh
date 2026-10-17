#!/usr/bin/env bash
# Render-assertion test for jenkins.image.repository and jenkins.agentImage.
#
# The operator builds the Jenkins controller image from
# VARROA_JENKINS_IMAGE_REPOSITORY, and the jnlp agent image from
# VARROA_JENKINS_AGENT_IMAGE. Both must follow the chart values, so an internal
# registry mirror reaches every controller and cell.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com)

fail() { echo "FAIL: $*" >&2; exit 1; }

# Prints the named env value on the operator Deployment, or "ABSENT".
operator_env() {
  yq eval-all "
    select(.kind==\"Deployment\" and (.metadata.name | test(\"varroa-operator$\")))
    | .spec.template.spec.containers[0].env
    | map(select(.name==\"$1\")) | .[0].value // \"ABSENT\"
  " -
}

# --- Defaults: the public images ---
out="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
repo="$(printf '%s\n' "$out" | operator_env VARROA_JENKINS_IMAGE_REPOSITORY)"
[ "$repo" = "jenkins/jenkins" ] \
  || fail "default repository: expected jenkins/jenkins, got: ${repo:-none}"
agent="$(printf '%s\n' "$out" | operator_env VARROA_JENKINS_AGENT_IMAGE)"
[ "$agent" = "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21" ] \
  || fail "default agent image: got: ${agent:-none}"

# --- Mirror: both images follow the chart values ---
out="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set jenkins.image.repository=registry.internal/mirror/jenkins \
  --set jenkins.agentImage=registry.internal/mirror/inbound-agent:3384-jdk21)"
repo="$(printf '%s\n' "$out" | operator_env VARROA_JENKINS_IMAGE_REPOSITORY)"
[ "$repo" = "registry.internal/mirror/jenkins" ] \
  || fail "mirror repository: got: ${repo:-none}"
agent="$(printf '%s\n' "$out" | operator_env VARROA_JENKINS_AGENT_IMAGE)"
[ "$agent" = "registry.internal/mirror/inbound-agent:3384-jdk21" ] \
  || fail "mirror agent image: got: ${agent:-none}"

echo "PASS: jenkins.image.repository and jenkins.agentImage render the public defaults and a mirror"
