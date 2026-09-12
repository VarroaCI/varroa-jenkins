package recordstore

import (
	"regexp"
	"strings"
)

// matchExportEntries finds every entry of exports whose glob matches
// resource for kind, so the grant orders can take the union of every
// matching entry's callers rather than only the first one. A job
// resource is made relative to target's own folder before it is
// matched; a lock resource arrives already relative to target's own
// prefix. It answers ok=false when no entry matches, or when a job
// resource does not carry target's folder prefix at all.
func matchExportEntries(exports []exportEntry, kind GrantResourceKind, resource, target string) ([]exportEntry, bool) {
	var matches []exportEntry
	switch kind {
	case GrantResourceJob:
		relative, ok := stripTenantPrefix(target, resource)
		if !ok {
			return nil, false
		}
		for _, e := range exports {
			if e.Job != "" && matchGlob(e.Job, relative) {
				matches = append(matches, e)
			}
		}
	case GrantResourceLock:
		for _, e := range exports {
			for _, glob := range e.Locks {
				if matchGlob(glob, resource) {
					matches = append(matches, e)
					break
				}
			}
		}
	}
	return matches, len(matches) > 0
}

// stripTenantPrefix removes tenant's folder prefix, "<tenant>/", from
// qualified. It answers ok=false when qualified does not carry that
// prefix.
func stripTenantPrefix(tenant, qualified string) (string, bool) {
	return strings.CutPrefix(qualified, tenant+"/")
}

// matchGlob reports whether s matches pattern under the
// tenant-export-grant capability's three metacharacters: "*" matches any
// run of characters except "/", "**" matches across "/", and "?" matches
// one character. The match is anchored at both ends. A pattern the CRD
// already validated always compiles; a pattern that somehow does not
// answers no match rather than a panic.
func matchGlob(pattern, s string) bool {
	re, err := regexp.Compile(globToRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// globToRegexp compiles one export glob into an anchored regular
// expression. "**" is read before a lone "*", so a doubled star never
// falls through to the single-star case.
func globToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); {
		switch {
		case pattern[i] == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteByte('$')
	return b.String()
}
