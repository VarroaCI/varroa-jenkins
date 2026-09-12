#!/usr/bin/env bash
# The gateway's ClusterRole grants the cell tenant resolver its reads:
# controllers and teams, get/list/watch, beside the groups list.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
R="$(helm template varroa "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com)"
CR="$(printf '%s\n' "$R" | awk '/^kind: ClusterRole$/{p=1} p&&/name: varroa-varroa-gateway-group-reader/{q=1} p&&q&&/^---/{exit} p&&q')"
[ -n "$CR" ] || { echo "FAIL: gateway ClusterRole not rendered"; exit 1; }
for want in '"controllers"' '"teams"' '"watch"'; do
  printf '%s\n' "$CR" | grep -q -- "$want" || { echo "FAIL: gateway ClusterRole lacks $want"; exit 1; }
done
echo "PASS: gateway ClusterRole reads controllers and teams"
