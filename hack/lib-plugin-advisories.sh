#!/usr/bin/env bash
# hack/lib-plugin-advisories.sh — helpers that read the security-warning section
# of jenkins-plugin-cli stderr. Sourced by hack/gen-plugin-lock.sh; never executed.
#
# The section goes to stderr when --view-security-warnings is set. Stdout keeps
# only name:version lines. A complete run ends with a Done line on stderr.

ADVISORY_SANDBOX_PLUGINS="script-security pipeline-groovy-lib workflow-cps workflow-support groovy-sandbox"

# advisories_extract <stderr-file>
# Prints the advisory lines of the Security warnings section, in input order.
advisories_extract() {
    awk '
        /^Security warnings:$/ { s = 1; next }
        s && /^[^ ]+ \([^)]*\): SECURITY-[^ ]+ / { print; next }
        s { s = 0 }
    ' "$1"
}

# advisories_complete <stderr-file>
# Returns 0 when the run printed its Done line.
advisories_complete() {
    grep -qx 'Done' "$1"
}

# advisory_key <advisory-line>
# Prints "<plugin> <SECURITY-id>". The id keeps any suffix, such as -1.
advisory_key() {
    printf '%s\n' "$1" | awk '{ print $1, $3 }'
}

# advisory_is_sandbox <advisory-line>
# Returns 0 for a sandbox plugin or a sandbox bypass title.
advisory_is_sandbox() {
    local plugin
    plugin="$(printf '%s\n' "$1" | awk '{ print $1 }')"
    case " ${ADVISORY_SANDBOX_PLUGINS} " in
        *" ${plugin} "*) return 0 ;;
    esac
    if printf '%s\n' "$1" | grep -qi 'sandbox bypass'; then
        return 0
    fi
    return 1
}

# advisories_profile_section <version> <old-stderr-or-empty> <new-stderr>
# Prints the Markdown section for one Jenkins line.
advisories_profile_section() {
    local version="$1" old="$2" new="$3"
    local tmpd n line key
    tmpd="$(mktemp -d)"

    advisories_extract "$new" > "$tmpd/new.txt"
    n="$(wc -l < "$tmpd/new.txt" | tr -d ' ')"

    printf '### Jenkins %s\n\n' "$version"
    if ! advisories_complete "$new"; then
        printf 'Remaining advisories: not known (the check of the resolved set did not complete)\n\n'
        printf 'Advisories this refresh clears:\n'
        printf -- '- not known (the check of the resolved set did not complete)\n\n'
        rm -rf "$tmpd"
        return 0
    fi
    printf 'Remaining advisories (%s):\n' "$n"
    if [ "$n" -eq 0 ]; then
        printf -- '- none\n'
    else
        while IFS= read -r line; do
            if advisory_is_sandbox "$line"; then
                printf -- "- \`%s\` **(sandbox)**\n" "$line"
            else
                printf -- "- \`%s\`\n" "$line"
            fi
        done < "$tmpd/new.txt"
    fi
    printf '\n'

    if [ -z "$old" ]; then
        printf 'Advisories this refresh clears:\n'
        printf -- '- not known (no committed set for this version)\n'
    elif ! advisories_complete "$old"; then
        printf 'Advisories this refresh clears:\n'
        printf -- '- not known (the check of the committed lock did not complete)\n'
    else
        : > "$tmpd/new-keys.txt"
        while IFS= read -r line; do
            advisory_key "$line" >> "$tmpd/new-keys.txt"
        done < "$tmpd/new.txt"
        advisories_extract "$old" > "$tmpd/old.txt"
        : > "$tmpd/cleared.txt"
        while IFS= read -r line; do
            key="$(advisory_key "$line")"
            if ! grep -qxF "$key" "$tmpd/new-keys.txt"; then
                printf '%s\n' "$line" >> "$tmpd/cleared.txt"
            fi
        done < "$tmpd/old.txt"
        n="$(wc -l < "$tmpd/cleared.txt" | tr -d ' ')"
        printf 'Advisories this refresh clears (%s):\n' "$n"
        if [ "$n" -eq 0 ]; then
            printf -- '- none\n'
        else
            while IFS= read -r line; do
                printf -- "- \`%s\`\n" "$line"
            done < "$tmpd/cleared.txt"
        fi
    fi
    printf '\n'

    rm -rf "$tmpd"
}

# advisories_log <version> <label> <stderr-file>
# Writes the advisory count and each line to stderr. Never annotates.
advisories_log() {
    local version="$1" label="$2" file="$3" n
    n="$(advisories_extract "$file" | wc -l | tr -d ' ')"
    echo "  Security warnings for ${version} (${label}): ${n}" >&2
    advisories_extract "$file" | sed 's/^/    /' >&2
}
