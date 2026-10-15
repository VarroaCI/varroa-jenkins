package main

import "testing"

func TestControllerIssuer(t *testing.T) {
	const (
		bff  = "https://app.example.com"
		oidc = "https://dex.example.com"
	)
	cases := []struct {
		name       string
		mode       string
		oidcIssuer string
		bffURL     string
		want       string
		wantErr    bool
	}{
		{name: "local uses the BFF", mode: "local", bffURL: bff, want: bff},
		{name: "ldap uses the BFF", mode: "ldap", bffURL: bff, want: bff},
		{name: "ldap ignores an OIDC issuer", mode: "ldap", oidcIssuer: oidc, bffURL: bff, want: bff},
		{name: "local without a BFF URL fails", mode: "local", oidcIssuer: oidc, wantErr: true},
		{name: "ldap without a BFF URL fails", mode: "ldap", oidcIssuer: oidc, wantErr: true},
		{name: "oidc uses the OIDC issuer", mode: "oidc", oidcIssuer: oidc, want: oidc},
		{name: "oidc without an issuer is empty", mode: "oidc", want: ""},
		// Local accounts beside OIDC keep the OIDC issuer as the primary one.
		{name: "oidc with a BFF URL keeps the OIDC issuer", mode: "oidc", oidcIssuer: oidc, bffURL: bff, want: oidc},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := controllerIssuer(c.mode, c.oidcIssuer, c.bffURL)
			if c.wantErr {
				if err == nil {
					t.Fatalf("controllerIssuer(%q, %q, %q) = %q, want an error", c.mode, c.oidcIssuer, c.bffURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("controllerIssuer(%q, %q, %q) error: %v", c.mode, c.oidcIssuer, c.bffURL, err)
			}
			if got != c.want {
				t.Errorf("controllerIssuer(%q, %q, %q) = %q, want %q", c.mode, c.oidcIssuer, c.bffURL, got, c.want)
			}
		})
	}
}
