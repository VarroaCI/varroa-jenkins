#!/bin/sh
# Builds the Go binaries of the backend image into one directory.
# The Dockerfile source stage (binaries-source) calls this script. The release
# workflow does not; see the root AGENTS.md CI section for the runner-storage reason.
# Usage: hack/build-image-binaries.sh <out-dir>. GIT_SHA sets main.version. The go toolchain's GOARCH selects the arch.
set -eu
out="${1:?usage: build-image-binaries.sh <out-dir>}"
sha="${GIT_SHA:-unknown}"
export CGO_ENABLED=0 GOOS=linux
mkdir -p "$out"
build() {
  go build -buildvcs=false -ldflags="$3" -o "$out/$1" "$2"
}
versioned="-s -w -X main.version=${sha}"
build varroa-mite         ./cmd/mite/         "$versioned"
build varroa-gateway      ./cmd/gateway/      "-s -w"
build varroa-bff          ./cmd/bff/          "$versioned"
build varroa-operator     ./cmd/operator/     "$versioned"
build varroa-updatecenter ./cmd/updatecenter/ "$versioned"
build varroa-recorder     ./cmd/recorder/     "$versioned"
build varroactl           ./cmd/varroactl/    "-s -w"
