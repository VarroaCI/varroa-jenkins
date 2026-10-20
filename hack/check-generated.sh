#!/usr/bin/env bash
# Fails when generated output under the paths differs from the index. Run it after the generator. It reports changed, deleted, and untracked files.
#
# Usage: hack/check-generated.sh <make-target> <path>...
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: hack/check-generated.sh <make-target> <path>..." >&2
  exit 2
fi

target=$1
shift
rc=0

git diff --exit-code -- "$@" || rc=1

deleted=$(git ls-files --deleted -- "$@")
if [ -n "$deleted" ]; then
  echo "check-generated: tracked files the generator no longer writes (git rm them):" >&2
  echo "$deleted" >&2
  rc=1
fi

untracked=$(git ls-files --others --exclude-standard -- "$@")
if [ -n "$untracked" ]; then
  echo "check-generated: untracked generated files (git add them):" >&2
  echo "$untracked" >&2
  rc=1
fi

if [ "$rc" -ne 0 ]; then
  echo "check-generated: run 'make $target' and commit the result" >&2
fi
exit "$rc"
