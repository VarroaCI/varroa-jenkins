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

# 11. A disabled record store defines exactly three identities and no
# recorder user.
disabled_user_count=$(grep -c 'user: "operator"\|user: "gateway"\|user: "bff"\|user: "recorder"' <<<"$RENDER")
[[ "$disabled_user_count" -eq 3 ]]
check "recordStore disabled: exactly three identities" $?

grep -q 'user: "recorder"' <<<"$RENDER" && check "recordStore disabled: no recorder user" 1 || check "recordStore disabled: no recorder user" 0

# 12. An enabled record store adds the recorder identity with exactly its
# D3 publish/subscribe grants, and extends the operator and gateway grants.
RS_RENDER=$(helm template "$ROOT" --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test --set recordStore.enabled=true -s templates/nats-auth-config.yaml 2>/dev/null) || {
  echo "FAIL: helm template (recordStore.enabled) failed"
  exit 1
}

rs_user_count=$(grep -c 'user: "operator"\|user: "gateway"\|user: "bff"\|user: "recorder"' <<<"$RS_RENDER")
[[ "$rs_user_count" -eq 4 ]]
check "recordStore enabled: exactly four identities" $?

recorder_block=$(awk 'index($0, "user: \"recorder\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$RS_RENDER")

for subj in \
  '"varroa.ledger.actions.>"' \
  '_INBOX_gateway.>' \
  '\$JS.API.STREAM.CREATE.varroa_builds' \
  '\$JS.API.STREAM.UPDATE.varroa_builds' \
  '\$JS.API.STREAM.INFO.varroa_builds' \
  '\$JS.API.CONSUMER.CREATE.varroa_builds.>' \
  '\$JS.API.CONSUMER.DURABLE.CREATE.varroa_builds.>' \
  '\$JS.API.CONSUMER.INFO.varroa_builds.>' \
  '\$JS.API.CONSUMER.MSG.NEXT.varroa_builds.>' \
  '\$JS.ACK.varroa_builds.>' \
  '\$JS.API.STREAM.CREATE.varroa_activity' \
  '\$JS.API.STREAM.UPDATE.varroa_activity' \
  '\$JS.API.STREAM.INFO.varroa_activity' \
  '\$JS.API.CONSUMER.CREATE.varroa_activity.>' \
  '\$JS.API.CONSUMER.DURABLE.CREATE.varroa_activity.>' \
  '\$JS.API.CONSUMER.INFO.varroa_activity.>' \
  '\$JS.API.CONSUMER.MSG.NEXT.varroa_activity.>' \
  '\$JS.ACK.varroa_activity.>'; do
  grep -q "$subj" <<<"$recorder_block"
  check "recorder publishes $subj" $?
done

for subj in 'varroa.ledger.claim' '_INBOX_recorder.>'; do
  grep -q "$subj" <<<"$recorder_block"
  check "recorder subscribes $subj" $?
done

# One recorder serves every cluster of the brood, so its ledger-action grant
# is the wildcard. A grant that names one cluster denies every member's action.
if grep -q '"varroa.ledger.actions.core"' <<<"$recorder_block"; then
  check "recorder does NOT hold the single-cluster ledger-action grant" 1
else
  check "recorder does NOT hold the single-cluster ledger-action grant" 0
fi
recorder_subscribe=$(awk '/subscribe: \[/{capture=1} capture{print} capture && /\]/{exit}' <<<"$recorder_block")
if grep -q 'varroa.ledger.actions' <<<"$recorder_subscribe"; then
  check "recorder does NOT subscribe to ledger actions" 1
else
  check "recorder does NOT subscribe to ledger actions" 0
fi

# The recorder holds no core activity.> grant, no KV grant, no mite grant, and
# no wake grant. The quoted form distinguishes the core "activity.>" subject
# from the recorder's legitimate "varroa_activity" JetStream subjects, which
# end in the same four characters.
for forbidden in '"activity.>"' '\$KV.' 'mite.' 'wake.>'; do
  if grep -q "$forbidden" <<<"$recorder_block"; then
    check "recorder does NOT hold $forbidden" 1
  else
    check "recorder does NOT hold $forbidden" 0
  fi
done

# The bff holds no JetStream operation on varroa_activity: it reads activity
# through the record store's SQL reader, not the stream.
bff_rs_block=$(awk 'index($0, "user: \"bff\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$RS_RENDER")
if grep -q 'varroa_activity' <<<"$bff_rs_block"; then
  check "bff does NOT hold any varroa_activity JetStream operation" 1
else
  check "bff does NOT hold any varroa_activity JetStream operation" 0
fi
grep -q '"activity\.>"' <<<"$bff_rs_block"
check "bff (recordStore enabled) keeps core activity.> access" $?

gateway_rs_block=$(awk 'index($0, "user: \"gateway\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$RS_RENDER")
grep -q 'builds.>' <<<"$gateway_rs_block"
check "gateway (recordStore enabled) publishes builds.>" $?
grep -q 'varroa.ledger.claim' <<<"$gateway_rs_block"
check "gateway (recordStore enabled) publishes varroa.ledger.claim" $?

operator_rs_block=$(awk 'index($0, "user: \"operator\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$RS_RENDER")
grep -q 'varroa.ledger.actions.>' <<<"$operator_rs_block"
check "operator (recordStore enabled) subscribes varroa.ledger.actions.>" $?

# 13. Live tail (control-bus-security): the bff subscribes to logs.> and
# publishes to varroa.tail.> only when the record store is enabled, and no
# other identity gains either subject.
bff_rs_publish=$(awk '/publish: \[/{p=1;next} p && /\]/{exit} p{print}' <<<"$bff_rs_block")
bff_rs_subscribe=$(awk '/subscribe: \[/{p=1;next} p && /\]/{exit} p{print}' <<<"$bff_rs_block")
grep -q '"varroa.tail.>"' <<<"$bff_rs_publish"
check "bff (recordStore enabled) publishes varroa.tail.>" $?
grep -q '"logs.>"' <<<"$bff_rs_subscribe"
check "bff (recordStore enabled) subscribes logs.>" $?

for identity_block in "operator_rs_block:$operator_rs_block" "recorder_block:$recorder_block"; do
  name="${identity_block%%:*}"
  block="${identity_block#*:}"
  if grep -q '"logs.>"\|"varroa.tail.>"' <<<"$block"; then
    check "$name holds NEITHER logs.> nor varroa.tail.>" 1
  else
    check "$name holds NEITHER logs.> nor varroa.tail.>" 0
  fi
done

# A disabled record store withholds both grants from the bff: the tail
# feature depends on the record store, so its subjects follow the same gate
# as the gateway's own logs.>/varroa.tail.> grants.
if grep -q '"logs.>"\|"varroa.tail.>"' <<<"$bff_block"; then
  check "bff (recordStore disabled) holds NEITHER logs.> nor varroa.tail.>" 1
else
  check "bff (recordStore disabled) holds NEITHER logs.> nor varroa.tail.>" 0
fi

# 14. Pipeline trigger/cancel (control-bus-security): the bff publishes to
# mite.*.*.*.out only when the record store is enabled, following the same
# gate as the tail grants. The recorder never holds it.
grep -q '"mite\.\*\.\*\.\*\.out"' <<<"$bff_rs_publish"
check "bff (recordStore enabled) publishes mite.*.*.*.out" $?

if grep -q 'mite\.\*\.\*\.\*\.out' <<<"$bff_block"; then
  check "bff (recordStore disabled) does NOT hold mite.*.*.*.out" 1
else
  check "bff (recordStore disabled) does NOT hold mite.*.*.*.out" 0
fi

if grep -q 'mite\.\*\.\*\.\*\.out' <<<"$recorder_block"; then
  check "recorder does NOT hold mite.*.*.*.out" 1
else
  check "recorder does NOT hold mite.*.*.*.out" 0
fi

# 15. Tenant schedule purge (tenant-placement): the operator requests
# varroa.recordstore.tenant.purge only when the record store is enabled,
# the recorder subscribes to it, and the recorder can reply into the
# operator's own inbox.
grep -q 'varroa.recordstore.tenant.purge' <<<"$operator_rs_block"
check "operator (recordStore enabled) publishes varroa.recordstore.tenant.purge" $?

if grep -q 'varroa.recordstore.tenant.purge' <<<"$operator_block"; then
  check "operator (recordStore disabled) does NOT hold varroa.recordstore.tenant.purge" 1
else
  check "operator (recordStore disabled) does NOT hold varroa.recordstore.tenant.purge" 0
fi

grep -q 'varroa.recordstore.tenant.purge' <<<"$recorder_block"
check "recorder subscribes varroa.recordstore.tenant.purge" $?

grep -q '_INBOX_operator.>' <<<"$recorder_block"
check "recorder publishes _INBOX_operator.>" $?

# 16. Tenant sync (placement-fencing, build-record-store): the operator
# requests varroa.recordstore.tenant.sync only when the record store is
# enabled, and subscribes its own reply inbox; the recorder subscribes
# to the request subject and can reply into the operator's own inbox.
grep -q 'varroa.recordstore.tenant.sync' <<<"$operator_rs_block"
check "operator (recordStore enabled) publishes varroa.recordstore.tenant.sync" $?

if grep -q 'varroa.recordstore.tenant.sync' <<<"$operator_block"; then
  check "operator (recordStore disabled) does NOT hold varroa.recordstore.tenant.sync" 1
else
  check "operator (recordStore disabled) does NOT hold varroa.recordstore.tenant.sync" 0
fi

grep -q '_INBOX_operator.>' <<<"$operator_block"
check "operator subscribes its own reply inbox _INBOX_operator.>" $?

grep -q 'varroa.recordstore.tenant.sync' <<<"$recorder_block"
check "recorder subscribes varroa.recordstore.tenant.sync" $?

# 17. Cross-tenant pipeline calls (control-bus-security, D16): the gateway
# requests varroa.ledger.crosstenant only when the record store is
# enabled, and the recorder answers it. No other identity holds it, and
# no mite NATS identity exists at all in this chart — a mite only ever
# reaches the recorder through the gateway's relay.
grep -q 'varroa.ledger.crosstenant' <<<"$gateway_rs_block"
check "gateway (recordStore enabled) publishes varroa.ledger.crosstenant" $?

if grep -q 'varroa.ledger.crosstenant' <<<"$gateway_block"; then
  check "gateway (recordStore disabled) does NOT hold varroa.ledger.crosstenant" 1
else
  check "gateway (recordStore disabled) does NOT hold varroa.ledger.crosstenant" 0
fi

grep -q 'varroa.ledger.crosstenant' <<<"$recorder_block"
check "recorder subscribes varroa.ledger.crosstenant" $?

for identity_block in "operator_rs_block:$operator_rs_block" "bff_rs_block:$bff_rs_block"; do
  name="${identity_block%%:*}"
  block="${identity_block#*:}"
  if grep -q 'varroa.ledger.crosstenant' <<<"$block"; then
    check "$name does NOT hold varroa.ledger.crosstenant" 1
  else
    check "$name does NOT hold varroa.ledger.crosstenant" 0
  fi
done

# 18. Cell-loss detection (control-bus-security): the operator subscribes
# to events.brood.> so a mite disconnect reaches a pod read at once. The
# operator already publishes events.brood.*, so the subscribe grant is
# matched on the ">" form alone. The recorder holds no brood grant in
# either direction: it decides no cell loss and reads no brood event.
grep -q '"events.brood.>"' <<<"$operator_block"
check "operator subscribes events.brood.>" $?

grep -q '"events.brood.>"' <<<"$operator_rs_block"
check "operator (recordStore enabled) subscribes events.brood.>" $?

if grep -q 'events.brood' <<<"$recorder_block"; then
  check "recorder holds NO events.brood grant in either direction" 1
else
  check "recorder holds NO events.brood grant in either direction" 0
fi

if grep -q 'user: "mite"' <<<"$RS_RENDER"; then
  check "no mite NATS identity exists" 1
else
  check "no mite NATS identity exists" 0
fi

# 19. Live run events: the recorder publishes runs.> and the bff subscribes
# to it only when the record store is enabled, and no other identity holds
# the subject in either direction.
grep -q '"runs\.>"' <<<"$recorder_block"
check "recorder (recordStore enabled) publishes runs.>" $?

grep -q '"runs\.>"' <<<"$bff_rs_subscribe"
check "bff (recordStore enabled) subscribes runs.>" $?

for identity_block in "operator_rs_block:$operator_rs_block" "gateway_rs_block:$gateway_rs_block"; do
  name="${identity_block%%:*}"
  block="${identity_block#*:}"
  if grep -q 'runs\.>' <<<"$block"; then
    check "$name does NOT hold runs.>" 1
  else
    check "$name does NOT hold runs.>" 0
  fi
done

# The tail grants follow the record store gate, and this subject follows the
# same gate: a disabled record store runs no recorder, so it publishes
# nothing to read.
if grep -q 'runs\.>' <<<"$bff_block"; then
  check "bff (recordStore disabled) does NOT hold runs.>" 1
else
  check "bff (recordStore disabled) does NOT hold runs.>" 0
fi

if grep -q 'runs\.>' <<<"$operator_block$gateway_block"; then
  check "recordStore disabled: no other identity holds runs.>" 1
else
  check "recordStore disabled: no other identity holds runs.>" 0
fi

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
