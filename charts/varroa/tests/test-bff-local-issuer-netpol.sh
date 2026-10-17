#!/usr/bin/env bash
# =============================================================================
# Golden render test: BFF ingress from controller pods for the local issuer
# =============================================================================
# With a local issuer, each Jenkins controller fetches the BFF's OIDC
# discovery document and JWKS from auth.bffUrl. The varroa-np-bff policy must
# admit controller pods, selected by the fleet label, on the BFF port. Without
# a local issuer, the policy must not admit them.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=test --set auth.dashboardUrl=http://example.com
  --set networkPolicy.enabled=true)
BFF_URL=(--set auth.bffUrl=http://varroa-varroa-bff.varroa-system.svc.cluster.local:8080)

failures=0

# check runs a command and records a PASS or FAIL for its description.
check() {
  local desc="$1"
  shift
  if "$@"; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc"
    failures=$((failures + 1))
  fi
}

# has and lacks test whether a pattern occurs in a text.
has() { grep -q -- "$1" <<<"$2"; }
lacks() { ! grep -q -- "$1" <<<"$2"; }

# bff_policy prints the varroa-np-bff NetworkPolicy document of a render.
bff_policy() {
  awk 'BEGIN{RS="---"} /name: [^\n]*-np-bff\n/' <<<"$1"
}

# issuer_rule prints the local-issuer ingress rule of a bff policy.
issuer_rule() {
  awk '/# Local issuer:/{f=1} f&&/^  egress:/{exit} f' <<<"$1"
}

echo "=== BFF local-issuer ingress golden render assertions ==="

echo "--- default oidc: no controller ingress ---"
POLICY=$(bff_policy "$(helm template "$ROOT" "${COMMON[@]}")")
INGRESS=$(awk '/^  ingress:/{f=1} /^  egress:/{f=0} f' <<<"$POLICY")
check "bff policy renders" has 'policyTypes' "$POLICY"
check "bff ingress names no controller pods" lacks 'managed-by: varroa-operator' "$INGRESS"

echo "--- localAccounts beside oidc ---"
RULE=$(issuer_rule "$(bff_policy "$(helm template "$ROOT" "${COMMON[@]}" "${BFF_URL[@]}" \
  --set auth.localAccounts.enabled=true)")")
check "namespaceSelector matches every namespace" has '- namespaceSelector: {}' "$RULE"
# The pod selector belongs to the same peer as the namespace selector, so the
# rule does not admit every pod in every namespace.
check "podSelector shares the namespaceSelector peer" \
  has '^          podSelector:' "$(grep -A1 -- '- namespaceSelector: {}' <<<"$RULE")"
check "pod selector is the fleet label" has 'app.kubernetes.io/managed-by: varroa-operator' "$RULE"
check "rule admits the BFF port" has 'port: 8080' "$RULE"

echo "--- mode local ---"
RULE=$(issuer_rule "$(bff_policy "$(helm template "$ROOT" "${COMMON[@]}" "${BFF_URL[@]}" \
  --set auth.mode=local --set dex.enabled=false)")")
check "mode local admits controller pods" has 'app.kubernetes.io/managed-by: varroa-operator' "$RULE"

echo "--- mode ldap ---"
RULE=$(issuer_rule "$(bff_policy "$(helm template "$ROOT" "${COMMON[@]}" "${BFF_URL[@]}" \
  --set auth.mode=ldap --set dex.enabled=false)")")
check "mode ldap admits controller pods" has 'app.kubernetes.io/managed-by: varroa-operator' "$RULE"

echo "--- tenantNamespaceSelector and bff.service.port ---"
RULE=$(issuer_rule "$(bff_policy "$(helm template "$ROOT" "${COMMON[@]}" "${BFF_URL[@]}" \
  --set auth.localAccounts.enabled=true \
  --set-string 'networkPolicy.tenantNamespaceSelector.varroa\.dev/tenant=true' \
  --set bff.service.port=8181)")")
check "namespaceSelector uses tenantNamespaceSelector" has 'varroa.dev/tenant: "true"' "$RULE"
check "no all-namespace selector with a tenant selector" lacks 'namespaceSelector: {}' "$RULE"
check "rule follows bff.service.port" has 'port: 8181' "$RULE"

echo "--- hive mode ---"
HIVE=$(helm template "$ROOT" -f "$ROOT/values-hive.yaml" "${COMMON[@]}" "${BFF_URL[@]}" \
  --set cluster.name=x --set bus.url=nats://h:4222 --set auth.localAccounts.enabled=true \
  --set-string 'networkPolicy.coreNatsEgress.cidrs={192.0.2.10/32}')
check "hive render succeeds" has 'np-default-deny' "$HIVE"
check "hive renders no bff policy" lacks 'name: .*-np-bff$' "$HIVE"

echo
if [[ $failures -gt 0 ]]; then
  echo "FAILED: $failures assertion(s)"
  exit 1
fi
echo "All BFF local-issuer ingress assertions passed."
