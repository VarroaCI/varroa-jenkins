package pipelinetemplate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a strict MAJOR.MINOR.PATCH semantic version, with no
// prerelease text and no build metadata.
type Version struct {
	Major, Minor, Patch int
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseVersion parses s as a strict MAJOR.MINOR.PATCH version. Prerelease
// text and build metadata are rejected.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version %q must be a strict MAJOR.MINOR.PATCH version with no prerelease and no build metadata", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

// String renders v as MAJOR.MINOR.PATCH.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// CompareVersions returns -1, 0, or 1 as a is less than, equal to, or
// greater than b.
func CompareVersions(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return compareInt(a.Major, b.Major)
	case a.Minor != b.Minor:
		return compareInt(a.Minor, b.Minor)
	default:
		return compareInt(a.Patch, b.Patch)
	}
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// constraintKind names one of the four version constraint forms from D5.
type constraintKind int

const (
	constraintAny constraintKind = iota
	constraintMajor
	constraintMajorMinor
	constraintExact
)

// Constraint is a templateVersion value. It is one of: omitted or "*" for
// any delivered version of a name, "M" for any M.*.*, "M.N" for any M.N.*,
// or "M.N.P" for that version only.
type Constraint struct {
	kind  constraintKind
	major int
	minor int
	exact Version
	raw   string
}

var nonNegativeIntPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// ParseConstraint parses s as one of the four version constraint forms.
func ParseConstraint(s string) (Constraint, error) {
	if s == "" || s == "*" {
		return Constraint{kind: constraintAny, raw: s}, nil
	}
	parts := strings.Split(s, ".")
	switch len(parts) {
	case 1:
		if !nonNegativeIntPattern.MatchString(parts[0]) {
			return Constraint{}, fmt.Errorf("version constraint %q is not a valid major version", s)
		}
		major, _ := strconv.Atoi(parts[0])
		return Constraint{kind: constraintMajor, major: major, raw: s}, nil
	case 2:
		if !nonNegativeIntPattern.MatchString(parts[0]) || !nonNegativeIntPattern.MatchString(parts[1]) {
			return Constraint{}, fmt.Errorf("version constraint %q is not a valid major.minor version", s)
		}
		major, _ := strconv.Atoi(parts[0])
		minor, _ := strconv.Atoi(parts[1])
		return Constraint{kind: constraintMajorMinor, major: major, minor: minor, raw: s}, nil
	case 3:
		v, err := ParseVersion(s)
		if err != nil {
			return Constraint{}, fmt.Errorf("version constraint %q is not a valid version: %w", s, err)
		}
		return Constraint{kind: constraintExact, exact: v, raw: s}, nil
	default:
		return Constraint{}, fmt.Errorf("version constraint %q has too many segments", s)
	}
}

// Matches reports whether v satisfies c.
func (c Constraint) Matches(v Version) bool {
	switch c.kind {
	case constraintAny:
		return true
	case constraintMajor:
		return v.Major == c.major
	case constraintMajorMinor:
		return v.Major == c.major && v.Minor == c.minor
	case constraintExact:
		return v == c.exact
	default:
		return false
	}
}

// String renders c for a message. An omitted constraint renders as "*".
func (c Constraint) String() string {
	if c.raw == "" {
		return "*"
	}
	return c.raw
}
