#!/usr/bin/env bash
# hack/lib-plugin-advisories_test.sh — tests for hack/lib-plugin-advisories.sh.
# Each case feeds inline jenkins-plugin-cli stderr fixtures to the helpers.
#
# Run: bash hack/lib-plugin-advisories_test.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=hack/lib-plugin-advisories.sh
. "$HERE/lib-plugin-advisories.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "PASS $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; }

# Old committed-lock stderr: a listing, the ten advisories, and Done.
cat >"$WORK/old.err" <<'EOF'
Resolving plugins
workflow-multibranch 841.vec5b_9e1806ec
script-security 1415.v9a_f9b_3a_c253d
All requested plugins: 125
Security warnings:
workflow-multibranch (841.vec5b_9e1806ec): SECURITY-3729 Exposure of System-scoped credentials https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3729
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3897 Classpath approval bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3897
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3923 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3923
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3925-1 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3925%20(1)
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3925-2 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3925%20(2)
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3929 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3929
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3931 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3931
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3932 TOCTOU race condition https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3932
script-security (1415.v9a_f9b_3a_c253d): SECURITY-3977 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3977
pipeline-groovy-lib (805.va_fc79344957d): SECURITY-3796 Path traversal vulnerability https://www.jenkins.io/security/advisory/2026-09-16/#SECURITY-3796
Done
EOF

# Clean run: listing and Done, no section.
cat >"$WORK/clean.err" <<'EOF'
workflow-multibranch 841.vec5b_9e1806ec
All requested plugins: 125
Done
EOF

# Failed run: update-center 403, no Done.
cat >"$WORK/403.err" <<'EOF'
Error getting update center json
EOF

# New resolve with one advisory on a sandbox plugin.
cat >"$WORK/new.err" <<'EOF'
Security warnings:
script-security (1436.x): SECURITY-3923 Sandbox bypass vulnerability https://www.jenkins.io/security/advisory/2026-10-01/#SECURITY-3923
Done
EOF

# A non-matching line ends the section before the Done line.
cat >"$WORK/stop.err" <<'EOF'
Security warnings:
workflow-multibranch (841.vec5b_9e1806ec): SECURITY-3729 Exposure u
this line does not match
pipeline-groovy-lib (805.va_fc79344957d): SECURITY-3796 Path traversal u
Done
EOF

# 1. Clean run yields no advisory lines.
out="$(advisories_extract "$WORK/clean.err")"
if [ -z "$out" ]; then pass extract_clean; else fail extract_clean; fi

# 2. Old fixture yields exactly ten advisory lines.
n="$(advisories_extract "$WORK/old.err" | wc -l | tr -d ' ')"
if [ "$n" -eq 10 ]; then pass extract_old; else fail "extract_old (got $n)"; fi

# 3. Extraction stops at the first non-matching line, and Done is never printed.
out="$(advisories_extract "$WORK/stop.err")"
n="$(printf '%s\n' "$out" | grep -c .)"
if [ "$n" -eq 1 ] && ! printf '%s\n' "$out" | grep -qx 'Done'; then
    pass extract_stops_at_nonmatching
else
    fail "extract_stops_at_nonmatching (got $n lines)"
fi

# 4. Done marks a complete run.
if advisories_complete "$WORK/old.err"; then pass complete_true; else fail complete_true; fi

# 5. A 403 run is not complete.
if advisories_complete "$WORK/403.err"; then fail complete_false_on_403; else pass complete_false_on_403; fi

# 6. The key keeps the suffix and drops the version.
line="$(advisories_extract "$WORK/old.err" | grep 'SECURITY-3925-1')"
key="$(advisory_key "$line")"
if [ "$key" = "script-security SECURITY-3925-1" ]; then pass key_strips_version; else fail "key_strips_version ($key)"; fi

# 7. A sandbox plugin counts as sandbox.
line="$(advisories_extract "$WORK/old.err" | grep 'pipeline-groovy-lib')"
if advisory_is_sandbox "$line"; then pass sandbox_by_plugin; else fail sandbox_by_plugin; fi

# 8. A sandbox bypass title counts as sandbox, whatever the plugin.
if advisory_is_sandbox 'foo (1.0): SECURITY-1 Sandbox bypass vulnerability u'; then
    pass sandbox_by_title
else
    fail sandbox_by_title
fi

# 9. A non-sandbox plugin with no sandbox title is not sandbox.
line="$(advisories_extract "$WORK/old.err" | grep 'workflow-multibranch')"
if advisory_is_sandbox "$line"; then fail not_sandbox; else pass not_sandbox; fi

# 10. The section lists the remaining advisory and the nine cleared ones.
out="$(advisories_profile_section 2.570 "$WORK/old.err" "$WORK/new.err")"
if printf '%s\n' "$out" | grep -qF 'Remaining advisories (1):' \
    && printf '%s\n' "$out" | grep -qF '(sandbox)' \
    && printf '%s\n' "$out" | grep -qF 'Advisories this refresh clears (9):'; then
    pass section_cleared_and_remaining
else
    fail section_cleared_and_remaining
    printf '%s\n' "$out" | sed 's/^/     | /'
fi

# 11. An incomplete committed-lock check is reported as not known.
out="$(advisories_profile_section 2.570 "$WORK/403.err" "$WORK/new.err")"
if printf '%s\n' "$out" | grep -qF 'not known (the check of the committed lock did not complete)'; then
    pass section_old_incomplete
else
    fail section_old_incomplete
fi

# 12. A version with no committed set is reported as not known.
out="$(advisories_profile_section 2.999 "" "$WORK/new.err")"
if printf '%s\n' "$out" | grep -qF 'not known (no committed set'; then
    pass section_new_profile
else
    fail section_new_profile
fi

# 13. An incomplete resolved-set check prints not known and no count or cleared list.
out="$(advisories_profile_section 2.570 "$WORK/old.err" "$WORK/403.err")"
if printf '%s\n' "$out" | grep -qF 'Remaining advisories: not known (the check of the resolved set did not complete)' \
    && ! printf '%s\n' "$out" | grep -qF 'Remaining advisories (' \
    && ! printf '%s\n' "$out" | grep -qF 'Advisories this refresh clears ('; then
    pass section_new_incomplete
else
    fail section_new_incomplete
    printf '%s\n' "$out" | sed 's/^/     | /'
fi

echo "PASS: $PASS  FAIL: $FAIL"
[ "$FAIL" -eq 0 ]
