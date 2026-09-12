package controller

import (
	"reflect"
	"testing"
)

// TestBuildStatefulSetHeadlessEnv asserts design D3: buildStatefulSet stamps
// VARROA_HEADLESS=true on the jenkins container only when spec.Headless is
// true, and a non-headless spec renders a byte-identical pod template to the
// output before this field existed.
func TestBuildStatefulSetHeadlessEnv(t *testing.T) {
	base := StatefulSetSpec{
		Name:            "ci-00000000",
		Namespace:       "team-a",
		ControllerName:  "ci",
		VarroaEndpoint:  "example.com:443",
		OIDCIssuer:      "https://issuer.example.com",
		VarroaLoginURL:  "https://login.example.com",
		OIDCUserClaim:   "sub",
		OIDCGroupClaim:  "groups",
		MitePubKeyPEM:   "pubkey",
		MitePubKeyKID:   "kid",
		ApikeyVerifyURL: "https://verify.example.com",
		CAPEM:           "ca",
	}

	// base never sets Headless, so it renders exactly what the field's
	// zero value produces — the same output as before the field existed.
	stsBefore := buildStatefulSet(base)

	nonHeadless := base
	nonHeadless.Headless = false
	stsAfter := buildStatefulSet(nonHeadless)
	if !reflect.DeepEqual(stsBefore, stsAfter) {
		t.Fatal("an explicit spec.Headless: false must render a byte-identical pod template to the unset field")
	}

	env := builtContainerEnv(t, stsAfter, "jenkins")
	if _, ok := env["VARROA_HEADLESS"]; ok {
		t.Error("VARROA_HEADLESS should be absent when spec.Headless is false")
	}

	headless := base
	headless.Headless = true
	stsHeadless := buildStatefulSet(headless)
	envHeadless := builtContainerEnv(t, stsHeadless, "jenkins")
	if got := envHeadless["VARROA_HEADLESS"]; got != "true" {
		t.Errorf("VARROA_HEADLESS = %q, want %q", got, "true")
	}

	// The only allowed difference between the headless and non-headless
	// jenkins-container env is the VARROA_HEADLESS entry itself.
	delete(envHeadless, "VARROA_HEADLESS")
	if !reflect.DeepEqual(env, envHeadless) {
		t.Errorf("flipping Headless must add exactly one env entry; non-headless=%v headless(minus flag)=%v", env, envHeadless)
	}
}
