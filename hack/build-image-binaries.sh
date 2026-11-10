#!/bin/sh
# Builds the Go binaries of the backend image into one directory.
# The Dockerfile source stage and both release workflows call this script.
# The release workflows call it once for each target arch.
# The Checks job of both pr.yaml workflows calls it with --check and GOARCH=arm64.
# Usage: hack/build-image-binaries.sh <out-dir>. GIT_SHA sets main.version. GOARCH selects the target arch; it defaults to the host arch.
# Usage: hack/build-image-binaries.sh --check. It builds each binary and writes no binary.
# With --check, the script tries every binary and prints the elapsed time. It exits 1 when one or more binaries fail.
set -eu
usage="usage: build-image-binaries.sh <out-dir> | --check"
arg="${1:?$usage}"
sha="${GIT_SHA:-unknown}"
export CGO_ENABLED=0 GOOS=linux
check=false
out=""
case "$arg" in
  --check) check=true ;;
  -*) echo "$usage" >&2; exit 2 ;;
  *) out="$arg"; mkdir -p "$out" ;;
esac
arch="$(go env GOARCH)"
failed=""
start=$(date +%s)
build() {
  if [ "$check" = true ]; then
    echo "=== $1 linux/$arch"
    go build -buildvcs=false -ldflags="$3" -o /dev/null "$2" || failed="$failed $1"
  else
    go build -buildvcs=false -ldflags="$3" -o "$out/$1" "$2"
  fi
}
versioned="-s -w -X main.version=${sha}"
build varroa-mite         ./cmd/mite/         "$versioned"
build varroa-gateway      ./cmd/gateway/      "-s -w"
build varroa-bff          ./cmd/bff/          "$versioned"
build varroa-operator     ./cmd/operator/     "$versioned"
build varroa-updatecenter ./cmd/updatecenter/ "$versioned"
build varroa-recorder     ./cmd/recorder/     "$versioned"
build varroa-agent        ./cmd/agentrunner/  "$versioned"
build varroactl           ./cmd/varroactl/    "-s -w"
if [ "$check" = true ]; then
  echo "Elapsed: $(( $(date +%s) - start ))s"
  if [ -n "$failed" ]; then
    echo "These image binaries do not build for linux/$arch:$failed" >&2
    exit 1
  fi
  echo "Each image binary builds for linux/$arch."
fi
