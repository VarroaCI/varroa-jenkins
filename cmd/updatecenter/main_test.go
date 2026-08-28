package main

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// TestArchiveBaseURL pins the three-state contract: an operator who never sets the
// variable keeps the fallback, one who sets it redirects the fallback, and one who sets
// it empty turns it off. Collapsing "unset" and "set empty" into one case would make
// disabling impossible, since empty is what SetArchiveBaseURL treats as "off".
func TestArchiveBaseURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		set  bool
		want string
	}{
		{
			name: "unset keeps the built-in default",
			set:  false,
			want: ucmeta.DefaultArchiveBaseURL,
		},
		{
			name: "explicit value redirects at an internal mirror",
			raw:  "https://artifacts.internal/maven",
			set:  true,
			want: "https://artifacts.internal/maven",
		},
		{
			name: "explicitly empty disables the fallback",
			raw:  "",
			set:  true,
			want: "",
		},
		{
			name: "whitespace-only disables the fallback",
			raw:  "   ",
			set:  true,
			want: "",
		},
		{
			name: "surrounding whitespace is trimmed",
			raw:  "  https://artifacts.internal/maven\n",
			set:  true,
			want: "https://artifacts.internal/maven",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := archiveBaseURL(tt.raw, tt.set); got != tt.want {
				t.Errorf("archiveBaseURL(%q, %v) = %q, want %q", tt.raw, tt.set, got, tt.want)
			}
		})
	}
}
