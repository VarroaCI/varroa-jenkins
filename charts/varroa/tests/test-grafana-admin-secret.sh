#!/usr/bin/env bash
# Render-assertion test for Grafana admin credential generation (#414).
# Verifies the grafana-admin Secret is rendered, the Deployment injects
# credentials via __FILE, and the default admin/admin is replaced by a
# random generated password.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
fail() { echo "FAIL: $*" >&2; exit 1; }

render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"

# A grafana-admin Secret must exist with a non-static password.
pw="$(echo "$render" | yq eval-all 'select(.kind=="Secret" and (.metadata.name|test("grafana-admin"))) | .stringData["admin-password"]' -)"
[ -n "$pw" ] || fail "no grafana-admin Secret / admin-password rendered"
[ "$pw" != "admin" ] || fail "grafana admin password must not be the literal 'admin'"

# The grafana Deployment must inject the password via __FILE (not a literal env value).
echo "$render" | yq eval-all '
  select(.kind=="Deployment" and (.metadata.name|test("grafana")))
  | .spec.template.spec.containers[0].env[]
  | select(.name=="GF_SECURITY_ADMIN_PASSWORD__FILE")
  | .value' - | grep -q "/etc/grafana/secrets/admin-password" \
  || fail "GF_SECURITY_ADMIN_PASSWORD__FILE must point at the mounted secret file"

# The literal env var must NOT exist (that would mean a raw password in the env table).
echo "$render" | yq eval-all '
  select(.kind=="Deployment" and (.metadata.name|test("grafana")))
  | .spec.template.spec.containers[0].env[]
  | select(.name=="GF_SECURITY_ADMIN_PASSWORD")' - | grep -q . \
  && fail "GF_SECURITY_ADMIN_PASSWORD must not be set as a raw env value"

# The Deployment must mount the grafana-admin Secret.
echo "$render" | yq eval-all '
  select(.kind=="Deployment" and (.metadata.name|test("grafana")))
  | .spec.template.spec.volumes[]
  | select(.secret.secretName|test("grafana-admin"))' - | grep -q . \
  || fail "grafana Deployment must mount the grafana-admin Secret"

echo "PASS: grafana admin credential is generated and injected via __FILE, not admin/admin"
