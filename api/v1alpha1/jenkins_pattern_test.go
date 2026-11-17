package v1alpha1

import (
	"strings"
	"testing"
)

func TestCheckJavaPattern_AcceptsWhatJavaCompiles(t *testing.T) {
	patterns := []string{
		`payments/.*-production`, `^challenge-freestyle-.*`, `tenant-a/.*/main`, `tenant-*`, `.*`, `\.\*`,
		`(?=a)b`, `(?!secret/).*`, `(?<=a)b`, `(a)\1`, `(?<n>a)\k<n>`, `a*+`, `a++`, `a?+`, `(?>a)`,
		`[a-z&&[^b]]`, `[[a]]`, `[](]`, `[^]a]`, `[a]]`, `[&&a]`, `[a&&]`, `[{a}]`, `[(]`,
		`[[:alpha:]]+`, `\p{javaLowerCase}`, `\p{L}+`, `\x{41}`, `\N{LATIN SMALL LETTER A}`,
		`\b{g}`, `\b{g}*`, `\cA`, `(?i)tenant-a/.*`, `(?s-i)a`, `(?)`, `(?i:a)`, `(?U)\w+`,
		`(?x)a # (`, `\Q(\E`, `\Qa\`, `{2}`, `^{2}`, `a|{2}`, `a{1,2}{3}`, `a{2}?`, `a{2}+`,
		`^*`, `$*`, `()*`, `(a|)`, `a}`, `}`, `\{a}`, `\)`, `\(?P`, `payments/(?:api|web)/.*`,
		`^(alpha|beta)/.*$`, `.*-(prod|stage)$`,
	}
	for _, p := range patterns {
		if got := checkJavaPattern(p); got != "" {
			t.Errorf("checkJavaPattern(%q) = %q, want accepted", p, got)
		}
	}
}

func TestCheckJavaPattern_RefusesWhatJavaRefuses(t *testing.T) {
	cases := []struct {
		pattern string
		reason  string
	}{
		{`(foo`, "unclosed group"},
		{`((a)`, "unclosed group"},
		{`\\(`, "unclosed group"},
		{`foo)`, "unmatched closing ')'"},
		{`(a))`, "unmatched closing ')'"},
		{`[abc`, "unclosed character class"},
		{`[]`, "unclosed character class"},
		{`*foo`, "dangling meta character '*'"},
		{`*`, "dangling meta character '*'"},
		{`**/foo`, "dangling meta character '*'"},
		{`a|*b`, "dangling meta character '*'"},
		{`(*a)`, "dangling meta character '*'"},
		{`(|*)`, "dangling meta character '*'"},
		{`a**`, "dangling meta character '*'"},
		{`a{2}+*`, "dangling meta character '*'"},
		{`?`, "'?'"},
		{`+x`, "'+'"},
		{`a*?+`, "'+'"},
		{`foo\`, "trailing backslash"},
		{`{a}`, "illegal repetition"},
		{`a{b}`, "illegal repetition"},
		{`job-{a,b}`, "illegal repetition"},
		{`x{,3}`, "illegal repetition"},
		{`{`, "illegal repetition"},
		{`({a})`, "illegal repetition"},
		{`^{a}`, "illegal repetition"},
		{`(?P<n>a)`, "unknown inline modifier"},
		{`a(?#comment)`, "unknown inline modifier"},
	}
	for _, tc := range cases {
		got := checkJavaPattern(tc.pattern)
		if !strings.Contains(got, tc.reason) {
			t.Errorf("checkJavaPattern(%q) = %q, want a reason containing %q", tc.pattern, got, tc.reason)
		}
	}
}

// TestCheckJavaPattern_LeavesUnprovenPatternsToJenkins pins the conservative
// edge. Java refuses these patterns, and the check accepts them, so Jenkins
// stays the final judge for them.
func TestCheckJavaPattern_LeavesUnprovenPatternsToJenkins(t *testing.T) {
	for _, p := range []string{`a{2`, `\c`, `[a-\d]`, `(?<name)`, `[&&]a]`, `a{2}*`, `(?i)*`, `(?:*a)`, `\Q\E*`, `(\Qabc`} {
		if got := checkJavaPattern(p); got != "" {
			t.Errorf("checkJavaPattern(%q) = %q, want accepted (left to Jenkins)", p, got)
		}
	}
}
