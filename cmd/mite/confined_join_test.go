package main

import (
	"path/filepath"
	"testing"
)

func TestConfinedJoin(t *testing.T) {
	base := "/var/casc"
	safe := []string{"jenkins.yaml", "rbac.yaml", "a-b_c.txt"}
	for _, name := range safe {
		got, err := confinedJoin(base, name)
		if err != nil {
			t.Errorf("confinedJoin(%q) unexpected error: %v", name, err)
		}
		if got != filepath.Join(base, name) {
			t.Errorf("confinedJoin(%q) = %q", name, got)
		}
	}

	unsafe := []string{
		"../etc/passwd",
		"..",
		"sub/dir.yaml",
		"/abs/path.yaml",
		"a/../../b",
	}
	for _, name := range unsafe {
		if _, err := confinedJoin(base, name); err == nil {
			t.Errorf("confinedJoin(%q) should have been rejected", name)
		}
	}
}
