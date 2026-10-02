#!/usr/bin/env bash
# The gateway's ClusterRole grants the cell tenant resolver its reads:
# controllers and teams, get/list/watch, beside the groups list.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
R="$(helm template varroa "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com)"
# Read the render from a here-string. A pipe from printf fails under
# pipefail when awk exits before printf writes the whole render.
CR="$(awk '/^kind: ClusterRole$/{p=1} p&&/name: varroa-varroa-gateway-group-reader/{q=1} p&&q&&/^---/{exit} p&&q' <<<"$R")"
[ -n "$CR" ] || { echo "FAIL: gateway ClusterRole not rendered"; exit 1; }
for want in '"controllers"' '"teams"' '"watch"'; do
  grep -q -- "$want" <<<"$CR" || { echo "FAIL: gateway ClusterRole lacks $want"; exit 1; }
done
echo "PASS: gateway ClusterRole reads controllers and teams"
