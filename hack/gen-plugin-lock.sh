#!/usr/bin/env bash
# gen-plugin-lock.sh — regenerate internal/controller/pluginlock/lock.yaml
#                      and Helm version-profile templates
#
# Requirements: yq v4, docker, network access to updates.jenkins.io.
#
# The supported version set and per-version catalog metadata live in the
# reviewed manifest hack/version-profiles.yaml (the single source of truth).
# This script parses that manifest with yq, then for each profile entry runs
# jenkins-plugin-cli inside the matching jenkins/jenkins:<resolveVersion> image
# to produce the fully-resolved pinned plugin set. Each set is written to
# lock.yaml keyed by the profile's `version`. Additionally, for each version a
# ConfigMap and JenkinsVersionProfile are emitted under
# charts/varroa/templates/version-profiles/.
#
# Manifest schema (see hack/version-profiles.yaml for the full commentary):
#   baseline:        string, must equal some profiles[].version; written as
#                    lock.yaml `baseline`.
#   profiles[]:
#     version:        required. 2-segment = LTS line, 3-segment = exact pin.
#     channel:        required, one of {lts, weekly}.
#     recommended:    optional bool; at most one profile may be true.
#     eol:            optional YYYY-MM-DD.
#     resolveVersion: image tag plugins are resolved against; REQUIRED for
#                     2-segment LTS *line* profiles (pin to the earliest patch,
#                     e.g. 2.552.1), defaults to `version` for weekly/exact pins.
#
# Usage:
#   hack/gen-plugin-lock.sh                 full regeneration (needs docker + net)
#   hack/gen-plugin-lock.sh --validate-only validate the manifest only, no docker
#
# Core seed plugins are the 7 plugins Varroa pins:
#   configuration-as-code, role-strategy, instance-identity, kubernetes,
#   workflow-aggregator, workflow-cps-global-lib, mcp-server
# The two pipeline-family seeds (workflow-aggregator, workflow-cps-global-lib)
# pull in and pin the full pipeline plugin stack so the operator overrides
# stale bundle-supplied pipeline versions.
# mcp-server serves the Jenkins MCP endpoint (/mcp-server/mcp) consumed by the
# BFF MCP proxy on every controller.
#
# This script is NOT run during reconciliation. Lockfile changes must be
# committed and reviewed as normal source changes (see the
# .github/workflows/plugin-lock-refresh.yaml scheduled workflow).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTFILE="${REPO_ROOT}/internal/controller/pluginlock/lock.yaml"
MANIFEST="${REPO_ROOT}/hack/version-profiles.yaml"
PROFILE_DIR="${REPO_ROOT}/charts/varroa/templates/version-profiles"

CORE_PLUGINS="configuration-as-code role-strategy instance-identity kubernetes workflow-aggregator workflow-cps-global-lib mcp-server"

# --- yq v4 prerequisite ---------------------------------------------------
if ! command -v yq >/dev/null 2>&1; then
    echo "ERROR: yq v4 is required but not found on PATH. Install mikefarah/yq v4." >&2
    exit 1
fi
if ! yq --version 2>&1 | grep -q ' v4\.'; then
    echo "ERROR: yq v4 is required (found: $(yq --version 2>&1)). Install mikefarah/yq v4." >&2
    exit 1
fi

if [ ! -f "$MANIFEST" ]; then
    echo "ERROR: manifest not found: $MANIFEST" >&2
    exit 1
fi

# --- Manifest parsing + validation ---------------------------------------
# Read baseline and the profile rows (version, channel, recommended, eol,
# resolveVersion) via yq -o=tsv with `// ""` defaults so absent fields become
# empty columns.
BASELINE="$(yq eval '.baseline // ""' "$MANIFEST")"
# One pipe-delimited row per profile: version|channel|recommended|eol|resolveVersion.
# A non-whitespace delimiter is required: with a whitespace IFS (tab), bash `read`
# collapses consecutive empty fields, misaligning the columns. Jenkins version
# strings, channels, and dates never contain a literal '|'.
PROFILE_TSV="$(yq eval '.profiles[] | [.version // "", .channel // "", .recommended // "", .eol // "", .resolveVersion // ""] | join("|")' "$MANIFEST")"

# version_lt a b — return 0 (true) if a < b, 1 (false) otherwise.
# Dotted numeric comparison: each segment compared numerically, missing
# segments treated as 0 (e.g. 2.555 < 2.555.3).
version_lt() {
    local a="$1" b="$2"
    local IFS=.
    local a_parts b_parts
    a_parts=($a)
    b_parts=($b)
    local i a_seg b_seg
    for i in 0 1 2 3; do
        a_seg="${a_parts[$i]:-0}"
        b_seg="${b_parts[$i]:-0}"
        if [ "$a_seg" -lt "$b_seg" ]; then
            return 0
        elif [ "$a_seg" -gt "$b_seg" ]; then
            return 1
        fi
    done
    return 1  # equal
}

validate_manifest() {
    if [ -z "$BASELINE" ]; then
        echo "ERROR: manifest baseline is empty" >&2
        exit 1
    fi
    if [ -z "$PROFILE_TSV" ]; then
        echo "ERROR: manifest has no profiles" >&2
        exit 1
    fi

    local baseline_found=0
    local recommended_count=0
    local version channel recommended eol resolveVersion dots
    while IFS='|' read -r version channel recommended eol resolveVersion; do
        [ -z "$version" ] && continue

        if [ "$version" = "$BASELINE" ]; then
            baseline_found=1
        fi

        case "$channel" in
            lts|weekly) ;;
            *)
                echo "ERROR: profile $version has invalid channel '$channel' (must be lts or weekly)" >&2
                exit 1
                ;;
        esac

        if [ "$recommended" = "true" ]; then
            recommended_count=$((recommended_count + 1))
        fi

        # dot-count discriminates 2-segment from 3-segment. A 2-segment LTS
        # profile is a *line* (2.552) and MUST pin resolveVersion to the line's
        # earliest patch (2.552.1). A 2-segment weekly profile (2.570) is an
        # exact weekly release whose own tag exists, so resolveVersion defaults
        # to version; likewise any 3-segment exact pin.
        dots="$(awk -F. '{print NF-1}' <<<"$version")"
        if [ "$dots" -eq 1 ] && [ "$channel" = "lts" ] && [ -z "$resolveVersion" ]; then
            echo "ERROR: 2-segment LTS line $version requires a resolveVersion (pin to the earliest patch, e.g. ${version}.1)" >&2
            exit 1
        fi

        if [ -n "$eol" ] && ! [[ "$eol" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
            echo "ERROR: profile $version has invalid eol '$eol' (must be YYYY-MM-DD)" >&2
            exit 1
        fi
    done <<<"$PROFILE_TSV"

    if [ "$baseline_found" -ne 1 ]; then
        echo "ERROR: baseline '$BASELINE' does not match any profiles[].version" >&2
        exit 1
    fi
    if [ "$recommended_count" -gt 1 ]; then
        echo "ERROR: at most one profile may be recommended (found $recommended_count)" >&2
        exit 1
    fi

    # Guardrail (issue #257): every profile's effective resolve version must be
    # >= plugin/pom.xml's <jenkins.version> floor.  The in-house varroa-mite-auth
    # plugin is baked into every controller pod regardless of profile; a profile
    # whose resolve version sits below the plugin's minimum core requirement can
    # never boot Jenkins.  This runs for both --validate-only and full generation.
    local PLUGIN_FLOOR
    PLUGIN_FLOOR="$(sed -n 's/.*<jenkins\.version>\([^<]*\)<\/jenkins\.version>.*/\1/p' "${REPO_ROOT}/plugin/pom.xml" | head -1)"
    if [ -z "$PLUGIN_FLOOR" ]; then
        echo "ERROR: could not parse <jenkins.version> from plugin/pom.xml" >&2
        exit 1
    fi
    local eff_version
    while IFS='|' read -r version channel recommended eol resolveVersion; do
        [ -z "$version" ] && continue
        eff_version="${resolveVersion:-$version}"
        if version_lt "$eff_version" "$PLUGIN_FLOOR"; then
            echo "ERROR: profile $version effective resolve version $eff_version is below plugin/pom.xml <jenkins.version> floor $PLUGIN_FLOOR — varroa-mite-auth will refuse to load, making this profile unbootable (issue #257)" >&2
            exit 1
        fi
    done <<<"$PROFILE_TSV"
}

validate_manifest

if [ "${1:-}" = "--validate-only" ]; then
    echo "manifest OK"
    exit 0
fi

# --- Full generation (docker + network) ----------------------------------
# TMP_OUTFILE holds the whole lock until every profile has resolved AND passed
# the bootstrap assertion. The old code wrote $OUTFILE up front and appended per
# profile, so a mid-loop failure left a truncated lock behind — which defeats
# the point of an assertion that is supposed to fail WITHOUT writing a lock.
# Created beside $OUTFILE so the final replace is a same-filesystem rename.
TMP_OUTFILE="$(mktemp "${OUTFILE}.XXXXXX")"
# Captures jenkins-plugin-cli's stderr so a failed resolve can be diagnosed.
ERRLOG="$(mktemp)"
trap 'rm -f "$TMP_OUTFILE" "$ERRLOG"' EXIT

# --- Bootstrap closure root ----------------------------------------------
# varroa-mite-auth is baked into the image and copied into every Jenkins pod, so
# it is never a member of the resolved lock — but its mandatory dependency
# closure must be. cmd/bootstrapdeps walks that closure and fails the run if any
# member is missing, so a lock that would break authentication fleet-wide is
# never written.
MITE_AUTH_HPI="${REPO_ROOT}/plugin/target/varroa-mite-auth.hpi"
if [ ! -f "$MITE_AUTH_HPI" ]; then
    echo "  Building varroa-mite-auth HPI (plugin/target/varroa-mite-auth.hpi is absent)..." >&2
    docker run --rm \
        -v "${REPO_ROOT}/plugin:/build" \
        -w /build \
        maven:3.9-eclipse-temurin-21 \
        mvn -B -q -DskipTests package >&2
fi
if [ ! -f "$MITE_AUTH_HPI" ]; then
    echo "ERROR: $MITE_AUTH_HPI not found after build; cannot assert the bootstrap closure." >&2
    exit 1
fi

GEN_DATE="$(date -u +%Y-%m-%d)"

# emit_profile_files writes the ConfigMap and JenkinsVersionProfile YAML files
# for a given version. Arguments:
#   1 — full version string (e.g. "2.552") — used for spec.version and file names
#   2 — path to a file with "artifactId:version" lines (one per plugin)
#   3 — channel (lts/weekly)
#   4 — recommended (true/false)
#   5 — eol (date or empty)
#   6 — resolveVersion (image tag the set was resolved against)
emit_profile_files() {
    local ver="$1"
    local plugins_file="$2"
    local channel="$3"
    local recommended="$4"
    local eol="$5"
    local resolveVersion="$6"

    mkdir -p "$PROFILE_DIR"

    # Sanitize dots to dashes for k8s resource names.
    local safe="${ver//./-}"
    local profile_name="jenkins-version-${safe}"
    local cm_name="${profile_name}-pluginset"
    local header="# generated by hack/gen-plugin-lock.sh from hack/version-profiles.yaml (resolved against ${resolveVersion} on ${GEN_DATE})"

    # Write ConfigMap.
    cat > "${PROFILE_DIR}/${ver}-pluginset-configmap.yaml" <<CMEOF
${header}
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${cm_name}
  namespace: {{ .Release.Namespace }}
data:
  plugins.yaml: |
    core:
$(for p in $CORE_PLUGINS; do echo "      - $p"; done)
    plugins:
$(while IFS=: read -r aid aiv; do
  aid="${aid// /}"
  aiv="${aiv// /}"
  if [ -n "$aid" ] && [ -n "$aiv" ]; then
    echo "      - artifactId: $aid"
    # Quoted: a two-segment pin like 1.24 is a YAML float unquoted, and yaml.v3
    # refuses to decode a float into a string field.
    echo "        version: \"$aiv\""
  fi
done < "$plugins_file")
CMEOF

    # Write JenkinsVersionProfile.
    cat > "${PROFILE_DIR}/${ver}-profile.yaml" <<PROFEOF
${header}
apiVersion: varroa.dev/v1alpha1
kind: JenkinsVersionProfile
metadata:
  name: ${profile_name}
  labels:
    app.kubernetes.io/managed-by: varroa-operator
spec:
  version: "${ver}"
  channel: ${channel}
  recommended: ${recommended:-false}
$(if [ -n "$eol" ]; then echo "  eol: \"${eol}\""; fi)
$(if [ -n "$resolveVersion" ] && [ "$resolveVersion" != "$ver" ]; then echo "  resolveVersion: \"${resolveVersion}\""; fi)
  pluginSetRef:
    name: ${cm_name}
PROFEOF

    echo "  Wrote ${PROFILE_DIR}/${ver}-pluginset-configmap.yaml" >&2
    echo "  Wrote ${PROFILE_DIR}/${ver}-profile.yaml" >&2
}

# Wipe stale templates so versions removed from the manifest leave no artifacts.
rm -f "${PROFILE_DIR}/"*.yaml

# lock.yaml header: baseline from the manifest key. Written to a temp file and
# moved into place only once every profile has resolved and asserted clean.
cat > "$TMP_OUTFILE" <<EOF
baseline: "${BASELINE}"
sets:
EOF

while IFS='|' read -r version channel recommended eol resolveVersion; do
    [ -z "$version" ] && continue

    # Default resolveVersion to `version` for 3-segment exact pins.
    if [ -z "$resolveVersion" ]; then
        resolveVersion="$version"
    fi

    echo "  Resolving plugins for Jenkins ${version} (image ${resolveVersion})..." >&2
    OUTPUT="$(mktemp)"
    # NOTE: `jenkins-plugin-cli --list` resolves the full transitive plugin set with
    # pinned versions but exits non-zero even on success, so we swallow its exit code
    # (`|| true`). It can also intermittently emit non-empty but MALFORMED output on a
    # transient update-center hiccup (empty or space-separated instead of `name:version`).
    # A mere non-empty check let that through and the IFS=: parse below silently dropped
    # every plugin, producing an empty-plugin-set regression PR (#270). So we validate the
    # SHAPE of the result — well-formed `name:version` lines plus every core seed present —
    # and retry a few times before giving up (#188 — do NOT switch to --available-updates).
    #
    # The seed list is passed as `--plugins` ARGUMENTS, deliberately NOT as a
    # bind-mounted `--plugin-file`. A mounted file inherits the host uid/mode it
    # was created with; `mktemp` yields 0600 owned by the invoking user, and the
    # jenkins image runs as uid 1000, so on any host whose user is not uid 1000
    # (uid 1001 on both GitHub-hosted and ARC runners) the container could not
    # read it. jenkins-plugin-cli treats an unreadable --plugin-file as an EMPTY
    # plugin list and still exits 0, so every CI run resolved 0 plugins while
    # passing on developer machines that happen to be uid 1000. Arguments carry
    # no filesystem permissions and cannot regress that way.
    core_count=$(wc -w <<<"$CORE_PLUGINS" | tr -d ' ')
    attempts=0
    max_attempts=3
    while :; do
        attempts=$((attempts + 1))
        : > "$ERRLOG"
        docker run --rm \
            "jenkins/jenkins:${resolveVersion}-alpine-jdk21" \
            sh -c "
                jenkins-plugin-cli --plugins $CORE_PLUGINS --list --no-download --output txt || true
            " 2>"$ERRLOG" | grep -v '^#' | sort > "$OUTPUT" || true

        # Count strictly well-formed `name:version` lines (exactly one colon, no
        # surrounding whitespace). LC_ALL=C keeps the character classes deterministic;
        # the version-field class excludes ':' so stray-colon lines can't mis-split.
        valid=$(LC_ALL=C grep -cE '^[^:[:space:]]+:[^:[:space:]]+$' "$OUTPUT" || true)
        # Every core seed must actually resolve — proves we got the requested set, not junk.
        missing=""
        for p in $CORE_PLUGINS; do
            grep -q "^${p}:" "$OUTPUT" || missing="${missing:+$missing }$p"
        done

        if [ "$valid" -ge "$core_count" ] && [ -z "$missing" ]; then
            break
        fi
        if [ "$attempts" -ge "$max_attempts" ]; then
            echo "ERROR: failed to resolve a valid plugin set for ${version} after ${attempts} attempts (image jenkins/jenkins:${resolveVersion}-alpine-jdk21 may not exist, network/update-center unavailable, or plugin-cli returned malformed output: ${valid} well-formed name:version lines, need >= ${core_count}; missing seeds: ${missing:-none})" >&2
            echo "--- jenkins-plugin-cli stderr (last 40 lines) ---" >&2
            tail -40 "$ERRLOG" >&2 || true
            echo "--- resolve stdout (last 20 lines) ---" >&2
            tail -20 "$OUTPUT" >&2 || true
            echo "-------------------------------------------------" >&2
            rm -f "$OUTPUT"
            exit 1
        fi
        echo "  WARN: resolve for ${version} produced ${valid} valid lines (need >= ${core_count}; missing seeds: ${missing:-none}); retrying (${attempts}/${max_attempts})..." >&2
        sleep 5
    done

    # lock.yaml set keyed by `version` (not resolveVersion).
    echo "  ${version}:" >> "$TMP_OUTFILE"
    echo "    core:" >> "$TMP_OUTFILE"
    for p in $CORE_PLUGINS; do
        echo "      - $p" >> "$TMP_OUTFILE"
    done
    echo "    plugins:" >> "$TMP_OUTFILE"
    while IFS=: read -r artifactId version_pin; do
        artifactId="${artifactId// /}"
        version_pin="${version_pin// /}"
        if [ -n "$artifactId" ] && [ -n "$version_pin" ]; then
            echo "      - artifactId: $artifactId" >> "$TMP_OUTFILE"
            # Quoted: a two-segment pin like 1.24 is a YAML float unquoted, and
            # yaml.v3 refuses to decode a float into a string field.
            echo "        version: \"$version_pin\"" >> "$TMP_OUTFILE"
        fi
    done < "$OUTPUT"

    # Assert varroa-mite-auth's mandatory dependency closure against THIS set and
    # record it. A missing member exits non-zero here, before $OUTFILE is
    # touched, so the lock that would break authentication is never written.
    echo "  Asserting varroa-mite-auth bootstrap closure for ${version}..." >&2
    if ! (cd "$REPO_ROOT" && go run ./cmd/bootstrapdeps --resolve \
            --hpi "$MITE_AUTH_HPI" \
            --plugins "$OUTPUT" \
            --indent 4) >> "$TMP_OUTFILE"; then
        echo "ERROR: bootstrap closure assertion failed for ${version}; no lock file written." >&2
        rm -f "$OUTPUT"
        exit 1
    fi

    # Emit Helm template files for this version.
    emit_profile_files "$version" "$OUTPUT" "$channel" "$recommended" "$eol" "$resolveVersion"

    rm -f "$OUTPUT"
done <<<"$PROFILE_TSV"

# Every profile resolved and asserted clean — publish the lock atomically.
chmod 644 "$TMP_OUTFILE"
mv "$TMP_OUTFILE" "$OUTFILE"
echo "  Wrote $OUTFILE" >&2
