#!/usr/bin/env bash
# =============================================================================
# Golden render test: NATS bus security assertions
# =============================================================================
# Verifies that `helm template` produces:
#   - TLS required (cert_file/key_file in nats config)
#   - Three users (operator, gateway, bff) with per-service ACLs
#   - Operator-only publish to $KV.mite_desired.>
#   - Gateway-only subscribe to $KV.mite_desired.>
#   - Gateway and BFF have per-bucket JetStream flow-control publish grants
#   - BFF has NO mite_desired access (publish or subscribe)
#   - No NAMES/LIST grants
#   - No fakemite user
#   - Bus passwords reach the components as a mounted file, never an env var
#   - Operator readiness probe hits /readyz (the bus-aware check)
#   - Every bus client has a startup probe outlasting the bus startup wait
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Render the chart (set auth.oidc.clientSecret to bypass required check)
RENDER=$(helm template "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test 2>/dev/null) || {
  echo "FAIL: helm template failed"
  exit 1
}

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

echo "=== NATS bus security golden render assertions ==="

# Keep assertions scoped to one rendered user block without pipefail-sensitive pipelines.
user_block() {
  local user="$1"
  awk -v user="$user" 'index($0, "user: \"" user "\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$RENDER"
}
operator_block=$(user_block operator)
gateway_block=$(user_block gateway)
bff_block=$(user_block bff)

# Scope assertions to one rendered Deployment. The release name is helm's
# default here, so match the "<release>-varroa-<component>" suffix.
deployment_block() {
  local component="$1"
  awk -v suffix="-varroa-$component" '
    $0 == "kind: Deployment" {seen_kind=1; buf=""; capture=0}
    seen_kind && index($0, "  name: ") == 1 && substr($0, length($0) - length(suffix) + 1) == suffix {capture=1; seen_kind=0}
    capture && $0 == "---" {capture=0}
    capture {print}
  ' <<<"$RENDER"
}

# 1. TLS is required — cert_file and key_file present
grep -q '"cert_file": "/etc/nats-certs/nats/tls.crt"' <<<"$RENDER" && \
  grep -q '"key_file": "/etc/nats-certs/nats/tls.key"' <<<"$RENDER"
check "TLS cert_file and key_file in nats config" $?

# 2. Three users present
user_count=$(grep -c 'user: "operator"\|user: "gateway"\|user: "bff"' <<<"$RENDER")
[[ "$user_count" -ge 3 ]]
check "Three users (operator, gateway, bff) defined" $?

# 3. operator publish includes $KV.mite_desired.>
grep -q '\$KV.mite_desired.>' <<<"$operator_block"
check "Operator can publish to \$KV.mite_desired.>" $?

# 3b. operator publish includes mite.*.*.*.out (token grant + imperative commands)
grep -q 'mite.\*.\*.\*.out' <<<"$operator_block"
check "Operator can publish to mite.*.*.*.out (token grant)" $?

# 4. gateway subscribe includes $KV.mite_desired.>
grep -q '\$KV.mite_desired.>' <<<"$gateway_block"
check "Gateway can subscribe to \$KV.mite_desired.>" $?

# 4b. gateway publish includes $JS.ACK.varroa.> (ack the per-controller imperative consumer)
grep -q '\$JS.ACK.varroa.>' <<<"$gateway_block"
check "Gateway can publish \$JS.ACK.varroa.> (imperative consumer ack)" $?

# 4c. gateway publish includes the flow-control grant for the mite_desired KV watcher
grep -q '\$JS.FC.KV_mite_desired.>' <<<"$gateway_block"
check "Gateway can publish \$JS.FC.KV_mite_desired.> (KV watcher flow control)" $?

# 4d. BFF publish includes the flow-control grant for the varroa_clusters KV watcher
grep -q '\$JS.FC.KV_varroa_clusters.>' <<<"$bff_block"
check "BFF can publish \$JS.FC.KV_varroa_clusters.> (KV watcher flow control)" $?

# 5. BFF has NO $KV.mite_desired.> in publish or subscribe
if grep -q '\$KV.mite_desired' <<<"$bff_block"; then
  check "BFF has NO mite_desired access" 1
else
  check "BFF has NO mite_desired access" 0
fi

# 6. No NAMES or LIST grants
names_list=$(grep -c 'NAMES\|LIST' <<<"${operator_block}${gateway_block}${bff_block}" || true)
[[ "$names_list" -eq 0 ]]
check "No NAMES/LIST grants present" $?

# 7. No fakemite user
grep -q 'user: "fakemite"' <<<"$RENDER" && \
  check "No fakemite user" 1 || check "No fakemite user" 0

# 8. Gateway publishes to _INBOX_operator.> and _INBOX_bff.>
grep -q '_INBOX_operator.>' <<<"$gateway_block"
check "Gateway publishes to _INBOX_operator.>" $?
grep -q '_INBOX_bff.>' <<<"$gateway_block"
check "Gateway publishes to _INBOX_bff.>" $?

# 8b. Bus passwords must come from the mounted Secret file, never an env var:
# env vars are frozen at pod start and cannot follow a rotation.
for c in operator gateway bff; do
  block=$(deployment_block "$c")
  if [[ -n "$block" ]]; then
    check "$c Deployment found in render" 0
  else
    check "$c Deployment found in render" 1
  fi

  if grep -q 'name: BUS_PASSWORD' <<<"$block"; then
    check "$c does not receive BUS_PASSWORD via env" 1
  else
    check "$c does not receive BUS_PASSWORD via env" 0
  fi

  if grep -q -- "-bus-pass-file=/etc/nats-creds/${c}-password" <<<"$block"; then
    check "$c passes -bus-pass-file" 0
  else
    check "$c passes -bus-pass-file" 1
  fi
done

# 8c. Operator readiness must hit the bus-aware /readyz endpoint; liveness stays
# on /healthz so a bus outage never restarts the pod.
operator_deployment=$(deployment_block operator)
if grep -A3 readinessProbe <<<"$operator_deployment" | grep -q 'path: /readyz'; then
  check "operator readiness probe hits /readyz" 0
else
  check "operator readiness probe hits /readyz" 1
fi
if grep -A3 livenessProbe <<<"$operator_deployment" | grep -q 'path: /healthz'; then
  check "operator liveness probe stays on /healthz" 0
else
  check "operator liveness probe stays on /healthz" 1
fi

# 8d. Connect blocks at startup for up to bus.DefaultStartupTimeout (3 minutes)
# while it waits out a bus outage or an unsynced rotated Secret, and no probe
# port is listening until it returns. Without a startup probe, liveness kills
# the container mid-wait and the crash loop the retry prevents comes back. The
# window here must stay ahead of DefaultStartupTimeout.
for c in operator gateway bff; do
  probe=$(deployment_block "$c" | awk '/startupProbe:/{p=1;next} p && /^          [a-zA-Z]+:/{p=0} p{print}')
  period=$(awk '/periodSeconds:/{print $2; exit}' <<<"$probe")
  threshold=$(awk '/failureThreshold:/{print $2; exit}' <<<"$probe")
  if [[ -n "$period" && -n "$threshold" ]] && (( period * threshold >= 240 )); then
    check "$c startup probe window (${period}s x ${threshold}) >= 240s" 0
  else
    check "$c startup probe window >= 240s (period='${period}' threshold='${threshold}')" 1
  fi
done

# 9. Auth resource is a Secret (not ConfigMap) — no plaintext passwords
AUTH_RENDER=$(helm template "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test -s templates/nats-auth-config.yaml 2>/dev/null) || {
  echo "FAIL: helm template for auth config failed"
  exit 1
}

grep -q 'kind: Secret' <<<"$AUTH_RENDER"
check "Auth resource is kind: Secret (not ConfigMap)" $?

# 10. Passwords in auth Secret are bcrypt hashes ($2a$...), never plaintext
bad_plaintext=$(echo "$AUTH_RENDER" | grep 'password:' | grep -v 'password: "\$2a' || true)
if [[ -z "$bad_plaintext" ]]; then
  check "All passwords in auth Secret are bcrypt hashes (no plaintext)" 0
else
  check "All passwords in auth Secret are bcrypt hashes (no plaintext)" 1
fi

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
