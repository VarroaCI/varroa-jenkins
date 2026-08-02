#!/usr/bin/env bash
# Run the CI pipeline locally for pre-push validation.
# Requires: golangci-lint, Docker, Go 1.22+
# Usage: bash scripts/ci-local.sh
set -euo pipefail

echo "=== Lint ==="
make lint
echo ""

echo "=== Test ==="
make test
echo ""

echo "=== Build ==="
make build
echo ""

echo "=== Docker Build ==="
make docker-build
echo ""

echo "All stages passed."
