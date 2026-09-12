package controller

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellControllerFixture returns a cell Controller with no tenants placed.
func cellControllerFixture() *v1alpha1.Controller {
	cr := testController("cell1", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	return cr
}

// TestApplyCellContent_MergesCellDocumentAndKeepsBundleCloud is task 8.8's
// acceptance: the cell JCasC document's two keys land on the merged
// jenkins.yaml, and a cloud the bundle already declared survives the merge.
func TestApplyCellContent_MergesCellDocumentAndKeepsBundleCloud(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	cr := cellControllerFixture()
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  clouds:\n    - kubernetes:\n        name: bundle-cloud\n"}

	if err := rec.applyCellContent(context.Background(), cr, mat, "http://cell1-svc.ns1.svc.cluster.local:8080"); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &doc); err != nil {
		t.Fatalf("unmarshal merged jcasc: %v", err)
	}

	jenkins, _ := doc["jenkins"].(map[string]any)
	if jenkins == nil {
		t.Fatalf("merged jcasc has no jenkins key: %s", mat.JenkinsYAML)
	}
	if numExec, ok := jenkins["numExecutors"]; !ok || numExec != 0 {
		t.Errorf("jenkins.numExecutors = %v, want 0", numExec)
	}

	unclassified, _ := doc["unclassified"].(map[string]any)
	durability, _ := unclassified["globalDefaultFlowDurabilityLevel"].(map[string]any)
	if durability == nil || durability["durabilityHint"] != "PERFORMANCE_OPTIMIZED" {
		t.Errorf("unclassified.globalDefaultFlowDurabilityLevel.durabilityHint = %v, want PERFORMANCE_OPTIMIZED", durability)
	}

	clouds, _ := jenkins["clouds"].([]any)
	found := false
	for _, c := range clouds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		k8s, _ := cm["kubernetes"].(map[string]any)
		if k8s != nil && k8s["name"] == "bundle-cloud" {
			found = true
		}
	}
	if !found {
		t.Errorf("bundle-declared cloud did not survive the merge, clouds: %+v", clouds)
	}
}

// TestApplyCellContent_NotACellIsNoop asserts a non-cell controller's
// jenkins.yaml is left untouched.
func TestApplyCellContent_NotACellIsNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	cr := testController("normal", "ns1", v1alpha1.ControllerPhaseConnected)
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	original := mat.JenkinsYAML

	if err := rec.applyCellContent(context.Background(), cr, mat, ""); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	if mat.JenkinsYAML != original {
		t.Errorf("non-cell controller's jenkins.yaml changed: got %q, want %q", mat.JenkinsYAML, original)
	}
}

// TestApplyCellContent_TenantCloudRendering is task 8.10's golden test: one
// placed tenant's cloud renders with the exact name/namespace/jenkinsUrl/
// usageRestricted/credentialsId, and its pod template carries the tenant
// ServiceAccount and the reserved node label. The tenant folder carries the
// kubernetes folder property naming that one cloud.
func TestApplyCellContent_TenantCloudRendering(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-agents"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        "lts",
				AgentNamespace: "payments-agents",
			},
		},
	})
	rec := newTestReconciler(client)

	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: ""}

	const endpoint = "http://cell1-svc.ns1.svc.cluster.local:8080"
	if err := rec.applyCellContent(context.Background(), cr, mat, endpoint); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &doc); err != nil {
		t.Fatalf("unmarshal merged jcasc: %v", err)
	}
	jenkins, _ := doc["jenkins"].(map[string]any)
	clouds, _ := jenkins["clouds"].([]any)
	if len(clouds) != 1 {
		t.Fatalf("expected exactly one tenant cloud, got %d: %+v", len(clouds), clouds)
	}
	k8s, _ := clouds[0].(map[string]any)["kubernetes"].(map[string]any)
	if k8s == nil {
		t.Fatalf("cloud has no kubernetes key: %+v", clouds[0])
	}
	wantScalars := map[string]any{
		"name":            "payments",
		"namespace":       "payments-agents",
		"jenkinsUrl":      endpoint,
		"usageRestricted": true,
		"credentialsId":   "varroa-cloud-payments",
	}
	for k, want := range wantScalars {
		if got := k8s[k]; got != want {
			t.Errorf("cloud.kubernetes.%s = %v, want %v", k, got, want)
		}
	}
	templates, _ := k8s["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected exactly one pod template, got %d", len(templates))
	}
	tmpl, _ := templates[0].(map[string]any)
	if tmpl["serviceAccount"] != "varroa-agent-payments" {
		t.Errorf("template.serviceAccount = %v, want varroa-agent-payments", tmpl["serviceAccount"])
	}
	labels, _ := tmpl["label"].(string)
	if !containsLabel(labels, "varroa-tenant-payments") {
		t.Errorf("template.label = %q, want it to contain the reserved label varroa-tenant-payments", labels)
	}

	// The tenant folder carries the kubernetes folder property naming that
	// one cloud.
	itemsDoc := unmarshalYAMLMap(t, mat.ItemsYAML)
	itemList, _ := itemsDoc["items"].([]any)
	if len(itemList) != 1 {
		t.Fatalf("expected exactly one top-level folder, got %d", len(itemList))
	}
	folder, _ := itemList[0].(map[string]any)
	props, _ := folder["properties"].([]any)
	permitted := findPermittedClouds(props)
	if len(permitted) != 1 || permitted[0] != "payments" {
		t.Errorf("permittedClouds = %v, want [payments]", permitted)
	}
}

func containsLabel(labels, want string) bool {
	for _, l := range splitFields(labels) {
		if l == want {
			return true
		}
	}
	return false
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func unmarshalYAMLMap(t *testing.T, y string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("unmarshal yaml: %v\n%s", err, y)
	}
	return doc
}

func findPermittedClouds(props []any) []string {
	for _, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		k8s, ok := pm["kubernetes"].(map[string]any)
		if !ok {
			continue
		}
		raw, _ := k8s["permittedClouds"].([]any)
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
