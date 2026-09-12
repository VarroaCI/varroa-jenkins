package org.jenkinsci.plugins.varroamiteauth.template;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * A strict {@code MAJOR.MINOR.PATCH} version and the four version
 * constraint forms from design D5. Mirrors
 * {@code internal/bundle/pipelinetemplate/version.go} so the plugin
 * resolves a marker's {@code templateVersion} the same way the operator
 * resolves a bundle's {@code pipeline-template} inputs.
 */
final class TemplateVersion implements Comparable<TemplateVersion> {

    private static final Pattern VERSION_PATTERN =
            Pattern.compile("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$");
    private static final Pattern NON_NEGATIVE_INT_PATTERN = Pattern.compile("^(0|[1-9][0-9]*)$");

    final int major;
    final int minor;
    final int patch;

    private TemplateVersion(int major, int minor, int patch) {
        this.major = major;
        this.minor = minor;
        this.patch = patch;
    }

    /** Parses {@code s} as a strict MAJOR.MINOR.PATCH version: no prerelease, no build metadata. */
    static TemplateVersion parse(String s) {
        Matcher m = VERSION_PATTERN.matcher(s);
        if (!m.matches()) {
            throw new IllegalArgumentException(
                    "version \"" + s + "\" must be a strict MAJOR.MINOR.PATCH version with no prerelease and no build metadata");
        }
        return new TemplateVersion(Integer.parseInt(m.group(1)), Integer.parseInt(m.group(2)), Integer.parseInt(m.group(3)));
    }

    @Override
    public int compareTo(TemplateVersion o) {
        if (major != o.major) {
            return Integer.compare(major, o.major);
        }
        if (minor != o.minor) {
            return Integer.compare(minor, o.minor);
        }
        return Integer.compare(patch, o.patch);
    }

    @Override
    public String toString() {
        return major + "." + minor + "." + patch;
    }

    private enum Kind { ANY, MAJOR, MAJOR_MINOR, EXACT }

    /**
     * A {@code templateVersion} constraint: omitted or {@code "*"} matches
     * any delivered version of a name, {@code "M"} matches any
     * {@code M.*.*}, {@code "M.N"} matches any {@code M.N.*}, and
     * {@code "M.N.P"} matches that version only.
     */
    static final class Constraint {
        private final Kind kind;
        private final int major;
        private final int minor;
        private final TemplateVersion exact;
        private final String raw;

        private Constraint(Kind kind, int major, int minor, TemplateVersion exact, String raw) {
            this.kind = kind;
            this.major = major;
            this.minor = minor;
            this.exact = exact;
            this.raw = raw;
        }

        boolean matches(TemplateVersion v) {
            switch (kind) {
                case ANY:
                    return true;
                case MAJOR:
                    return v.major == major;
                case MAJOR_MINOR:
                    return v.major == major && v.minor == minor;
                case EXACT:
                    return v.compareTo(exact) == 0;
                default:
                    return false;
            }
        }

        @Override
        public String toString() {
            return raw.isEmpty() ? "*" : raw;
        }
    }

    /** Parses {@code s} as one of the four version constraint forms from D5. */
    static Constraint parseConstraint(String s) {
        if (s.isEmpty() || "*".equals(s)) {
            return new Constraint(Kind.ANY, 0, 0, null, s);
        }
        String[] parts = s.split("\\.", -1);
        switch (parts.length) {
            case 1:
                if (!NON_NEGATIVE_INT_PATTERN.matcher(parts[0]).matches()) {
                    throw new IllegalArgumentException("version constraint \"" + s + "\" is not a valid major version");
                }
                return new Constraint(Kind.MAJOR, Integer.parseInt(parts[0]), 0, null, s);
            case 2:
                if (!NON_NEGATIVE_INT_PATTERN.matcher(parts[0]).matches()
                        || !NON_NEGATIVE_INT_PATTERN.matcher(parts[1]).matches()) {
                    throw new IllegalArgumentException("version constraint \"" + s + "\" is not a valid major.minor version");
                }
                return new Constraint(Kind.MAJOR_MINOR, Integer.parseInt(parts[0]), Integer.parseInt(parts[1]), null, s);
            case 3:
                TemplateVersion v;
                try {
                    v = parse(s);
                } catch (IllegalArgumentException e) {
                    throw new IllegalArgumentException("version constraint \"" + s + "\" is not a valid version: " + e.getMessage());
                }
                return new Constraint(Kind.EXACT, 0, 0, v, s);
            default:
                throw new IllegalArgumentException("version constraint \"" + s + "\" has too many segments");
        }
    }
}
