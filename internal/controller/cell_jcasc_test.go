package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestTenantAgentTimeoutsCoverTheResumeWindow pins the order of the agent
// pod ages. A change to the recorder's liveness window, arm window, or
// resume deadline that outgrows the plugin timeout fails here.
func TestTenantAgentTimeoutsCoverTheResumeWindow(t *testing.T) {
	// pluginRefreshInterval is the kubernetes plugin's refresh period
	// (GarbageCollection.recurrencePeriod), which Go cannot read.
	const pluginRefreshInterval = time.Minute
	gcTimeout := time.Duration(tenantAgentGCTimeoutSeconds) * time.Second

	if gcTimeout < TenantAgentAdoptBound+4*time.Minute {
		t.Errorf("plugin timeout %v is under the adopt bound %v plus the 4m margin", gcTimeout, TenantAgentAdoptBound)
	}
	if gcTimeout%time.Minute != 0 {
		t.Errorf("plugin timeout %v is not a whole minute", gcTimeout)
	}
	if tenantAgentRunningReapAfter != gcTimeout+5*time.Minute {
		t.Errorf("running reap age %v, want the plugin timeout %v plus 5m", tenantAgentRunningReapAfter, gcTimeout)
	}
	if tenantAgentPendingReapAfter >= gcTimeout {
		t.Errorf("pending reap age %v is not under the plugin timeout %v", tenantAgentPendingReapAfter, gcTimeout)
	}
	if tenantAgentPendingReapAfter < 2*pluginRefreshInterval {
		t.Errorf("pending reap age %v is under two plugin refreshes", tenantAgentPendingReapAfter)
	}
}

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

	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
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
	if durability == nil || durability["durabilityHint"] != "MAX_SURVIVABILITY" {
		t.Errorf("unclassified.globalDefaultFlowDurabilityLevel.durabilityHint = %v, want MAX_SURVIVABILITY", durability)
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

	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	if mat.JenkinsYAML != original {
		t.Errorf("non-cell controller's jenkins.yaml changed: got %q, want %q", mat.JenkinsYAML, original)
	}
}

// gitHostKeyStrategyOf returns
// security.gitHostKeyVerificationConfiguration.sshHostKeyVerificationStrategy
// from a JCasC document, and whether the path exists.
func gitHostKeyStrategyOf(t *testing.T, jenkinsYAML string) (any, bool) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
		t.Fatalf("unmarshal jcasc: %v", err)
	}
	security, _ := doc["security"].(map[string]any)
	config, _ := security["gitHostKeyVerificationConfiguration"].(map[string]any)
	if config == nil {
		return nil, false
	}
	strategy, ok := config["sshHostKeyVerificationStrategy"]
	return strategy, ok
}

func TestApplyCellContent_DefaultGitHostKeyStrategy(t *testing.T) {
	for name, jenkinsYAML := range map[string]string{
		"empty bundle":               "",
		"bundle with no strategy":    "jenkins:\n  systemMessage: hi\n",
		"bundle with other security": "security:\n  scriptApproval:\n    approvedSignatures: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			rec := newTestReconciler(newTestClient())
			mat := &bundle.MaterializedBundle{JenkinsYAML: jenkinsYAML}
			if err := rec.applyCellContent(context.Background(), cellControllerFixture(), mat, nil); err != nil {
				t.Fatalf("applyCellContent: %v", err)
			}
			strategy, ok := gitHostKeyStrategyOf(t, mat.JenkinsYAML)
			if !ok || strategy != "acceptFirstConnectionStrategy" {
				t.Errorf("sshHostKeyVerificationStrategy = %v (present %v), want acceptFirstConnectionStrategy:\n%s", strategy, ok, mat.JenkinsYAML)
			}
			if strings.Contains(jenkinsYAML, "scriptApproval") && !strings.Contains(mat.JenkinsYAML, "scriptApproval") {
				t.Errorf("the bundle's other security keys did not survive the merge:\n%s", mat.JenkinsYAML)
			}
		})
	}
}

func TestApplyCellContent_BundleGitHostKeyStrategyWins(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		mat := &bundle.MaterializedBundle{JenkinsYAML: "security:\n  gitHostKeyVerificationConfiguration:\n    sshHostKeyVerificationStrategy: noHostKeyVerificationStrategy\n"}
		if err := rec.applyCellContent(context.Background(), cellControllerFixture(), mat, nil); err != nil {
			t.Fatalf("applyCellContent: %v", err)
		}
		strategy, _ := gitHostKeyStrategyOf(t, mat.JenkinsYAML)
		if strategy != "noHostKeyVerificationStrategy" {
			t.Errorf("sshHostKeyVerificationStrategy = %v, want the bundle's noHostKeyVerificationStrategy", strategy)
		}
	})
	t.Run("map", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		mat := &bundle.MaterializedBundle{JenkinsYAML: "security:\n  gitHostKeyVerificationConfiguration:\n    sshHostKeyVerificationStrategy:\n      manuallyProvidedKeyVerificationStrategy:\n        approvedHostKeys: \"github.com ssh-ed25519 AAAA\"\n"}
		if err := rec.applyCellContent(context.Background(), cellControllerFixture(), mat, nil); err != nil {
			t.Fatalf("applyCellContent: %v", err)
		}
		strategy, _ := gitHostKeyStrategyOf(t, mat.JenkinsYAML)
		m, ok := strategy.(map[string]any)
		if !ok {
			t.Fatalf("sshHostKeyVerificationStrategy = %v, want the bundle's map", strategy)
		}
		manual, _ := m["manuallyProvidedKeyVerificationStrategy"].(map[string]any)
		if manual == nil || manual["approvedHostKeys"] != "github.com ssh-ed25519 AAAA" {
			t.Errorf("the bundle's manual strategy did not survive the merge: %v", m)
		}
		if strings.Contains(mat.JenkinsYAML, "acceptFirstConnectionStrategy") {
			t.Errorf("the default strategy leaked next to the bundle's map:\n%s", mat.JenkinsYAML)
		}
	})
}

func TestApplyCellContent_NonCellGetsNoGitHostKeyDefault(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	cr := testController("normal", "ns1", v1alpha1.ControllerPhaseConnected)
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	if strings.Contains(mat.JenkinsYAML, "gitHostKeyVerificationConfiguration") {
		t.Errorf("a controller that is not a cell got the git host-key default:\n%s", mat.JenkinsYAML)
	}
}

// TestApplyCellContent_TenantCloudRendering is task 8.10's golden test: one
// placed tenant's cloud renders with the exact name/namespace/jenkinsUrl/
// usageRestricted/credentialsId, and its pod template carries the tenant
// ServiceAccount and the reserved node label. The tenant folder carries the
// kubernetes folder property naming that one cloud.
func TestApplyCellContent_TenantCloudRendering(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-agents"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "lts",
				AgentNamespace: "payments-agents",
			},
		},
	})
	rec := newTestReconciler(client)

	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: ""}

	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
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
		"jenkinsUrl":      "http://varroa-agents-payments-0." + cr.Namespace + ".svc.cluster.local:8080",
		"usageRestricted": true,
		"credentialsId":   "varroa-cloud-payments",
	}
	for k, want := range wantScalars {
		if got := k8s[k]; got != want {
			t.Errorf("cloud.kubernetes.%s = %v, want %v", k, got, want)
		}
	}
	gc, _ := k8s["garbageCollection"].(map[string]any)
	if gc == nil || gc["timeout"] != tenantAgentGCTimeoutSeconds {
		t.Errorf("cloud.kubernetes.garbageCollection = %#v, want timeout %d", k8s["garbageCollection"], tenantAgentGCTimeoutSeconds)
	}
	if _, present := gc["namespaces"]; present {
		t.Errorf("cloud.kubernetes.garbageCollection carries namespaces %#v, want the key absent", gc["namespaces"])
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

// TestApplyCellContent_LiftedTemplateRendering asserts that a tenant
// bundle's pod template renders after <tenant>-default with the stamped
// keys and the singular label, and that a skipped template reaches
// cr.Status.Cell.TenantWarnings as PodTemplateSkipped.
func TestApplyCellContent_LiftedTemplateRendering(t *testing.T) {
	client := newTestClient()
	tenant := pipelinesTenant("payments")
	seedTenantBundleContent(client, tenant, "payments-bundle", map[string]string{
		"jenkins.yaml": minimalCloud("- name: golang\n  label: golang\n  labels: golang\n  namespace: kube-system\n  idleMinutes: 5\n- name: default\n"),
	})
	crdstore.MustSeed(client.store, tenant)
	rec := newTestReconciler(client)

	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{}
	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}

	doc := unmarshalYAMLMap(t, mat.JenkinsYAML)
	clouds, _ := doc["jenkins"].(map[string]any)["clouds"].([]any)
	if len(clouds) != 1 {
		t.Fatalf("expected one tenant cloud, got %d", len(clouds))
	}
	templates, _ := clouds[0].(map[string]any)["kubernetes"].(map[string]any)["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected the default and one lifted template, got %d: %+v", len(templates), templates)
	}
	if first, _ := templates[0].(map[string]any); first["name"] != "payments-default" {
		t.Errorf("first template = %v, want payments-default", first["name"])
	}
	lifted, _ := templates[1].(map[string]any)
	want := map[string]any{
		"name":           "payments-golang",
		"namespace":      "payments-ns",
		"serviceAccount": "varroa-agent-payments",
		"label":          "varroa-tenant-payments payments-golang",
		"idleMinutes":    5,
	}
	for k, v := range want {
		if lifted[k] != v {
			t.Errorf("lifted.%s = %v, want %v", k, lifted[k], v)
		}
	}
	if _, ok := lifted["labels"]; ok {
		t.Error("expected no labels key on the lifted template")
	}

	var skips []v1alpha1.TenantWarning
	for _, w := range cr.Status.Cell.TenantWarnings {
		if w.Reason == v1alpha1.TenantReasonPodTemplateSkipped {
			skips = append(skips, w)
		}
	}
	if len(skips) != 1 || skips[0].Tenant != "payments" {
		t.Errorf("expected one PodTemplateSkipped entry for payments, got %+v", cr.Status.Cell.TenantWarnings)
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
