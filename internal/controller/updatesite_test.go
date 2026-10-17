package controller

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestResolveRuntimeUpdateSiteURL(t *testing.T) {
	explicit := &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{PluginUpdateCenterURL: "https://mirror.example.com/uc.json"},
	}
	tests := []struct {
		name     string
		defaults *v1alpha1.ProvisioningDefaults
		uc       string
		upstream string
		want     string
	}{
		{
			name:     "explicit wins over update center and upstream",
			defaults: explicit,
			uc:       "http://uc.svc:8080",
			upstream: "https://updates.example.com",
			want:     "https://mirror.example.com/uc.json",
		},
		{
			name: "explicit actual.json URL is rewritten to update-center.json",
			defaults: &v1alpha1.ProvisioningDefaults{
				Spec: v1alpha1.ProvisioningDefaultsSpec{PluginUpdateCenterURL: "http://uc.svc:8080/update-center.actual.json"},
			},
			want: "http://uc.svc:8080/update-center.json",
		},
		{
			name: "update center is used when no explicit URL",
			uc:   "http://uc.svc:8080",
			want: "http://uc.svc:8080/update-center.json",
		},
		{
			name:     "trailing slashes are trimmed",
			defaults: &v1alpha1.ProvisioningDefaults{},
			uc:       "http://uc.svc:8080/",
			upstream: "https://updates.example.com/",
			want:     "http://uc.svc:8080/update-center.json",
		},
		{
			name:     "upstream is used when no update center",
			upstream: "https://updates.example.com/",
			want:     "https://updates.example.com/update-center.json",
		},
		{
			name: "no update center and no upstream gives empty",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRuntimeUpdateSiteURL(tt.defaults, tt.uc, tt.upstream); got != tt.want {
				t.Errorf("resolveRuntimeUpdateSiteURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// firstUpdateSites returns jenkins.updateCenter.sites from a resolved Jenkins
// YAML document, and fails the test when the key is missing.
func firstUpdateSites(t *testing.T, doc string) []interface{} {
	t.Helper()
	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatalf("parse resolved jenkins.yaml: %v", err)
	}
	jenkins, _ := got["jenkins"].(map[string]interface{})
	uc, _ := jenkins["updateCenter"].(map[string]interface{})
	sites, ok := uc["sites"].([]interface{})
	if !ok {
		t.Fatalf("jenkins.updateCenter.sites missing in %q", doc)
	}
	return sites
}

func TestResolveBundleForController_InjectsRuntimeUpdateSite(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	sites := firstUpdateSites(t, mat.JenkinsYAML)
	if len(sites) != 1 {
		t.Fatalf("sites = %v, want one entry", sites)
	}
	site, _ := sites[0].(map[string]interface{})
	if site["id"] != "default" || site["url"] != "http://uc.svc:8080/update-center.json" {
		t.Errorf("site = %v, want default at http://uc.svc:8080/update-center.json", site)
	}
}

func TestResolveBundleForController_UpstreamOffNoUCEmptySites(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.SetJenkinsUpstreamURL("")

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	if sites := firstUpdateSites(t, mat.JenkinsYAML); len(sites) != 0 {
		t.Errorf("sites = %v, want empty list", sites)
	}
}

func TestBuildDesiredStateCommand_CarriesRuntimeUpdateSite(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	cmd := mustBuildDesiredStateCommand(t, rec, cr, mat)
	sites := firstUpdateSites(t, cmd.JcascYaml)
	if len(sites) != 1 {
		t.Fatalf("JcascYaml sites = %v, want one entry", sites)
	}
	site, _ := sites[0].(map[string]interface{})
	if site["url"] != "http://uc.svc:8080/update-center.json" {
		t.Errorf("JcascYaml site url = %v", site["url"])
	}
}
