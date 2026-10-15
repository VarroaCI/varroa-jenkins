package main

import "testing"

func TestLocalAuthPlan(t *testing.T) {
	cases := []struct {
		mode          string
		localAccounts bool
		reconcilers   bool
		bootstrap     bool
	}{
		{"local", false, true, true},
		{"oidc", false, false, false},
		// Beside OIDC the reconcilers run, but the admin bootstrap never runs.
		{"oidc", true, true, false},
		{"ldap", false, false, false},
		{"ldap", true, false, false},
	}
	for _, c := range cases {
		rec, boot := localAuthPlan(c.mode, c.localAccounts)
		if rec != c.reconcilers || boot != c.bootstrap {
			t.Errorf("localAuthPlan(%q, %v) = (%v, %v), want (%v, %v)",
				c.mode, c.localAccounts, rec, boot, c.reconcilers, c.bootstrap)
		}
	}
}
