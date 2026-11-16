#!/usr/bin/env bash
# =============================================================================
# Golden render test: the NATS server certificate follows its SAN values
# =============================================================================
# Verifies that varroa.nats.creds (templates/_helpers.tpl):
#   - records the SAN list of the server certificate in the
#     varroa.dev/nats-tls-sans annotation of the creds Secret
#   - keeps every data key when the stored record equals the values
#   - signs a new server certificate with the stored CA when nats.external.host,
#     extraSANs, or ipSANs change, and keeps the CA and the passwords
#   - signs a new server certificate once for a Secret with no record
#   - drops a name that the values no longer hold
#   - fails the render when a new certificate is necessary and the Secret
#     holds no CA key
#   - never reads the creds Secret in hive mode
# It also checks that the NATS reloader watches tls.crt and tls.key. That
# reload is the only path that brings a new certificate to the server.
# helm template has no cluster, so lookup returns nothing. The test copies the
# chart and replaces the one lookup call in varroa.nats.creds with a values
# fixture. The fixture is a creds Secret from an earlier render.
# =============================================================================
# No errexit: each assertion counts its own failure.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE=(--namespace default --set auth.oidc.clientSecret=test --set auth.dashboardUrl=https://varroa.example.test)
EXT=(--set nats.external.host=nats.example.test)
SECRET_TPL=templates/secret-nats-creds.yaml
ANNOTATION='varroa.dev/nats-tls-sans'
INTERNAL='varroa-nats,varroa-nats.default.svc,varroa-nats.default.svc.cluster.local,*.varroa-nats.default.svc.cluster.local'
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
echo "=== 8. Secret with no record: one new certificate ==="
yq 'del(.metadata.annotations)' "$FIRST" > "$TMP/norecord-fixture.yaml"
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

echo ""
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "All NATS TLS re-sign assertions passed."
