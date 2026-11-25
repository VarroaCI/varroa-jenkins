package v1alpha1

import "testing"

func TestHeadlessLocationURL(t *testing.T) {
	tests := []struct {
		name                      string
		origin, cluster, ns, ctrl string
		want                      string
	}{
		{"empty origin", "", "core", "team-a", "ci", ""},
		{"empty cluster", "https://varroa.example.com", "", "team-a", "ci", ""},
		{"empty namespace", "https://varroa.example.com", "core", "", "ci", ""},
		{"empty name", "https://varroa.example.com", "core", "team-a", "", ""},
		{"trailing slash trimmed", "https://varroa.example.com/", "core", "team-a", "ci", "https://varroa.example.com/headless/core/team-a/ci/"},
		{"plain origin", "https://varroa.example.com", "core", "team-a", "ci", "https://varroa.example.com/headless/core/team-a/ci/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeadlessLocationURL(tc.origin, tc.cluster, tc.ns, tc.ctrl); got != tc.want {
				t.Errorf("HeadlessLocationURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHeadlessDoorPrefix checks that the door prefix is the start of every
// location URL of the cluster, and that an empty argument answers "".
func TestHeadlessDoorPrefix(t *testing.T) {
	if got := HeadlessDoorPrefix("", "core"); got != "" {
		t.Errorf("HeadlessDoorPrefix with no origin = %q, want empty", got)
	}
	if got := HeadlessDoorPrefix("https://varroa.example.com", ""); got != "" {
		t.Errorf("HeadlessDoorPrefix with no cluster = %q, want empty", got)
	}
	prefix := HeadlessDoorPrefix("https://varroa.example.com/", "core")
	if prefix != "https://varroa.example.com/headless/core/" {
		t.Errorf("HeadlessDoorPrefix = %q", prefix)
	}
	if door := HeadlessLocationURL("https://varroa.example.com/", "core", "cells", "cell-a"); door != prefix+"cells/cell-a/" {
		t.Errorf("HeadlessLocationURL = %q, want it under %q", door, prefix)
	}
}
