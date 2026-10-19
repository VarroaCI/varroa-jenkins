package auth

import "testing"

func TestPrincipalName_Order(t *testing.T) {
	cases := []struct {
		name      string
		preferred string
		email     string
		subject   string
		want      string
	}{
		{"preferred wins", "alice", "alice@example.com", "sub-1", "alice"},
		{"email when no preferred", "", "alice@example.com", "sub-1", "alice@example.com"},
		{"subject last", "", "", "sub-1", "sub-1"},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PrincipalName(tc.preferred, tc.email, tc.subject); got != tc.want {
				t.Errorf("PrincipalName(%q, %q, %q) = %q, want %q", tc.preferred, tc.email, tc.subject, got, tc.want)
			}
		})
	}
}
