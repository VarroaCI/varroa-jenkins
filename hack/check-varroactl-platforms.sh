#!/usr/bin/env bash
# Builds varroactl for each platform that a release publishes.
#
# This file holds the one list of varroactl release platforms. The release
# job that publishes the varroactl archives reads the list with --list.
#
# Usage:
#   hack/check-varroactl-platforms.sh          Builds each platform. Writes no binary.
#   hack/check-varroactl-platforms.sh --list   Prints one os/arch per line.
#
# The build tries every platform, then exits 1 when one or more failed.
set -euo pipefail

platforms=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
)

usage() {
  echo "usage: hack/check-varroactl-platforms.sh [--list]" >&2
  exit 2
}

if [ "$#" -gt 1 ]; then usage; fi
case "${1:-}" in
  --list)
    printf '%s\n' "${platforms[@]}"
    exit 0
    ;;
  "") ;;
  *) usage ;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# CGO_ENABLED=0 matches the release build. A cgo build needs a C toolchain for each target.
failed=()
for target in "${platforms[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  echo "=== varroactl ${target}"
  if ! CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -o /dev/null ./cmd/varroactl; then
    failed+=("$target")
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo "varroactl does not build for: ${failed[*]}" >&2
  exit 1
fi
echo "varroactl builds for each release platform."
