#!/usr/bin/env bash
# Render-assertion test for Dex gRPC API (#415).
# Verifies the gRPC port is disabled by default, gated behind
# dex.grpcApi.enabled, and that a custom dex.config.grpc block
# doesn't cause a duplicate-key hazard.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x --set global.domain=example.com --set auth.dashboardUrl=https://app.example.com)
fail() { echo "FAIL: $*" >&2; exit 1; }

default_render="$(helm template t "$CHART_DIR" "${COMMON[@]}")"
enabled_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set dex.grpcApi.enabled=true)"
custom_grpc_render="$(helm template t "$CHART_DIR" "${COMMON[@]}" --set dex.grpcApi.enabled=true \
  --set dex.config.grpc.addr=0.0.0.0:5557 --set dex.config.grpc.tls.cert=/etc/dex/tls/tls.crt)"

# Default: no grpc port anywhere, no grpc: stanza in the rendered dex config.
echo "$default_render" | yq eval-all 'select(.kind=="Service" and (.metadata.name|test("dex$"))) | .spec.ports[] | select(.name=="grpc")' - \
  | grep -q . && fail "default: dex Service must not expose a grpc port"
echo "$default_render" | yq eval-all 'select(.kind=="Deployment" and (.metadata.name|test("dex$"))) | .spec.template.spec.containers[0].ports[] | select(.name=="grpc")' - \
  | grep -q . && fail "default: dex Deployment must not expose a grpc containerPort"
echo "$default_render" | yq eval-all 'select(.kind=="Secret" and (.metadata.name|test("dex$"))) | .stringData["config.yaml"]' - \
  | grep -q "^grpc:" && fail "default: dex config.yaml must not enable grpc"

# Opt-in: dex.grpcApi.enabled=true renders the synthesized port and stanza.
echo "$enabled_render" | yq eval-all 'select(.kind=="Service" and (.metadata.name|test("dex$"))) | .spec.ports[] | select(.name=="grpc") | .port' - \
  | grep -qx 5557 || fail "opt-in: dex Service must expose grpc:5557 when enabled"
echo "$enabled_render" | yq eval-all 'select(.kind=="Secret" and (.metadata.name|test("dex$"))) | .stringData["config.yaml"]' - \
  | grep -q "^grpc:" || fail "opt-in: dex config.yaml must render grpc stanza when enabled"

# Custom dex.config.grpc + grpcApi.enabled=true: exactly one grpc: key, ports still render.
grpc_key_count="$(echo "$custom_grpc_render" | yq eval-all 'select(.kind=="Secret" and (.metadata.name|test("dex$"))) | .stringData["config.yaml"]' - | grep -c '^grpc:')"
[ "$grpc_key_count" -eq 1 ] || fail "custom dex.config.grpc + grpcApi.enabled must not duplicate the grpc: key (got $grpc_key_count)"
echo "$custom_grpc_render" | yq eval-all 'select(.kind=="Secret" and (.metadata.name|test("dex$"))) | .stringData["config.yaml"]' - \
  | grep -q "tls" || fail "custom dex.config.grpc block must be preserved verbatim"

echo "PASS: dex gRPC API is off by default, opt-in via dex.grpcApi.enabled, no duplicate-key hazard"
