#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RENDER=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test \
  --set auth.dashboardUrl=http://example.com \
  --set networkPolicy.enabled=true 2>/dev/null)
RULE=$(grep -A12 'call_jenkins_tool MCP proxy' <<<"$RENDER")

# The namespace and pod selectors belong to the same peer, so this is not an
# all-pods TCP 8080 allowance.
grep -q 'namespaceSelector: {}' <<<"$RULE"
grep -q 'podSelector:' <<<"$RULE"
grep -q 'app.kubernetes.io/managed-by: varroa-operator' <<<"$RULE"
grep -q 'protocol: TCP' <<<"$RULE"
grep -q 'port: 8080' <<<"$RULE"

echo "MCP BFF egress selects labeled controller pods on TCP 8080"
