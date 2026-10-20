#!/usr/bin/env bash
# hack/check-generated_test.sh — tests for hack/check-generated.sh.
# Each case builds a scratch git repo and runs the helper against it.
#
# Run: bash hack/check-generated_test.sh
set -uo pipefail

HELPER="$(cd "$(dirname "$0")" && pwd)/check-generated.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
PASS=0
FAIL=0
ERR="$WORK/stderr"

repo="$WORK/repo"
mkdir -p "$repo/out" "$repo/src"
(
  cd "$repo" || exit 1
  git init -q
  echo a >out/a.txt
  echo b >out/b.txt
  echo hand >src/hand.txt
  git add out src
  git -c user.email=t@t -c user.name=t commit -q -m init
) || { echo "FAIL setup"; exit 1; }

reset_repo() {
  (cd "$repo" && git reset -q --hard && git clean -qfd)
}

run_helper() {
  (cd "$repo" && bash "$HELPER" gen out) 2>"$ERR"
}

report() {
  local name=$1 want=$2 got=$3
  if [ "$want" = "zero" ] && [ "$got" -eq 0 ]; then
    PASS=$((PASS + 1)); echo "PASS $name"
  elif [ "$want" = "nonzero" ] && [ "$got" -ne 0 ] && [ "$got" -ne 2 ]; then
    PASS=$((PASS + 1)); echo "PASS $name"
  elif [ "$want" = "usage" ] && [ "$got" -eq 2 ]; then
    PASS=$((PASS + 1)); echo "PASS $name"
  else
    FAIL=$((FAIL + 1)); echo "FAIL $name (exit $got)"
    sed 's/^/     | /' "$ERR" | tail -n 10
  fi
}

# 1. Clean tree passes.
reset_repo
run_helper; report "clean tree passes" zero $?

# 2. A changed tracked output fails.
reset_repo
echo changed >"$repo/out/a.txt"
run_helper; report "changed tracked output fails" nonzero $?

# 3. An untracked output fails and is named.
reset_repo
echo new >"$repo/out/new.txt"
run_helper; rc=$?
report "untracked output fails" nonzero $rc
if grep -q "out/new.txt" "$ERR"; then
  PASS=$((PASS + 1)); echo "PASS untracked output is named"
else
  FAIL=$((FAIL + 1)); echo "FAIL untracked output is named"
fi

# 4. A deleted tracked output (stale output) fails and is named.
reset_repo
rm "$repo/out/b.txt"
run_helper; rc=$?
report "deleted tracked output fails" nonzero $rc
if grep -q "out/b.txt" "$ERR"; then
  PASS=$((PASS + 1)); echo "PASS deleted output is named"
else
  FAIL=$((FAIL + 1)); echo "FAIL deleted output is named"
fi

# 5. A staged change passes (index semantics).
reset_repo
echo staged >"$repo/out/a.txt"
(cd "$repo" && git add out/a.txt)
run_helper; report "staged change passes" zero $?

# 6. An untracked file outside the paths passes.
reset_repo
echo other >"$repo/src/other.txt"
run_helper; report "untracked file outside paths passes" zero $?

# 7. No path argument is a usage error.
reset_repo
(cd "$repo" && bash "$HELPER" gen) 2>"$ERR"; report "missing path is usage error" usage $?

echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
