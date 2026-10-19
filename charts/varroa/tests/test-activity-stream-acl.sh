#!/usr/bin/env bash
# =============================================================================
# Golden render test: activity stream JetStream ACLs (D10)
# =============================================================================
# Verifies that `helm template` with recordStore.enabled=true produces:
#   - The recorder holds the JetStream operations for varroa_activity: stream
#     create/update/info, consumer create/durable-create/info/msg-next, and
#     the stream's ack subject.
#   - The recorder publishes core activity.> (TR3) and holds no activity.> subscribe grant.
#   - The bff holds no JetStream operation on varroa_activity.
#   - The bff keeps its core activity.> publish and subscribe grant.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON="--set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.test"

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

user_block() {
  local user="$1"
  local input="$2"
  awk -v user="$user" 'index($0, "user: \"" user "\""){capture=1} capture{print} capture && $0 == "        }"{exit}' <<<"$input"
}

echo "=== Activity stream ACL golden render assertions (D10) ==="

RENDER=$(helm template "$ROOT" $COMMON --set recordStore.enabled=true -s templates/nats-auth-config.yaml 2>/dev/null) || {
  echo "FAIL: helm template failed"
  exit 1
}

recorder_block=$(user_block recorder "$RENDER")
recorder_subscribe=$(awk '/subscribe: \[/{capture=1} capture{print} capture && /\]/{exit}' <<<"$recorder_block")
bff_block=$(user_block bff "$RENDER")

echo ""
echo "--- recorder: gains the varroa_activity JetStream operations ---"
for subj in \
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

echo ""
echo "--- recorder: publishes activity.> (TR3), never subscribes to it ---"
# The quoted literal distinguishes the core "activity.>" subject from the
# recorder's legitimate "varroa_activity" JetStream subjects, which end in
# the same four characters. The recorder publishes the audit producers'
# subject; it reads nothing from it.
recorder_publish=$(awk '/publish: \[/{capture=1} capture{print} capture && /\]/{exit}' <<<"$recorder_block")
grep -q '"activity\.>"' <<<"$recorder_publish"
check "recorder publishes activity.>" $?
if grep -q '"activity\.>"' <<<"$recorder_subscribe"; then
  check "recorder does NOT subscribe to activity.>" 1
else
  check "recorder does NOT subscribe to activity.>" 0
fi

echo ""
echo "--- bff: loses every JetStream operation on varroa_activity ---"
if grep -q 'varroa_activity' <<<"$bff_block"; then
  check "bff does NOT hold any varroa_activity JetStream operation" 1
else
  check "bff does NOT hold any varroa_activity JetStream operation" 0
fi

echo ""
echo "--- bff: keeps core activity.> publish and subscribe ---"
bff_publish=$(awk '/publish: \[/{p=1;next} p && /\]/{exit} p{print}' <<<"$bff_block")
bff_subscribe=$(awk '/subscribe: \[/{p=1;next} p && /\]/{exit} p{print}' <<<"$bff_block")
grep -q 'activity\.>' <<<"$bff_publish"
check "bff publishes activity.>" $?
grep -q 'activity\.>' <<<"$bff_subscribe"
check "bff subscribes activity.>" $?

echo ""
if [[ "$failures" -eq 0 ]]; then
  echo "=== ALL ASSERTIONS PASSED ==="
else
  echo "=== $failures ASSERTION(S) FAILED ==="
fi
exit "$failures"
