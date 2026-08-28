#!/usr/bin/env bash
# Render-assertion test for updateCenter.pullThrough.archiveURL.
#
# The archive checksum fallback has three states, and the template must be able
# to express all of them: an ABSENT variable keeps the server's built-in default,
# a non-empty value redirects the fallback at an internal mirror, and an
# explicitly empty value disables it.
#
# The absent case is the one that needs a guard. `helm upgrade --reuse-values`
# from a release predating this value yields a pullThrough block with no
# archiveURL key (an explicit `--set updateCenter.pullThrough=null` reproduces
# the same shape locally, since a from-scratch `helm template` always coalesces
# the chart's own values.yaml defaults back in for a key that is merely
# *absent*). Rendering the variable unconditionally there would emit an empty
# string — which means DISABLED — silently switching off the fallback on an
# unrelated upgrade.
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
COMMON=(--set auth.oidc.clientSecret=x --set auth.oidc.clientId=x
        --set global.domain=example.com --set bff.oidcClientSecret=x
        --set auth.dashboardUrl=https://example.com
        --set updateCenter.enabled=true
        --set updateCenter.pullThrough.enabled=true)

fail() { echo "FAIL: $*" >&2; exit 1; }

# Prints the env var's value, or the literal "ABSENT" when it is not rendered.
# `// "ABSENT"` cannot do this: an explicitly empty value is a valid result that
# must stay distinguishable from the variable being missing entirely.
uc_archive_url() {
  yq eval-all '
    select(.kind=="Deployment" and .metadata.name=="varroa-updatecenter")
    | .spec.template.spec.containers[0].env
    | map(select(.name=="VARROA_UC_ARCHIVE_BASE_URL")) | .[0].value // "ABSENT"
  ' -
}

# --- Default: chart ships the Jenkins Maven repository ---
default_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" | uc_archive_url)"
[ "$default_url" = "https://repo.jenkins-ci.org/releases" ] \
  || fail "default: expected the Jenkins Maven repository, got: ${default_url:-none}"

# --- Explicit mirror: redirected verbatim ---
mirror_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.pullThrough.archiveURL=https://artifacts.internal/maven | uc_archive_url)"
[ "$mirror_url" = "https://artifacts.internal/maven" ] \
  || fail "mirror: expected the internal mirror URL, got: ${mirror_url:-none}"

# --- Explicit empty: rendered as empty, which the server reads as disabled ---
empty_url="$(helm template t "$CHART_DIR" "${COMMON[@]}" \
  --set updateCenter.pullThrough.archiveURL="" | uc_archive_url)"
[ "$empty_url" = "" ] \
  || fail "explicit empty: expected an empty value (fallback disabled), got: ${empty_url}"

# --- The absent-key case is deliberately NOT asserted here. ---
#
# It cannot be reproduced through `helm template`: helm coalesces the chart's own
# values.yaml defaults back into any partially-supplied map, so archiveURL always
# reappears, and nulling the whole pullThrough block takes `enabled` with it (and
# trips the pre-existing nil dereferences on upstreamURL/downloadURL besides).
# Only `helm upgrade --reuse-values`, which skips new chart defaults, produces the
# shape the template's `hasKey` guard defends against.
#
# What the guard turns into is covered where it is testable: archiveBaseURL's
# unset branch in cmd/updatecenter/main_test.go asserts that a missing variable
# resolves to the built-in default rather than to "disabled".

# --- Pull-through off: the resolver is never built, so the var is moot ---
off_url="$(helm template t "$CHART_DIR" \
  --set auth.oidc.clientSecret=x --set auth.oidc.clientId=x \
  --set global.domain=example.com --set bff.oidcClientSecret=x \
  --set auth.dashboardUrl=https://example.com \
  --set updateCenter.enabled=true | uc_archive_url)"
[ "$off_url" = "ABSENT" ] \
  || fail "pull-through off: expected VARROA_UC_ARCHIVE_BASE_URL to be omitted, got: '${off_url}'"

echo "PASS: updateCenter.pullThrough.archiveURL renders default/mirror/disabled, and is omitted when pull-through is off"
