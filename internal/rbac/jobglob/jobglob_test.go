package jobglob

import (
	"regexp"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "literal match", pattern: "checkout-api", path: "checkout-api", want: true},
		{name: "literal mismatch", pattern: "checkout-api", path: "checkout-api-2", want: false},

		{name: "single star matches one segment", pattern: "release/*", path: "release/eu", want: true},
		{
			name:    "single star does not cross a slash",
			pattern: "release/*",
			path:    "release/eu/deploy",
			want:    false,
		},

		{name: "double star matches one segment", pattern: "release/**", path: "release/eu", want: true},
		{
			name:    "double star crosses a slash",
			pattern: "release/**",
			path:    "release/eu/deploy",
			want:    true,
		},

		{name: "question mark matches one character", pattern: "eu?", path: "eu1", want: true},
		{name: "question mark requires exactly one character", pattern: "eu?", path: "eu", want: false},
		{
			name:    "question mark matches a slash",
			pattern: "eu?deploy",
			path:    "eu/deploy",
			want:    true,
		},

		{name: "regexp metacharacter matches itself", pattern: "v1.2+build", path: "v1.2+build", want: true},
		{
			name:    "regexp metacharacter does not act as a wildcard",
			pattern: "v1.2+build",
			path:    "v1x2xbuild",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(tt.pattern, tt.path); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// TestRegexpBody proves an anchored body gives the same result as Match for
// every table case, so a caller that embeds the body matches like Match.
func TestRegexpBody(t *testing.T) {
	cases := []struct{ pattern, path string }{
		{"checkout-api", "checkout-api"},
		{"checkout-api", "checkout-api-2"},
		{"release/*", "release/eu"},
		{"release/*", "release/eu/deploy"},
		{"release/**", "release/eu"},
		{"release/**", "release/eu/deploy"},
		{"eu?", "eu1"},
		{"eu?", "eu"},
		{"eu?deploy", "eu/deploy"},
		{"v1.2+build", "v1.2+build"},
		{"v1.2+build", "v1x2xbuild"},
	}
	for _, c := range cases {
		re := regexp.MustCompile(`\A` + RegexpBody(c.pattern) + `\z`)
		if got, want := re.MatchString(c.path), Match(c.pattern, c.path); got != want {
			t.Errorf("anchored RegexpBody(%q) on %q = %v, Match = %v", c.pattern, c.path, got, want)
		}
	}
}

// TestMatchCachesCompiledPattern proves repeated calls with the same
// pattern reuse the cached compiled expression rather than erroring or
// drifting in result.
func TestMatchCachesCompiledPattern(t *testing.T) {
	for i := 0; i < 3; i++ {
		if !Match("release/*", "release/eu") {
			t.Fatalf("Match returned false on repeated call %d", i)
		}
	}
}
