package recordstore

import "testing"

// TestResolveLockTarget asserts one lock call resolves to its owner
// tenant and bare name from the separate tenant field. The name is never
// split, so a controller tenant that holds a "/" of its own resolves
// whole.
func TestResolveLockTarget(t *testing.T) {
	cases := []struct {
		name        string
		req         CrossTenantRequest
		wantOwner   string
		wantBare    string
		wantCrosses bool
		wantVeto    bool
	}{
		{
			name:      "cell tenant, own lock",
			req:       CrossTenantRequest{CallerTenant: "alpha", Name: "deploy-prod"},
			wantOwner: "alpha",
			wantBare:  "deploy-prod",
		},
		{
			name:      "controller tenant, empty target",
			req:       CrossTenantRequest{CallerTenant: "team-pay/ci", Name: "deploy-prod"},
			wantOwner: "team-pay/ci",
			wantBare:  "deploy-prod",
		},
		{
			name:      "target equal to the caller",
			req:       CrossTenantRequest{CallerTenant: "team-pay/ci", TargetTenant: "team-pay/ci", Name: "deploy-prod"},
			wantOwner: "team-pay/ci",
			wantBare:  "deploy-prod",
		},
		{
			name:        "cross-tenant to a cell tenant",
			req:         CrossTenantRequest{CallerTenant: "alpha", TargetTenant: "beta", Name: "deploy-prod"},
			wantOwner:   "beta",
			wantBare:    "deploy-prod",
			wantCrosses: true,
		},
		{
			name:        "cross-tenant to a controller tenant",
			req:         CrossTenantRequest{CallerTenant: "alpha", TargetTenant: "team-pay/ci", Name: "deploy-prod"},
			wantOwner:   "team-pay/ci",
			wantBare:    "deploy-prod",
			wantCrosses: true,
		},
		{
			name:     "slash in the name",
			req:      CrossTenantRequest{CallerTenant: "alpha", Name: "beta/deploy-prod"},
			wantVeto: true,
		},
		{
			name:     "slash in the name with a target",
			req:      CrossTenantRequest{CallerTenant: "alpha", TargetTenant: "beta", Name: "x/deploy-prod"},
			wantVeto: true,
		},
		{
			name:     "empty name",
			req:      CrossTenantRequest{CallerTenant: "alpha"},
			wantVeto: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, bare, crosses, veto := resolveLockTarget(tc.req)
			if tc.wantVeto {
				if veto == nil {
					t.Fatalf("want a veto, got owner %q bare %q", owner, bare)
				}
				if veto.ReasonCode != ReasonInvalidName || veto.Message == "" || veto.Code != "" {
					t.Fatalf("veto = %+v, want reason %q, a message, and no code", *veto, ReasonInvalidName)
				}
				return
			}
			if veto != nil {
				t.Fatalf("unexpected veto: %+v", *veto)
			}
			if owner != tc.wantOwner || bare != tc.wantBare || crosses != tc.wantCrosses {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", owner, bare, crosses,
					tc.wantOwner, tc.wantBare, tc.wantCrosses)
			}
			if got := qualifiedLockName(owner, bare); got != tc.wantOwner+"/"+tc.wantBare {
				t.Fatalf("qualifiedLockName = %q", got)
			}
		})
	}
}
