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
