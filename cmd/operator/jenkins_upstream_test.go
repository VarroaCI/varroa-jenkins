package main

import "testing"

// TestJenkinsUpstreamURL_AbsentEmptyValue pins the three states of
// VARROA_JENKINS_UPSTREAM_URL: an absent variable keeps the built-in host, an
// empty variable turns the upstream reads off, and a set host is used with its
// trailing slashes removed.
func TestJenkinsUpstreamURL_AbsentEmptyValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  string
	}{
		{name: "absent keeps default", set: false, want: "https://updates.jenkins.io"},
		{name: "empty turns off", value: "", set: true, want: ""},
		{name: "blank turns off", value: "   ", set: true, want: ""},
		{name: "mirror used verbatim", value: "https://mirror.internal/jenkins", set: true, want: "https://mirror.internal/jenkins"},
		{name: "trailing slash removed", value: "https://mirror.internal/jenkins//", set: true, want: "https://mirror.internal/jenkins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				if key != "VARROA_JENKINS_UPSTREAM_URL" {
					t.Fatalf("unexpected lookup of %q", key)
				}
				return tc.value, tc.set
			}
			if got := jenkinsUpstreamURL(lookup); got != tc.want {
				t.Errorf("jenkinsUpstreamURL = %q, want %q", got, tc.want)
			}
		})
	}
}
