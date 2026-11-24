package controller

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// injectedLocationURL resolves the bundle of cr and returns the location URL
// that the operator injects into its jenkins.yaml.
func injectedLocationURL(t *testing.T, rec *Reconciler, cr *v1alpha1.Controller) string {
	t.Helper()
	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &got); err != nil {
		t.Fatalf("parse resolved jenkins.yaml: %v", err)
	}
	unclassified, _ := got["unclassified"].(map[string]interface{})
	location, _ := unclassified["location"].(map[string]interface{})
	url, _ := location["url"].(string)
	return url
}

// TestJenkinsLocationURLMatchesTheInjectedURL checks that the location URL
// that a commit-status restore compares is the one the operator injects, for
// a cell and for a headless controller, with and without a dashboard origin.
func TestJenkinsLocationURLMatchesTheInjectedURL(t *testing.T) {
	cases := []struct {
		name     string
		origin   bool
		cell     bool
		wantDoor bool
	}{
		{name: "cell with a dashboard origin", origin: true, cell: true, wantDoor: true},
		{name: "headless controller with a dashboard origin", origin: true, wantDoor: true},
		{name: "cell with no dashboard origin", cell: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClientWithBundle()
			rec := newTestReconciler(client)
			if tc.origin {
				rec.SetVarroaRedirectURL("https://varroa.example.com/callback")
				rec.SetClusterName("core")
			}
			cr := testController("test", "ns1", v1alpha1.ControllerPhaseRunning)
			if tc.cell {
				cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
			} else {
				cr.Spec.Headless = true
			}
			crdstore.MustSeed(client.store, cr)

			got := rec.JenkinsLocationURL(context.Background(), "ns1", "test")
			if got == "" || got != injectedLocationURL(t, rec, cr) {
				t.Fatalf("JenkinsLocationURL = %q, want the injected URL %q", got, injectedLocationURL(t, rec, cr))
			}
			if door := got == "https://varroa.example.com/headless/core/ns1/test/"; door != tc.wantDoor {
				t.Fatalf("JenkinsLocationURL = %q, door form = %v, want %v", got, door, tc.wantDoor)
			}
		})
	}
}

// TestJenkinsLocationURLRefusesWhatItCannotName checks the empty answer for a
// controller that is not headless and for a controller that does not exist.
func TestJenkinsLocationURLRefusesWhatItCannotName(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.SetVarroaRedirectURL("https://varroa.example.com/callback")
	rec.SetClusterName("core")
	cr := testController("classic", "ns1", v1alpha1.ControllerPhaseRunning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "ci.example.com"}
	crdstore.MustSeed(client.store, cr)

	if got := rec.JenkinsLocationURL(context.Background(), "ns1", "classic"); got != "" {
		t.Fatalf("JenkinsLocationURL of a classic controller = %q, want empty", got)
	}
	if got := rec.JenkinsLocationURL(context.Background(), "ns1", "missing"); got != "" {
		t.Fatalf("JenkinsLocationURL of a missing controller = %q, want empty", got)
	}
}
