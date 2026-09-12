#!/usr/bin/env bash
# =============================================================================
# Golden render test: agent WebSocket door reverse proxy (D9)
# =============================================================================
# Verifies:
#   1. frontend/nginx.conf carries a /runners/ location block that forwards
#      the WebSocket upgrade (Upgrade, Connection: upgrade, HTTP/1.1) with a
#      read and send timeout of at least 3600s.
#   2. The /api/ and /hibernation/ blocks, which cannot carry the upgrade,
#      are unchanged by the addition.
#   3. The chart's Ingress renders with no /runners/ path and is otherwise
#      the same shape as before this change: one host, one "/" path, routed
#      to the frontend Service.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/../.." && pwd)"
NGINX_CONF="$REPO_ROOT/frontend/nginx.conf"

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

# location_block prints one nginx `location <name> { ... }` block: the
# `location` line through its closing brace at column 4 (the file's fixed
# 4-space indentation for a top-level block inside `server { }`).
location_block() {
  local name="$1" file="$2"
  awk -v want="location ${name} {" '
    index($0, want) == 5 { f=1 }
    f { print }
    f && /^    }/ { exit }
  ' "$file"
}

echo "=== 1. /runners/ block exists and carries the upgrade contract ==="
if [[ ! -f "$NGINX_CONF" ]]; then
  echo "FAIL: $NGINX_CONF not found"
  exit 1
fi

RUNNERS_BLOCK=$(location_block "/runners/" "$NGINX_CONF")
[[ -n "$RUNNERS_BLOCK" ]]
check "location /runners/ block present" $?

grep -q 'proxy_set_header Upgrade \$http_upgrade;' <<<"$RUNNERS_BLOCK"
check "forwards the Upgrade header" $?

grep -q 'proxy_set_header Connection "upgrade";' <<<"$RUNNERS_BLOCK"
check "sets Connection: upgrade (not the /api/ block's empty override)" $?

grep -q 'proxy_http_version 1.1;' <<<"$RUNNERS_BLOCK"
check "sets proxy_http_version 1.1" $?

READ_TIMEOUT=$(grep -oE 'proxy_read_timeout [0-9]+s;' <<<"$RUNNERS_BLOCK" | grep -oE '[0-9]+')
[[ -n "$READ_TIMEOUT" && "$READ_TIMEOUT" -ge 3600 ]]
check "proxy_read_timeout is at least 3600s (got '${READ_TIMEOUT:-unset}')" $?

SEND_TIMEOUT=$(grep -oE 'proxy_send_timeout [0-9]+s;' <<<"$RUNNERS_BLOCK" | grep -oE '[0-9]+')
[[ -n "$SEND_TIMEOUT" && "$SEND_TIMEOUT" -ge 3600 ]]
check "proxy_send_timeout is at least 3600s (got '${SEND_TIMEOUT:-unset}')" $?

grep -q 'proxy_pass http://\${VARROA_API_HOST}:8080;' <<<"$RUNNERS_BLOCK"
check "proxies to the BFF" $?

echo "=== 2. The /api/ and /hibernation/ blocks did not gain the upgrade ==="
API_BLOCK=$(location_block "/api/" "$NGINX_CONF")
! grep -q 'Upgrade' <<<"$API_BLOCK"
check "/api/ still sets no Upgrade header" $?
grep -q "proxy_set_header Connection '';" <<<"$API_BLOCK"
check "/api/ still suppresses the upgrade with an empty Connection" $?

HIBERNATION_BLOCK=$(location_block "/hibernation/" "$NGINX_CONF")
! grep -q 'proxy_http_version' <<<"$HIBERNATION_BLOCK"
check "/hibernation/ still sets no proxy_http_version" $?
! grep -q 'Upgrade' <<<"$HIBERNATION_BLOCK"
check "/hibernation/ still sets no Upgrade header" $?

echo "=== 3. nginx accepts the rendered config ==="
if command -v docker >/dev/null 2>&1; then
  RENDERED_CONF=$(mktemp)
  trap 'rm -f "$RENDERED_CONF"' EXIT
  VARROA_API_HOST=localhost envsubst '${VARROA_API_HOST}' <"$NGINX_CONF" >"$RENDERED_CONF"
  if docker run --rm -v "$RENDERED_CONF:/etc/nginx/conf.d/default.conf:ro" nginx:alpine nginx -t \
      >/tmp/runner-door-nginx-t.log 2>&1; then
    check "nginx -t accepts the rendered config" 0
  else
    cat /tmp/runner-door-nginx-t.log
    check "nginx -t accepts the rendered config" 1
  fi
else
  echo "  SKIP: docker not available, cannot run nginx -t"
fi

echo "=== 4. The Ingress carries no /runners/ path and keeps its one-path shape ==="
RENDER=$(helm template "$ROOT" \
  --set auth.oidc.clientSecret=test \
  --set auth.dashboardUrl=http://example.com \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set global.domain=example.com \
  --show-only templates/ingress/ingress.yaml 2>/dev/null) || {
  echo "FAIL: helm template (ingress) failed"
  exit 1
}

[[ "$(grep -c 'path: /' <<<"$RENDER")" == 2 ]]
check "exactly two '/' paths render (dex host + frontend host, unchanged)" $?

! grep -q '/runners' <<<"$RENDER"
check "no /runners path in the rendered Ingress" $?

FRONTEND_SVC=$(grep -c 'name: release-name-frontend' <<<"$RENDER" 2>/dev/null || true)
[[ "${FRONTEND_SVC:-0}" -ge 1 ]]
check "the frontend Service is still the backend for the '/' path" $?

echo ""
if [[ $failures -eq 0 ]]; then
  echo "All checks passed."
  exit 0
else
  echo "$failures check(s) failed."
  exit 1
fi
