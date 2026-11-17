#!/usr/bin/env bash
# =============================================================================
# Golden render test: the NATS certificates follow their SAN values and dates
# =============================================================================
# Verifies that varroa.nats.creds (templates/_helpers.tpl):
#   - records the SAN list of the server certificate in the
#     varroa.dev/nats-tls-sans annotation of the creds Secret
#   - records the end dates of the server certificate and the CA certificate
#     in varroa.dev/nats-tls-not-after and varroa.dev/nats-ca-not-after
#   - makes a CA certificate of 3650 days and a server certificate of 365 days
#   - keeps every data key when the stored records are current
#   - signs a new server certificate with the stored CA when nats.external.host,
#     extraSANs, or ipSANs change, and keeps the CA and the passwords
#   - signs a new server certificate once for a Secret with no SAN record
#   - signs a new server certificate when fewer than 180 days remain, or when
#     the stored date is unreadable
#   - renews the CA certificate with the same key and name when fewer than
#     365 days remain, or when the CA date is absent or unreadable; the
#     earlier and the renewed CA certificate verify each new server
#     certificate
#   - drops a name that the values no longer hold
#   - fails the render when a new certificate is necessary and the Secret
#     holds no CA key
#   - never reads the creds Secret in hive mode
#   - prints the end dates in NOTES, and the member copy step after a CA
#     renewal
# It also checks that the NATS reloader watches tls.crt and tls.key. That
# reload is the only path that brings a new certificate to the server.
# helm template has no cluster, so lookup returns nothing. The test copies the
# chart and replaces the one lookup call in varroa.nats.creds with a values
# fixture. The fixture is a creds Secret from an earlier render.
# The day counts below must equal the counts in varroa.nats.creds.
# The tools are helm, yq v4, openssl, and GNU date.
# =============================================================================
# No errexit: each assertion counts its own failure.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE=(--namespace default --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test)
EXT=(--set nats.external.host=nats.example.test)
SECRET_TPL=templates/secret-nats-creds.yaml
ANNOTATION='varroa.dev/nats-tls-sans'
INTERNAL='varroa-nats,varroa-nats.default.svc,varroa-nats.default.svc.cluster.local,*.varroa-nats.default.svc.cluster.local'
TLS_DATE='varroa.dev/nats-tls-not-after'
CA_DATE='varroa.dev/nats-ca-not-after'
TLS_LIFE_DAYS=365
CA_LIFE_DAYS=3650
TLS_RENEW_DAYS=180
CA_RENEW_DAYS=365
DAY=86400
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# assert DESC CMD...: pass when CMD succeeds.
assert() {
  local desc="$1"
  shift
  if "$@"; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc"
    failures=$((failures + 1))
  fi
}

# refute DESC CMD...: pass when CMD fails.
refute() {
  local desc="$1"
  shift
  if "$@"; then
    echo "  FAIL: $desc"
    failures=$((failures + 1))
  else
    echo "  PASS: $desc"
  fi
}

command -v openssl >/dev/null || { echo "FAIL: openssl is required"; exit 1; }

# field FILE KEY: print one data key of a rendered Secret, base64 decoded.
field() {
  yq ".data[\"$2\"]" "$1" | base64 -d
}

# record FILE: print the SAN record annotation of a rendered Secret.
record() {
  yq ".metadata.annotations[\"$ANNOTATION\"]" "$1"
}

# record_is FILE WANT: the SAN record of FILE is WANT.
record_is() {
  local got
  got="$(record "$1")"
  [[ "$got" == "$2" ]] || { echo "    got '$got'"; return 1; }
}

# has_san FILE SAN: the server certificate in FILE holds SAN, for example
# "DNS:nats.example.test" or "IP Address:192.0.2.10".
has_san() {
  field "$1" tls.crt | openssl x509 -noout -text | grep -qF "$2"
}

# verifies FILE: the server certificate chains to the CA in FILE, and the
# key in FILE belongs to the certificate.
verifies() {
  field "$1" ca.crt > "$TMP/ca.pem"
  field "$1" tls.crt > "$TMP/tls.pem"
  openssl verify -CAfile "$TMP/ca.pem" "$TMP/tls.pem" >/dev/null 2>&1 || return 1
  [[ "$(openssl x509 -in "$TMP/tls.pem" -noout -pubkey)" == "$(field "$1" tls.key | openssl pkey -pubout)" ]]
}

# same A B KEY: the data key KEY is the same in A and B.
same() {
  [[ "$(yq ".data[\"$3\"]" "$1")" == "$(yq ".data[\"$3\"]" "$2")" ]]
}

# same_data A B: every data key is the same in A and B.
same_data() {
  diff <(yq '.data' "$1") <(yq '.data' "$2") >/dev/null
}

# kept A B: the CA and the passwords are the same in A and B.
kept() {
  local k
  for k in ca.crt ca.key operator-password gateway-password bff-password; do
    same "$1" "$2" "$k" || { echo "    $k changed"; return 1; }
  done
}

# kept_key A B: the CA key and the passwords are the same in A and B.
kept_key() {
  local k
  for k in ca.key operator-password gateway-password bff-password; do
    same "$1" "$2" "$k" || { echo "    $k changed"; return 1; }
  done
}

# note FILE KEY: print one annotation of a rendered Secret.
note() {
  yq ".metadata.annotations[\"$2\"]" "$1"
}

# same_note A B KEY: the annotation KEY is the same in A and B.
same_note() {
  [[ "$(note "$1" "$3")" == "$(note "$2" "$3")" ]]
}

# in_days N: print the UTC time N days from now in RFC 3339 form.
in_days() {
  date -u -d "+$1 days" +%Y-%m-%dT%H:%M:%SZ
}

# with_note IN OUT KEY VALUE: write fixture OUT, a copy of IN with one
# annotation set to VALUE.
with_note() {
  yq ".metadata.annotations[\"$3\"] = \"$4\"" "$1" > "$2"
}

# date_matches FILE ANNOTATION KEY: the date in ANNOTATION is the end date of
# the certificate in data key KEY, within two minutes.
date_matches() {
  local got want
  got="$(date -u -d "$(note "$1" "$2")" +%s)" || return 1
  want="$(date -u -d "$(field "$1" "$3" | openssl x509 -noout -enddate | cut -d= -f2)" +%s)" || return 1
  (( got - want <= 120 && want - got <= 120 )) || { echo "    annotation $got, certificate $want"; return 1; }
}

# lives FILE KEY DAYS: the certificate in data key KEY is valid for DAYS days,
# within one day.
lives() {
  field "$1" "$2" | openssl x509 -noout -checkend $((($3 - 1) * DAY)) >/dev/null || return 1
  ! field "$1" "$2" | openssl x509 -noout -checkend $((($3 + 1) * DAY)) >/dev/null
}

# cross CA_FILE LEAF_FILE: the server certificate in LEAF_FILE chains to the
# CA certificate in CA_FILE.
cross() {
  field "$1" ca.crt > "$TMP/cross-ca.pem"
  field "$2" tls.crt > "$TMP/cross-leaf.pem"
  openssl verify -CAfile "$TMP/cross-ca.pem" "$TMP/cross-leaf.pem" >/dev/null 2>&1
}

# same_ca_identity A B: the CA certificates in A and B have the same subject
# and the same public key.
same_ca_identity() {
  [[ "$(field "$1" ca.crt | openssl x509 -noout -subject)" == "$(field "$2" ca.crt | openssl x509 -noout -subject)" ]] || return 1
  [[ "$(field "$1" ca.crt | openssl x509 -noout -pubkey)" == "$(field "$2" ca.crt | openssl x509 -noout -pubkey)" ]]
}

echo "=== 1. First install (no stored Secret) ==="
FIRST="$TMP/first.yaml"
helm template varroa "$ROOT" "${BASE[@]}" "${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10}' -s "$SECRET_TPL" > "$FIRST" 2>"$TMP/err" || {
  echo "FAIL: helm template failed"
  cat "$TMP/err"
  exit 1
}
assert "annotation records the SAN list" record_is "$FIRST" "dns=$INTERNAL,nats.example.test;ip=192.0.2.10"
assert "certificate holds the host as a DNS SAN" has_san "$FIRST" "DNS:nats.example.test"
assert "certificate holds the ipSANs entry" has_san "$FIRST" "IP Address:192.0.2.10"
assert "certificate chains to the CA and matches its key" verifies "$FIRST"
assert "server certificate lives $TLS_LIFE_DAYS days" lives "$FIRST" tls.crt "$TLS_LIFE_DAYS"
assert "CA certificate lives $CA_LIFE_DAYS days" lives "$FIRST" ca.crt "$CA_LIFE_DAYS"
assert "server date annotation is the certificate end date" date_matches "$FIRST" "$TLS_DATE" tls.crt
assert "CA date annotation is the CA certificate end date" date_matches "$FIRST" "$CA_DATE" ca.crt

echo ""
echo "=== 2. NATS reloader watches the certificate files ==="
RELOADER_ARGS=$(helm template varroa "$ROOT" "${BASE[@]}" 2>/dev/null | yq eval-all 'select(.kind == "StatefulSet" and .metadata.name == "varroa-nats") | .spec.template.spec.containers[] | select(.name == "reloader") | .args[]')
assert "reloader watches tls.crt" grep -qx '/etc/nats-certs/nats/tls.crt' <<< "$RELOADER_ARGS"
assert "reloader watches tls.key" grep -qx '/etc/nats-certs/nats/tls.key' <<< "$RELOADER_ARGS"

echo ""
echo "=== 3. Fixture chart ==="
CHART="$TMP/chart"
cp -r "$ROOT" "$CHART"
HELPERS="$CHART/templates/_helpers.tpl"
# shellcheck disable=SC2016 # template text, not shell expansions
SEAM_FROM='lookup "v1" "Secret" $.Release.Namespace $secretName'
SEAM_TO='(.Values.natsCredsFixture | default "" | fromYaml)'
if [[ "$(grep -cF "$SEAM_FROM" "$HELPERS")" != 1 ]]; then
  echo "FAIL: varroa.nats.creds no longer holds one '$SEAM_FROM' call; update this test"
  exit 1
fi
content="$(<"$HELPERS")"
printf '%s\n' "${content/"$SEAM_FROM"/"$SEAM_TO"}" > "$HELPERS"
assert "fixture replaces the creds lookup" grep -qF "$SEAM_TO" "$HELPERS"

# upgrade OUT FIXTURE ARGS...: render the creds Secret over a stored Secret.
upgrade() {
  local out="$1" fixture="$2"
  shift 2
  helm template varroa "$CHART" "${BASE[@]}" --set-file "natsCredsFixture=$fixture" "$@" -s "$SECRET_TPL" > "$out" 2>"$TMP/err"
}

echo ""
echo "=== 4. Same values: no new certificate ==="
assert "render succeeds" upgrade "$TMP/same.yaml" "$FIRST" "${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10}'
assert "every data key stays the same" same_data "$FIRST" "$TMP/same.yaml"
assert "annotation stays the same" record_is "$TMP/same.yaml" "$(record "$FIRST")"
assert "server date annotation stays the same" same_note "$FIRST" "$TMP/same.yaml" "$TLS_DATE"
assert "CA date annotation stays the same" same_note "$FIRST" "$TMP/same.yaml" "$CA_DATE"

echo ""
echo "=== 5. Added ipSANs entry: new certificate from the stored CA ==="
ADDED="$TMP/added.yaml"
assert "render succeeds" upgrade "$ADDED" "$FIRST" "${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10,192.0.2.11}'
assert "CA and passwords stay the same" kept "$FIRST" "$ADDED"
refute "tls.crt changes" same "$FIRST" "$ADDED" tls.crt
refute "tls.key changes" same "$FIRST" "$ADDED" tls.key
assert "new certificate holds the added address" has_san "$ADDED" "IP Address:192.0.2.11"
assert "new certificate keeps the first address" has_san "$ADDED" "IP Address:192.0.2.10"
assert "new certificate chains to the stored CA and matches its key" verifies "$ADDED"
assert "annotation records the new SAN list" record_is "$ADDED" "dns=$INTERNAL,nats.example.test;ip=192.0.2.10,192.0.2.11"

echo ""
echo "=== 6. Added extraSANs entry: new certificate ==="
assert "render succeeds" upgrade "$TMP/dns.yaml" "$FIRST" "${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10}' --set 'nats.external.extraSANs={nats-2.example.test}'
refute "tls.crt changes" same "$FIRST" "$TMP/dns.yaml" tls.crt
assert "new certificate holds the added DNS name" has_san "$TMP/dns.yaml" "DNS:nats-2.example.test"
assert "CA and passwords stay the same" kept "$FIRST" "$TMP/dns.yaml"

echo ""
echo "=== 7. Removed ipSANs entry: the name leaves the certificate ==="
assert "render succeeds" upgrade "$TMP/removed.yaml" "$FIRST" "${EXT[@]}"
refute "removed address leaves the certificate" has_san "$TMP/removed.yaml" "IP Address:192.0.2.10"

echo ""
echo "=== 8. Secret with no SAN record: one new certificate ==="
yq "del(.metadata.annotations[\"$ANNOTATION\"])" "$FIRST" > "$TMP/norecord-fixture.yaml"
assert "render succeeds" upgrade "$TMP/norecord.yaml" "$TMP/norecord-fixture.yaml" "${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10}'
refute "tls.crt changes" same "$FIRST" "$TMP/norecord.yaml" tls.crt
assert "CA and passwords stay the same" kept "$FIRST" "$TMP/norecord.yaml"
assert "annotation is written" record_is "$TMP/norecord.yaml" "$(record "$FIRST")"
assert "new certificate chains to the stored CA" verifies "$TMP/norecord.yaml"

echo ""
echo "=== 9. No CA key and a stale record: the render fails ==="
yq 'del(.data["ca.key"])' "$TMP/norecord-fixture.yaml" > "$TMP/nokey-fixture.yaml"
refute "render fails" upgrade "$TMP/nokey.yaml" "$TMP/nokey-fixture.yaml" "${EXT[@]}"
assert "failure message names ca.key" grep -qF 'ca.key' "$TMP/err"

echo ""
echo "=== 10. Hive mode never reads the creds Secret ==="
yq '{"apiVersion": .apiVersion, "kind": .kind, "metadata": {"name": .metadata.name}, "data": {"operator-password": .data["operator-password"], "gateway-password": .data["gateway-password"], "ca.crt": .data["ca.crt"]}}' "$FIRST" > "$TMP/member-fixture.yaml"
hive() {
  helm template varroa "$CHART" "${BASE[@]}" -f "$CHART/values-hive.yaml" --set cluster.name=member-a --set bus.url=tls://192.0.2.20:4222 --set-file "natsCredsFixture=$TMP/member-fixture.yaml" > "$TMP/hive.yaml" 2>"$TMP/err"
}
assert "hive render succeeds over a member-shaped Secret" hive
refute "hive render holds no creds Secret" grep -qF "$ANNOTATION" "$TMP/hive.yaml"

# Sections 11 to 17 render with the values of the first install, so only the
# dates in the fixture decide.
SAME_VALUES=("${EXT[@]}" --set 'nats.external.ipSANs={192.0.2.10}')

echo ""
echo "=== 11. Server certificate near its end date: new certificate ==="
with_note "$FIRST" "$TMP/tls-due-fixture.yaml" "$TLS_DATE" "$(in_days $((TLS_RENEW_DAYS - 150)))"
TLS_DUE="$TMP/tls-due.yaml"
assert "render succeeds" upgrade "$TLS_DUE" "$TMP/tls-due-fixture.yaml" "${SAME_VALUES[@]}"
refute "tls.crt changes" same "$FIRST" "$TLS_DUE" tls.crt
refute "tls.key changes" same "$FIRST" "$TLS_DUE" tls.key
assert "CA and passwords stay the same" kept "$FIRST" "$TLS_DUE"
assert "CA date annotation stays the same" same_note "$FIRST" "$TLS_DUE" "$CA_DATE"
assert "new certificate chains to the stored CA and matches its key" verifies "$TLS_DUE"
assert "new certificate lives $TLS_LIFE_DAYS days" lives "$TLS_DUE" tls.crt "$TLS_LIFE_DAYS"
assert "server date annotation is the new end date" date_matches "$TLS_DUE" "$TLS_DATE" tls.crt

echo ""
echo "=== 12. Server certificate outside the renewal window: no change ==="
with_note "$FIRST" "$TMP/tls-ok-fixture.yaml" "$TLS_DATE" "$(in_days $((TLS_RENEW_DAYS + 20)))"
assert "render succeeds" upgrade "$TMP/tls-ok.yaml" "$TMP/tls-ok-fixture.yaml" "${SAME_VALUES[@]}"
assert "every data key stays the same" same_data "$FIRST" "$TMP/tls-ok.yaml"
assert "server date annotation keeps the stored value" same_note "$TMP/tls-ok-fixture.yaml" "$TMP/tls-ok.yaml" "$TLS_DATE"

echo ""
echo "=== 13. Unreadable server date: new certificate ==="
with_note "$FIRST" "$TMP/tls-bad-fixture.yaml" "$TLS_DATE" "not-a-date"
assert "render succeeds" upgrade "$TMP/tls-bad.yaml" "$TMP/tls-bad-fixture.yaml" "${SAME_VALUES[@]}"
refute "tls.crt changes" same "$FIRST" "$TMP/tls-bad.yaml" tls.crt
assert "CA and passwords stay the same" kept "$FIRST" "$TMP/tls-bad.yaml"
assert "server date annotation is the new end date" date_matches "$TMP/tls-bad.yaml" "$TLS_DATE" tls.crt

echo ""
echo "=== 14. CA certificate near its end date: renewed with the same key ==="
with_note "$FIRST" "$TMP/ca-due-fixture.yaml" "$CA_DATE" "$(in_days $((CA_RENEW_DAYS - 265)))"
CA_DUE="$TMP/ca-due.yaml"
assert "render succeeds" upgrade "$CA_DUE" "$TMP/ca-due-fixture.yaml" "${SAME_VALUES[@]}"
refute "ca.crt changes" same "$FIRST" "$CA_DUE" ca.crt
assert "CA key and passwords stay the same" kept_key "$FIRST" "$CA_DUE"
assert "renewed CA keeps the subject and the public key" same_ca_identity "$FIRST" "$CA_DUE"
assert "renewed CA lives $CA_LIFE_DAYS days" lives "$CA_DUE" ca.crt "$CA_LIFE_DAYS"
assert "CA date annotation is the new end date" date_matches "$CA_DUE" "$CA_DATE" ca.crt
refute "tls.crt changes too" same "$FIRST" "$CA_DUE" tls.crt
assert "server date annotation is the new end date" date_matches "$CA_DUE" "$TLS_DATE" tls.crt
assert "new server certificate chains to the renewed CA and matches its key" verifies "$CA_DUE"
assert "a member copy of the earlier CA verifies the new server certificate" cross "$FIRST" "$CA_DUE"
assert "the renewed CA verifies the earlier server certificate" cross "$CA_DUE" "$FIRST"

echo ""
echo "=== 15. CA certificate outside the renewal window: no change ==="
with_note "$FIRST" "$TMP/ca-ok-fixture.yaml" "$CA_DATE" "$(in_days $((CA_RENEW_DAYS + 35)))"
assert "render succeeds" upgrade "$TMP/ca-ok.yaml" "$TMP/ca-ok-fixture.yaml" "${SAME_VALUES[@]}"
assert "every data key stays the same" same_data "$FIRST" "$TMP/ca-ok.yaml"
assert "CA date annotation keeps the stored value" same_note "$TMP/ca-ok-fixture.yaml" "$TMP/ca-ok.yaml" "$CA_DATE"

echo ""
echo "=== 16. Secret with no records: CA renewed and new server certificate ==="
yq 'del(.metadata.annotations)' "$FIRST" > "$TMP/bare-fixture.yaml"
BARE="$TMP/bare.yaml"
assert "render succeeds" upgrade "$BARE" "$TMP/bare-fixture.yaml" "${SAME_VALUES[@]}"
refute "ca.crt changes" same "$FIRST" "$BARE" ca.crt
assert "CA key and passwords stay the same" kept_key "$FIRST" "$BARE"
assert "renewed CA keeps the subject and the public key" same_ca_identity "$FIRST" "$BARE"
refute "tls.crt changes" same "$FIRST" "$BARE" tls.crt
assert "SAN record is written" record_is "$BARE" "$(record "$FIRST")"
assert "server date annotation is written" date_matches "$BARE" "$TLS_DATE" tls.crt
assert "CA date annotation is written" date_matches "$BARE" "$CA_DATE" ca.crt
assert "a member copy of the earlier CA verifies the new server certificate" cross "$FIRST" "$BARE"

echo ""
echo "=== 17. Unreadable CA date: CA renewed ==="
with_note "$FIRST" "$TMP/ca-bad-fixture.yaml" "$CA_DATE" "not-a-date"
assert "render succeeds" upgrade "$TMP/ca-bad.yaml" "$TMP/ca-bad-fixture.yaml" "${SAME_VALUES[@]}"
refute "ca.crt changes" same "$FIRST" "$TMP/ca-bad.yaml" ca.crt
assert "CA key and passwords stay the same" kept_key "$FIRST" "$TMP/ca-bad.yaml"

echo ""
echo "=== 18. NOTES name the end dates and the member copy step ==="
# helm template drops NOTES.txt, and a helm 3 dry run needs a cluster. The
# chart copy renders NOTES.txt through tpl in a probe ConfigMap instead.
cp "$CHART/templates/NOTES.txt" "$CHART/notes-probe.txt"
cat > "$CHART/templates/notes-probe.yaml" <<'PROBE'
apiVersion: v1
kind: ConfigMap
metadata:
  name: notes-probe
data:
  notes: |
{{ tpl (.Files.Get "notes-probe.txt") . | indent 4 }}
PROBE
notes() {
  local out="$1" fixture="$2"
  shift 2
  helm template varroa "$CHART" "${BASE[@]}" --set-file "natsCredsFixture=$fixture" "$@" -s templates/notes-probe.yaml 2>"$TMP/err" | yq '.data.notes' > "$out"
}
MEMBERS=("${SAME_VALUES[@]}" --set nats.external.enabled=true --set 'nats.external.allowedCIDRs={0.0.0.0/0}')
assert "NOTES render after a CA renewal" notes "$TMP/notes-renewed.txt" "$TMP/ca-due-fixture.yaml" "${MEMBERS[@]}"
assert "NOTES name the CA end date" grep -qF "CA certificate:     valid until $(note "$CA_DUE" "$CA_DATE" | cut -c1-10)" "$TMP/notes-renewed.txt"
assert "NOTES report the CA renewal" grep -qF "This upgrade renewed the CA certificate." "$TMP/notes-renewed.txt"
assert "NOTES name the member copy step" grep -qF "Copy the new ca.crt to the varroa-nats-creds Secret of each member" "$TMP/notes-renewed.txt"
assert "NOTES render with current dates" notes "$TMP/notes-current.txt" "$FIRST" "${MEMBERS[@]}"
assert "NOTES name the server end date" grep -qF "Server certificate: valid until $(note "$FIRST" "$TLS_DATE")" "$TMP/notes-current.txt"
refute "NOTES report no CA renewal" grep -qF "This upgrade renewed the CA certificate." "$TMP/notes-current.txt"
assert "hive NOTES render" notes "$TMP/notes-hive.txt" "$TMP/member-fixture.yaml" -f "$CHART/values-hive.yaml" --set cluster.name=member-a --set bus.url=tls://192.0.2.20:4222
refute "hive NOTES hold no certificate section" grep -qF "NATS Certificates" "$TMP/notes-hive.txt"

echo ""
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "All NATS TLS re-sign assertions passed."
