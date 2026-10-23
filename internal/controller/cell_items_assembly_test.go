package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// seedTenantBundle seeds a Ready ComposedBundle plus its materialized
// content ConfigMap for a tenant, and points the Tenant's
// spec.pipelines.composedBundleRef at it.
func seedTenantBundle(client *testClient, tenant *v1alpha1.Tenant, bundleName, itemsYAML string) {
	crdstore.MustSeed(client.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: bundleName, Namespace: "catalog-ns"},
		Status:     v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundleReady, ContentRef: bundleName + "-content"},
	})
	client.configMapData[bundleName+"-content"] = map[string]string{"items.yaml": itemsYAML}
	tenant.Spec.Pipelines.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: bundleName, Namespace: "catalog-ns"}
}

const cleanPipelineItemsYAML = `items:
  - kind: pipeline
    name: build
    definition:
      script: "echo hi"
`

const offendingPipelineItemsYAML = `items:
  - kind: pipeline
    name: offender
    definition:
      script: "echo hi"
    triggers:
      - pollSCM: {}
`

// tenantTemplateFixtureYAML builds a pipeline-templates.yaml body, in the
// shape composePipelineTemplates writes, holding one template named name
// whose body carries ${env.BRANCH_NAME} — a Groovy expression, not a
// catalog variable.
func tenantTemplateFixtureYAML(name string) string {
	return fmt.Sprintf(`templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: %s
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline { stages { stage('build') { steps { echo '${env.BRANCH_NAME}' } } } }"
`, name)
}

// TestAssembleCellContent_DrainingTenantOmitted is half of task 8.12's
// end-to-end acceptance: a draining tenant's folder is omitted from the
// merged manifest, and a clean tenant's folder is present. The draining
// tenant's cloud and cloud credential stay, so a build already running
// when the folder disappeared can still provision an agent (design D4).
func TestAssembleCellContent_DrainingTenantOmitted(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	clean := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "clean"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"clean-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "clean-ns"},
		},
	}
	seedTenantBundle(client, clean, "clean-bundle", cleanPipelineItemsYAML)

	draining := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "draining"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"draining-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "draining-ns"},
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cell: cr.Name, CellNamespace: cr.Namespace},
			Conditions: []v1alpha1.TenantCondition{
				{Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TenantReasonTenantMoving},
			},
		},
	}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{clean, draining}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}

	doc := unmarshalYAMLMap(t, state.ItemsYAML)
	itemList, _ := doc["items"].([]any)
	if len(itemList) != 1 {
		t.Fatalf("expected exactly one top-level folder (clean only), got %d: %+v", len(itemList), itemList)
	}
	folder, _ := itemList[0].(map[string]any)
	if folder["name"] != "clean" {
		t.Errorf("expected the surviving folder to be %q, got %q", "clean", folder["name"])
	}
	// A draining tenant's folder is gone, so no new build can be scheduled
	// there. Its cloud stays until it leaves status.cell.tenants, so a
	// build already running when the folder disappeared can still
	// provision an agent.
	if len(state.Clouds) != 2 {
		t.Fatalf("expected both tenant clouds (clean and draining) to remain, got %+v", state.Clouds)
	}
	cloudNames := map[string]bool{}
	for _, c := range state.Clouds {
		cloudNames[c.Name] = true
	}
	if !cloudNames["clean"] || !cloudNames["draining"] {
		t.Errorf("expected clouds for both clean and draining, got %+v", state.Clouds)
	}
}

// TestAssembleCellContent_LintFindingHoldsOnlyThatTenant proves that a lint
// finding in one tenant's items holds that tenant's folder and pushes the
// other tenants.
func TestAssembleCellContent_LintFindingHoldsOnlyThatTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	clean := pipelinesTenant("clean")
	seedTenantBundle(client, clean, "clean-bundle", cleanPipelineItemsYAML)

	draining := pipelinesTenant("draining")
	markDraining(draining, cr)

	rejected := pipelinesTenant("rejected")
	seedTenantBundle(client, rejected, "rejected-bundle", offendingPipelineItemsYAML)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{clean, draining, rejected}, nil, "")
	if err != nil {
		t.Fatalf("expected one tenant's finding to hold only that tenant, got: %v", err)
	}
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "clean" {
		t.Errorf("expected only the clean folder pushed, got %v", got)
	}
	if len(manifest.Retain) != 1 || manifest.Retain[0] != "rejected" {
		t.Errorf("expected retain [rejected], got %v", manifest.Retain)
	}
	msg := heldMessage(state.Warnings, "rejected")
	if !strings.Contains(msg, "no-poll-scm") || !strings.Contains(msg, "rejected/offender") {
		t.Errorf("expected the ItemsHeld message to name no-poll-scm and rejected/offender, got %q", msg)
	}

	crdstore.MustSeed(client.store, clean)
	crdstore.MustSeed(client.store, draining)
	crdstore.MustSeed(client.store, rejected)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"clean", "draining", "rejected"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: untouched\n"}
	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	if heldMessage(cr.Status.Cell.TenantWarnings, "rejected") == "" {
		t.Errorf("expected an ItemsHeld entry for rejected on the cell status, got %+v", cr.Status.Cell.TenantWarnings)
	}
}

// TestAssembleCellContent_TemplateFragmentsGetOwnFolders is task 3.7's
// acceptance for task 3.6: two placed tenants each carry their own template
// content, and each gets its own Folder value in the returned fragments.
func TestAssembleCellContent_TemplateFragmentsGetOwnFolders(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenantA := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, tenantA, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	tenantB := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}
	seedTenantBundle(client, tenantB, "team-b-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-b-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("node-service")

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenantA, tenantB}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}

	if len(state.TemplateFragments) != 2 {
		t.Fatalf("expected one fragment per tenant, got %+v", state.TemplateFragments)
	}
	byFolder := map[string]string{}
	for _, f := range state.TemplateFragments {
		byFolder[f.Folder] = f.TemplatesYAML
	}
	if !strings.Contains(byFolder["team-a"], "go-service") {
		t.Errorf("expected team-a's fragment to hold go-service, got %+v", byFolder)
	}
	if !strings.Contains(byFolder["team-b"], "node-service") {
		t.Errorf("expected team-b's fragment to hold node-service, got %+v", byFolder)
	}
	if byFolder["team-a"] == byFolder["team-b"] {
		t.Error("expected each tenant to keep its own template content")
	}
}

// TestPipelineTemplateDocument_FolderKeyRejectedAtSync is task 3.7's other
// half: a tenant cannot declare a folder. The operator is the only writer
// of folder, and Document carries no such field, so strict decoding rejects
// an unknown key at sync (task 2.1). The cell's tenant assembly therefore
// never needs to strip one.
func TestPipelineTemplateDocument_FolderKeyRejectedAtSync(t *testing.T) {
	raw := []byte(`apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: go-service
spec:
  version: 1.0.0
  folder: team-a
  parameters:
    type: object
    additionalProperties: false
  jenkinsfile: "pipeline {}"
`)
	findings, doc := pipelinetemplate.ValidateDocument(raw)
	if doc != nil {
		t.Fatalf("expected no parsed document for an unknown spec.folder key, got %+v", doc)
	}
	if len(findings) == 0 {
		t.Fatal("expected a finding rejecting the unknown folder key")
	}
}

// TestResolveBundleForController_CellTenantTemplateBodySurvivesInjection is
// design D15's second injection-order test: for a cell, a placed tenant's
// template body holding ${env.BRANCH_NAME} survives resolveBundleForController
// and lands in the rendered jenkins.yaml, folder-stamped with the Tenant name
// (design D7, tenant-isolation).
func TestResolveBundleForController_CellTenantTemplateBodySurvivesInjection(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, tenant, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")
	crdstore.MustSeed(client.store, tenant)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	if !strings.Contains(mat.JenkinsYAML, "${env.BRANCH_NAME}") {
		t.Errorf("expected the tenant template body to survive injection, got: %s", mat.JenkinsYAML)
	}

	doc := unmarshalYAMLMap(t, mat.JenkinsYAML)
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected exactly one delivered template, got %+v", templates)
	}
	entry, _ := templates[0].(map[string]any)
	if entry["name"] != "go-service" {
		t.Errorf("template name = %v, want go-service", entry["name"])
	}
	if entry["folder"] != "team-a" {
		t.Errorf("expected folder %q for the placed tenant, got %v", "team-a", entry["folder"])
	}
}

// TestResolveBundleForController_CellTwoTenantsSameTemplateBothReachJenkinsYAML
// proves new 🔴1 of review round 2 at the full resolveBundleForController
// cell path, not only at InjectPipelineTemplates: two placed tenants that
// each deliver the identical go-service@1.0.0 document both reach the
// rendered jenkins.yaml with their own folder (design D7). Before the
// round-2 fix, InjectPipelineTemplates deduplicated the two tenants'
// identical entries into one, and the second tenant's jobs could not
// resolve the template.
func TestResolveBundleForController_CellTwoTenantsSameTemplateBothReachJenkinsYAML(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenantA := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, tenantA, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	tenantB := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}
	seedTenantBundle(client, tenantB, "team-b-bundle", cleanPipelineItemsYAML)
	// Same name, version, and content as team-a's fragment: only the
	// folder differs once each fragment is stamped.
	client.configMapData["team-b-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	crdstore.MustSeed(client.store, tenantA)
	crdstore.MustSeed(client.store, tenantB)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a", "team-b"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	doc := unmarshalYAMLMap(t, mat.JenkinsYAML)
	unclassified, _ := doc["unclassified"].(map[string]any)
	store, _ := unclassified["varroaPipelineTemplates"].(map[string]any)
	templates, _ := store["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected both tenants' identical templates to survive as two folder-scoped entries, got %+v", templates)
	}
	folders := map[string]int{}
	for _, tpl := range templates {
		entry, _ := tpl.(map[string]any)
		if entry["name"] != "go-service" {
			t.Errorf("expected both entries to be go-service, got %+v", entry)
		}
		folders[entry["folder"].(string)]++
	}
	if folders["team-a"] != 1 || folders["team-b"] != 1 {
		t.Errorf("expected exactly one entry each for team-a and team-b, got %+v", folders)
	}
}

// TestAssembleCellContent_TenantCloudNamesTheAgentService asserts that each
// tenant cloud of one cell names that tenant's own slot 0 agent Service in
// the cell's namespace, and that webSocket stays true.
func TestAssembleCellContent_TenantCloudNamesTheAgentService(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenantA := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	tenantB := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenantA, tenantB}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if len(state.Clouds) != 2 {
		t.Fatalf("expected one cloud per tenant, got %d", len(state.Clouds))
	}
	want := map[string]string{
		"team-a": "http://varroa-agents-team-a-0." + cr.Namespace + ".svc.cluster.local:8080",
		"team-b": "http://varroa-agents-team-b-0." + cr.Namespace + ".svc.cluster.local:8080",
	}
	for _, cloud := range state.Clouds {
		if cloud.JenkinsURL != want[cloud.Name] {
			t.Errorf("%s jenkinsUrl = %q, want %q", cloud.Name, cloud.JenkinsURL, want[cloud.Name])
		}
		if !cloud.WebSocket {
			t.Errorf("%s webSocket = false, want true", cloud.Name)
		}
	}
}

// seedTenantBundleContent seeds a tenant bundle whose content ConfigMap
// holds cleanPipelineItemsYAML plus every key of extra.
func seedTenantBundleContent(client *testClient, tenant *v1alpha1.Tenant, bundleName string, extra map[string]string) {
	seedTenantBundle(client, tenant, bundleName, cleanPipelineItemsYAML)
	for k, v := range extra {
		client.configMapData[bundleName+"-content"][k] = v
	}
}

// pipelinesTenant builds a pipelines tenant whose agent namespace is
// "<name>-ns".
func pipelinesTenant(name string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{name + "-ns"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: name + "-ns"},
		},
	}
}

// markDraining gives tenant the Placed=False/TenantMoving condition and a
// placement on cr, which makes it a draining tenant of cr.
func markDraining(tenant *v1alpha1.Tenant, cr *v1alpha1.Controller) {
	tenant.Status = v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: cr.Name, CellNamespace: cr.Namespace},
		Conditions: []v1alpha1.TenantCondition{
			{Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TenantReasonTenantMoving},
		},
	}
}

func cloudTemplateNames(state assembleCellState, cloud string) []string {
	var names []string
	for _, c := range state.Clouds {
		if c.Name != cloud {
			continue
		}
		for _, tpl := range c.Templates {
			names = append(names, tpl.Name)
		}
	}
	return names
}

func tenantWarningsFor(ws []v1alpha1.TenantWarning, tenant, reason string) []string {
	var out []string
	for _, w := range ws {
		if w.Tenant == tenant && w.Reason == reason {
			out = append(out, w.Message)
		}
	}
	return out
}

func folderNames(t *testing.T, itemsYAML string) []string {
	t.Helper()
	itemList, _ := unmarshalYAMLMap(t, itemsYAML)["items"].([]any)
	names := make([]string, 0, len(itemList))
	for _, it := range itemList {
		m, _ := it.(map[string]any)
		names = append(names, fmt.Sprint(m["name"]))
	}
	return names
}

func TestAssembleCellContent_LiftsTenantPodTemplates(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	golang := pipelinesTenant("golang")
	seedTenantBundleContent(client, golang, "golang-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})

	residue := pipelinesTenant("residue")
	seedTenantBundleContent(client, residue, "residue-bundle", map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: hi\n",
	})

	plugins := pipelinesTenant("plugins")
	seedTenantBundleContent(client, plugins, "plugins-bundle", map[string]string{"plugins.yaml": "plugins:\n  - git\n"})

	skipped := pipelinesTenant("skipped")
	seedTenantBundleContent(client, skipped, "skipped-bundle", map[string]string{
		"jenkins.yaml": minimalCloud("- name: default\n- name: node\n"),
	})

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{golang, residue, plugins, skipped}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}

	if got := cloudTemplateNames(state, "golang"); len(got) != 2 || got[0] != "golang-default" || got[1] != "golang-golang" {
		t.Errorf("golang cloud templates = %v, want [golang-default golang-golang]", got)
	}
	if got := tenantWarningsFor(state.Warnings, "golang", v1alpha1.TenantReasonBundleKeysIgnored); len(got) != 0 {
		t.Errorf("expected no BundleKeysIgnored for a pod-template-only jenkins.yaml, got %q", got)
	}

	wantResidue := []string{`tenant bundle key "jenkins.yaml" is ignored on a cell except its pod templates: jenkins.systemMessage`}
	if got := tenantWarningsFor(state.Warnings, "residue", v1alpha1.TenantReasonBundleKeysIgnored); !equalStrings(got, wantResidue) {
		t.Errorf("residue warnings = %q, want %q", got, wantResidue)
	}

	wantPlugins := []string{`tenant bundle key "plugins.yaml" is ignored on a cell`}
	if got := tenantWarningsFor(state.Warnings, "plugins", v1alpha1.TenantReasonBundleKeysIgnored); !equalStrings(got, wantPlugins) {
		t.Errorf("plugins warnings = %q, want %q", got, wantPlugins)
	}

	if got := cloudTemplateNames(state, "skipped"); len(got) != 2 || got[1] != "skipped-node" {
		t.Errorf("skipped cloud templates = %v, want [skipped-default skipped-node]", got)
	}
	wantSkip := []string{`pod template "default": the name "default" is reserved`}
	if got := tenantWarningsFor(state.Warnings, "skipped", v1alpha1.TenantReasonPodTemplateSkipped); !equalStrings(got, wantSkip) {
		t.Errorf("skip warnings = %q, want %q", got, wantSkip)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAssembleCellContent_DrainingTenantKeepsLiftedTemplates(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	draining := pipelinesTenant("draining")
	seedTenantBundleContent(client, draining, "draining-bundle", map[string]string{
		"jenkins.yaml": "unclassified: {}\n" + minimalCloud(golangPodTemplate),
	})
	markDraining(draining, cr)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{draining}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if got := cloudTemplateNames(state, "draining"); len(got) != 2 || got[1] != "draining-golang" {
		t.Errorf("draining cloud templates = %v, want [draining-default draining-golang]", got)
	}
	if got := folderNames(t, state.ItemsYAML); len(got) != 0 {
		t.Errorf("expected no folder for a draining tenant, got %v", got)
	}
	want := []string{`tenant bundle key "jenkins.yaml" is ignored on a cell except its pod templates: unclassified`}
	if got := tenantWarningsFor(state.Warnings, "draining", v1alpha1.TenantReasonBundleKeysIgnored); !equalStrings(got, want) {
		t.Errorf("draining warnings = %q, want %q", got, want)
	}
}

func TestAssembleCellContent_DrainingTenantUnreadableBundle(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	draining := pipelinesTenant("draining")
	seedTenantBundleContent(client, draining, "draining-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})
	delete(client.configMapData, "draining-bundle-content")
	markDraining(draining, cr)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{draining}, nil, "")
	if err != nil {
		t.Fatalf("expected the assembly to succeed, got %v", err)
	}
	if got := cloudTemplateNames(state, "draining"); len(got) != 1 || got[0] != "draining-default" {
		t.Errorf("draining cloud templates = %v, want [draining-default]", got)
	}
}

// TestAssembleCellContent_ActiveTenantUnreadableBundleFails proves that a
// read error is not a tenant fault: a hold would push the tenant's cloud
// with no pod template and drop its templates, so the cell keeps its last
// applied state instead.
func TestAssembleCellContent_ActiveTenantUnreadableBundleFails(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	active := pipelinesTenant("active")
	seedTenantBundleContent(client, active, "active-bundle", nil)
	delete(client.configMapData, "active-bundle-content")

	if _, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{active}, nil, ""); err == nil {
		t.Fatal("expected the unreadable bundle of a non-draining tenant to fail the assembly")
	}
}

func TestAssembleCellContent_UnparseableJenkinsYAML(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	broken := pipelinesTenant("broken")
	seedTenantBundleContent(client, broken, "broken-bundle", map[string]string{"jenkins.yaml": "jenkins: [unclosed\n"})

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{broken}, nil, "")
	if err != nil {
		t.Fatalf("expected the assembly to succeed, got %v", err)
	}
	if got := cloudTemplateNames(state, "broken"); len(got) != 1 || got[0] != "broken-default" {
		t.Errorf("broken cloud templates = %v, want [broken-default]", got)
	}
	got := tenantWarningsFor(state.Warnings, "broken", v1alpha1.TenantReasonBundleKeysIgnored)
	if len(got) != 1 || !strings.HasPrefix(got[0], `tenant bundle key "jenkins.yaml" does not parse: `) {
		t.Errorf("unexpected warnings %q", got)
	}
}

func TestAssembleCellContent_EveryMemberCellRendersTheSameTemplates(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	tenant := pipelinesTenant("payments")
	seedTenantBundleContent(client, tenant, "payments-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})
	slot0, slot1 := int32(0), int32(1)
	tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
		Cell: "cell-0", CellNamespace: "ns1",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-0", CellNamespace: "ns1", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0},
			{Cell: "cell-1", CellNamespace: "ns1", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot1},
			{Cell: "cell-2", CellNamespace: "ns1", Role: v1alpha1.TenantMemberRoleStandby},
		},
	}

	for _, cell := range []string{"cell-0", "cell-1", "cell-2"} {
		cr := cellControllerFixture()
		cr.Name = cell
		state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, "")
		if err != nil {
			t.Fatalf("%s: assembleCellContent: %v", cell, err)
		}
		if got := cloudTemplateNames(state, "payments"); len(got) != 2 || got[1] != "payments-golang" {
			t.Errorf("%s: payments cloud templates = %v, want [payments-default payments-golang]", cell, got)
		}
	}
}

// TestResolveBundleForController_CellSkipsInterpolatedPodTemplate proves
// that one tenant's pod template with a cell-side reference cannot fail
// the cell: the lift skips it, and the other tenant's template renders.
func TestResolveBundleForController_CellSkipsInterpolatedPodTemplate(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	evil := pipelinesTenant("evil")
	seedTenantBundleContent(client, evil, "evil-bundle", map[string]string{
		"jenkins.yaml": minimalCloud("- name: steal\n  yaml: ${readFile:/x}\n"),
	})
	good := pipelinesTenant("good")
	seedTenantBundleContent(client, good, "good-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})
	crdstore.MustSeed(client.store, evil)
	crdstore.MustSeed(client.store, good)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"evil", "good"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	if strings.Contains(mat.JenkinsYAML, "evil-steal") || strings.Contains(mat.JenkinsYAML, "readFile:/x") {
		t.Errorf("expected the skipped template absent, got: %s", mat.JenkinsYAML)
	}
	if !strings.Contains(mat.JenkinsYAML, "good-golang") {
		t.Errorf("expected good-golang in the cell jenkins.yaml, got: %s", mat.JenkinsYAML)
	}
	got := tenantWarningsFor(cr.Status.Cell.TenantWarnings, "evil", v1alpha1.TenantReasonPodTemplateSkipped)
	if len(got) != 1 {
		t.Errorf("expected one PodTemplateSkipped entry for evil, got %q", got)
	}
}

// TestResolveBundleForController_CellResolvesTenantVariable proves that a
// tenant variable resolves inside a lifted template, and the cell's
// unresolved-variable check then passes.
func TestResolveBundleForController_CellResolvesTenantVariable(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("payments")
	seedTenantBundleContent(client, tenant, "payments-bundle", map[string]string{
		"jenkins.yaml":   minimalCloud("- name: golang\n  containers:\n    - name: golang\n      image: ${golang_image}\n"),
		"variables.yaml": "golang_image: golang:1.26-bookworm\n",
	})
	crdstore.MustSeed(client.store, tenant)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	if !strings.Contains(mat.JenkinsYAML, "image: golang:1.26-bookworm") {
		t.Errorf("expected the resolved image in the cell jenkins.yaml, got: %s", mat.JenkinsYAML)
	}
}

// parseCellItems parses a cell's merged items.yaml.
func parseCellItems(t *testing.T, itemsYAML string) *items.Manifest {
	t.Helper()
	m, err := items.Parse(itemsYAML)
	if err != nil {
		t.Fatalf("parse cell items: %v\n%s", err, itemsYAML)
	}
	return m
}

// topLevelNames lists the top-level item names of m.
func topLevelNames(m *items.Manifest) []string {
	names := make([]string, 0, len(m.Items))
	for _, it := range m.Items {
		names = append(names, it.Name)
	}
	return names
}

// heldMessage answers the message of the ItemsHeld warning for tenant, or
// "" when no such warning exists.
func heldMessage(warnings []v1alpha1.TenantWarning, tenant string) string {
	for _, w := range warnings {
		if w.Tenant == tenant && w.Reason == v1alpha1.TenantReasonItemsHeld {
			return w.Message
		}
	}
	return ""
}

// assertHeldAlone assembles good and a tenant named bad whose items.yaml is
// badItems, with no plugin pinned, and checks that only bad is held. It
// answers the ItemsHeld message for bad.
func assertHeldAlone(t *testing.T, badItems string) string {
	t.Helper()
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", badItems)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{good, bad}, nil, "")
	if err != nil {
		t.Fatalf("expected the fault to hold only the tenant bad, got: %v", err)
	}
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the good folder pushed, got %v", got)
	}
	if len(manifest.Retain) != 1 || manifest.Retain[0] != "bad" {
		t.Errorf("expected retain [bad], got %v", manifest.Retain)
	}
	msg := heldMessage(state.Warnings, "bad")
	if msg == "" {
		t.Fatalf("expected an ItemsHeld warning for bad, got %+v", state.Warnings)
	}
	if heldMessage(state.Warnings, "good") != "" {
		t.Errorf("expected no ItemsHeld warning for good, got %+v", state.Warnings)
	}
	return msg
}

func TestAssembleCellContent_UnresolvedVariableHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, `items:
  - kind: pipeline
    name: build
    description: "uses ${x}"
    definition:
      script: "echo hi"
`)
	if !strings.Contains(msg, "unresolved variables: x") {
		t.Errorf("expected the message to name x, got %q", msg)
	}
}

func TestAssembleCellContent_UnparseableItemsHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, "items: [unclosed\n")
	if !strings.Contains(msg, "parse tenant items.yaml") {
		t.Errorf("expected a parse fault in the message, got %q", msg)
	}
}

func TestAssembleCellContent_TenantRootHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, "root: escape\nitems: []\n")
	if !strings.Contains(msg, "root") {
		t.Errorf("expected the message to name root, got %q", msg)
	}
}

func TestAssembleCellContent_ControllerVariableInTenantItemHolds(t *testing.T) {
	msg := assertHeldAlone(t, `items:
  - kind: pipeline
    name: build
    description: "runs on ${varroa_controller_name}"
    definition:
      script: "echo hi"
`)
	if !strings.Contains(msg, "varroa_controller_name") {
		t.Errorf("expected the message to name varroa_controller_name, got %q", msg)
	}
}

func TestAssembleCellContent_MultibranchWithoutStrategyPluginHoldsTenant(t *testing.T) {
	msg := assertHeldAlone(t, multibranchItemsYAML)
	if !strings.Contains(msg, branchBuildStrategiesPlugin) {
		t.Errorf("expected the message to name %s, got %q", branchBuildStrategiesPlugin, msg)
	}
}

func TestAssembleCellContent_JobDSLStepHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, `items:
  - kind: pipeline
    name: seed
    definition:
      script: "jobDsl targets: 'jobs/*.groovy'"
`)
	if !strings.Contains(msg, "no-job-dsl") {
		t.Errorf("expected the message to name no-job-dsl, got %q", msg)
	}
	if !strings.Contains(msg, "seed") {
		t.Errorf("expected the message to name the seed path, got %q", msg)
	}
}

func TestAssembleCellContent_TenantVariableResolvesItsItems(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("payments")
	seedTenantBundle(client, tenant, "payments-bundle", `items:
  - kind: pipeline
    name: build
    description: "uses ${x}"
    definition:
      script: "echo hi"
`)
	client.configMapData["payments-bundle-content"]["variables.yaml"] = "x: hello\n"

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 0 {
		t.Fatalf("expected no hold, got %v: %+v", held, state.Warnings)
	}
	if !strings.Contains(state.ItemsYAML, "uses hello") {
		t.Errorf("expected the tenant variable resolved, got:\n%s", state.ItemsYAML)
	}
}

func TestAssembleCellContent_HeldTenantKeepsCloudAndTemplatesAndCarriesNoCredentials(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "cred-text",
		map[string]string{"varroa.dev/credential-id": "token", "varroa.dev/credential-kind": "secretText"},
		map[string][]byte{"secret": []byte("value")})
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	payments := pipelinesTenant("payments")
	seedTenantBundle(client, payments, "payments-bundle", offendingPipelineItemsYAML)
	client.configMapData["payments-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-build")
	client.configMapData["payments-bundle-content"]["jenkins.yaml"] = minimalCloud(golangPodTemplate)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{payments}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 1 || held[0] != "payments" {
		t.Fatalf("expected held [payments], got %v", held)
	}
	if got := cloudTemplateNames(state, "payments"); !slices.Contains(got, "payments-golang") {
		t.Errorf("expected the held tenant to keep its lifted pod template, got %v", got)
	}
	if len(state.TemplateFragments) != 1 || state.TemplateFragments[0].Folder != "payments" {
		t.Errorf("expected the held tenant to keep its template fragment, got %+v", state.TemplateFragments)
	}
	if len(state.FolderCredentials) != 0 {
		t.Errorf("expected no folder credentials for a held folder, got %+v", state.FolderCredentials)
	}
}

func TestAssembleCellContent_DrainingTenantIsNeverHeld(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	draining := pipelinesTenant("draining")
	seedTenantBundle(client, draining, "draining-bundle", offendingPipelineItemsYAML)
	markDraining(draining, cr)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{draining}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	manifest := parseCellItems(t, state.ItemsYAML)
	if len(manifest.Items) != 0 || len(manifest.Retain) != 0 {
		t.Errorf("expected a draining tenant omitted and not retained, got items %v retain %v",
			topLevelNames(manifest), manifest.Retain)
	}
	if heldMessage(state.Warnings, "draining") != "" {
		t.Errorf("expected no ItemsHeld warning for a draining tenant, got %+v", state.Warnings)
	}
}

func TestAssembleCellContent_RetainListsEveryHeldTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	zeta := pipelinesTenant("zeta")
	seedTenantBundle(client, zeta, "zeta-bundle", offendingPipelineItemsYAML)
	alpha := pipelinesTenant("alpha")
	seedTenantBundle(client, alpha, "alpha-bundle", "items: [unclosed\n")

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{zeta, alpha}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	manifest := parseCellItems(t, state.ItemsYAML)
	if len(manifest.Retain) != 2 || manifest.Retain[0] != "alpha" || manifest.Retain[1] != "zeta" {
		t.Errorf("expected retain [alpha zeta], got %v", manifest.Retain)
	}
}

// resolveCellWithTenants seeds every tenant on a cell and runs
// resolveBundleForController.
func resolveCellWithTenants(t *testing.T, client *testClient, tenants ...*v1alpha1.Tenant) (*bundle.MaterializedBundle, *v1alpha1.Controller) {
	t.Helper()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	names := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		crdstore.MustSeed(client.store, tenant)
		names = append(names, tenant.Name)
	}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: names}
	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	return mat, cr
}

func TestResolveBundleForController_CellResolvesTenantItemVariable(t *testing.T) {
	client := newTestClientWithBundle()
	tenant := pipelinesTenant("payments")
	seedTenantBundleContent(client, tenant, "payments-bundle", map[string]string{
		"items.yaml": `items:
  - kind: pipeline
    name: build
    description: "uses ${x}"
    definition:
      script: "echo hi"
`,
		"variables.yaml": "x: hello\n",
	})
	mat, _ := resolveCellWithTenants(t, client, tenant)
	if !strings.Contains(mat.ItemsYAML, "uses hello") {
		t.Errorf("expected the tenant variable resolved in the cell items, got:\n%s", mat.ItemsYAML)
	}
}

func TestResolveBundleForController_CellTenantFaultLeavesOtherTenantsPushed(t *testing.T) {
	client := newTestClientWithBundle()
	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", `items:
  - kind: pipeline
    name: build
    description: "uses ${x}"
    definition:
      script: "echo hi"
`)
	mat, cr := resolveCellWithTenants(t, client, good, bad)
	manifest := parseCellItems(t, mat.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected the good folder pushed, got %v", got)
	}
	if len(manifest.Retain) != 1 || manifest.Retain[0] != "bad" {
		t.Errorf("expected retain [bad], got %v", manifest.Retain)
	}
	if heldMessage(cr.Status.Cell.TenantWarnings, "bad") == "" {
		t.Errorf("expected an ItemsHeld entry for bad, got %+v", cr.Status.Cell.TenantWarnings)
	}
}

func TestResolveBundleForController_CellEscapedItemReferenceSurvives(t *testing.T) {
	client := newTestClientWithBundle()
	tenant := pipelinesTenant("payments")
	seedTenantBundle(client, tenant, "payments-bundle", `items:
  - kind: pipeline
    name: build
    description: "literal ^${varroa_controller_name}"
    definition:
      script: "echo hi"
`)
	mat, _ := resolveCellWithTenants(t, client, tenant)
	if !strings.Contains(mat.ItemsYAML, "^${varroa_controller_name}") {
		t.Errorf("expected the escaped reference unchanged, got:\n%s", mat.ItemsYAML)
	}
}

func TestAppliedTenantsAfterItemsApply_HeldTenantKeepsPriorState(t *testing.T) {
	cell := &v1alpha1.CellStatus{
		Tenants:        []string{"fresh", "held-new", "held-old", "plain"},
		AppliedTenants: []string{"held-old"},
		TenantWarnings: []v1alpha1.TenantWarning{
			{Tenant: "held-new", Reason: v1alpha1.TenantReasonItemsHeld, Message: "m"},
			{Tenant: "held-old", Reason: v1alpha1.TenantReasonItemsHeld, Message: "m"},
			{Tenant: "plain", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: "m"},
		},
	}
	got := appliedTenantsAfterItemsApply(cell, nil)
	want := []string{"fresh", "held-old", "plain"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("appliedTenantsAfterItemsApply = %v, want %v", got, want)
	}
}

func TestAppliedTenantsAfterItemsApply_RefusedTenantKeepsPriorState(t *testing.T) {
	cell := func() *v1alpha1.CellStatus {
		return &v1alpha1.CellStatus{
			Tenants:        []string{"healthy", "refused-new", "refused-old", "held-new"},
			AppliedTenants: []string{"refused-old", "gone"},
			TenantWarnings: []v1alpha1.TenantWarning{
				{Tenant: "held-new", Reason: v1alpha1.TenantReasonItemsHeld, Message: "m"},
			},
		}
	}
	cases := []struct {
		name    string
		refused []string
		want    []string
	}{
		{
			name:    "mixed failures",
			refused: []string{"refused-new/a", "refused-old", "refused-old/b/c", "other/x"},
			want:    []string{"healthy", "refused-old"},
		},
		{
			name:    "no failures",
			refused: nil,
			want:    []string{"healthy", "refused-new", "refused-old"},
		},
		{
			name:    "prefix trap",
			refused: []string{"healthy-x/job"},
			want:    []string{"healthy", "refused-new", "refused-old"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appliedTenantsAfterItemsApply(cell(), tc.refused)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("appliedTenantsAfterItemsApply = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssembleCellContent_UnloadableTemplateFragmentHoldsTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", cleanPipelineItemsYAML)
	client.configMapData["bad-bundle-content"]["pipeline-templates.yaml"] = "templates: [unclosed\n"

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{good, bad}, nil, "")
	if err != nil {
		t.Fatalf("expected a bad fragment to hold only its tenant, got: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 1 || held[0] != "bad" {
		t.Errorf("expected held [bad], got %v", held)
	}
	if !strings.Contains(heldMessage(state.Warnings, "bad"), "pipeline-templates.yaml") {
		t.Errorf("expected the message to name pipeline-templates.yaml, got %+v", state.Warnings)
	}
	for _, f := range state.TemplateFragments {
		if f.Folder == "bad" {
			t.Errorf("expected the bad fragment dropped, got %+v", state.TemplateFragments)
		}
	}
}

func TestAssembleCellContent_FragmentConflictingWithCellTemplateHoldsTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("payments")
	seedTenantBundle(client, tenant, "payments-bundle", cleanPipelineItemsYAML)
	client.configMapData["payments-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-build")
	base, err := bundle.InjectPipelineTemplates("jenkins:\n  numExecutors: 0\n",
		strings.Replace(tenantTemplateFixtureYAML("go-build"), "echo", "sh", 1), "")
	if err != nil {
		t.Fatalf("build the template base: %v", err)
	}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, base)
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 1 || held[0] != "payments" {
		t.Errorf("expected held [payments], got %v: %+v", held, state.Warnings)
	}
}

func TestResolveBundleForController_CellTenantFragmentFaultLeavesCellResolving(t *testing.T) {
	client := newTestClientWithBundle()
	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", cleanPipelineItemsYAML)
	client.configMapData["bad-bundle-content"]["pipeline-templates.yaml"] = "templates: [unclosed\n"

	mat, cr := resolveCellWithTenants(t, client, good, bad)
	if got := topLevelNames(parseCellItems(t, mat.ItemsYAML)); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected the good folder pushed, got %v", got)
	}
	if heldMessage(cr.Status.Cell.TenantWarnings, "bad") == "" {
		t.Errorf("expected an ItemsHeld entry for bad, got %+v", cr.Status.Cell.TenantWarnings)
	}
}

func TestDropKeptTenants_RemovesHeldAndRefusedTenants(t *testing.T) {
	tests := []struct {
		name     string
		applied  []string
		warnings []v1alpha1.TenantWarning
		want     string
	}{
		{
			name:     "held tenant is removed",
			applied:  []string{"held", "plain"},
			warnings: []v1alpha1.TenantWarning{{Tenant: "held", Reason: v1alpha1.TenantReasonItemsHeld, Message: "m"}},
			want:     "plain",
		},
		{
			name:     "refused tenant is removed",
			applied:  []string{"refused", "plain"},
			warnings: []v1alpha1.TenantWarning{{Tenant: "refused", Reason: v1alpha1.TenantReasonItemsApplyFailed, Message: "m"}},
			want:     "plain",
		},
		{
			name:     "bundle keys ignored keeps tenant",
			applied:  []string{"ignored", "plain"},
			warnings: []v1alpha1.TenantWarning{{Tenant: "ignored", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: "m"}},
			want:     "ignored,plain",
		},
		{
			name:     "credential skipped keeps tenant",
			applied:  []string{"skipped", "plain"},
			warnings: []v1alpha1.TenantWarning{{Tenant: "skipped", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: "m"}},
			want:     "skipped,plain",
		},
		{
			name:    "plain tenant is kept",
			applied: []string{"plain", "other"},
			want:    "plain,other",
		},
		{
			name:    "no warnings leaves the list unchanged",
			applied: []string{"b", "a"},
			want:    "b,a",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cell := &v1alpha1.CellStatus{AppliedTenants: tc.applied, TenantWarnings: tc.warnings}
			dropKeptTenants(cell)
			if got := strings.Join(cell.AppliedTenants, ","); got != tc.want {
				t.Errorf("AppliedTenants = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDropKeptTenants_RefusedTenantReentersAfterCleanWalk(t *testing.T) {
	cell := &v1alpha1.CellStatus{
		Tenants:        []string{"ok", "bad"},
		AppliedTenants: []string{"bad", "ok"},
		TenantWarnings: []v1alpha1.TenantWarning{{Tenant: "bad", Reason: v1alpha1.TenantReasonItemsApplyFailed, Message: "m"}},
	}
	dropKeptTenants(cell)
	if got := strings.Join(cell.AppliedTenants, ","); got != "ok" {
		t.Fatalf("after pod change AppliedTenants = %q, want ok", got)
	}
	if got := appliedTenantsAfterItemsApply(cell, []string{"bad/job"}); strings.Join(got, ",") != "ok" {
		t.Errorf("refused walk applied = %v, want [ok]", got)
	}
	if got := appliedTenantsAfterItemsApply(cell, nil); strings.Join(got, ",") != "bad,ok" {
		t.Errorf("clean walk applied = %v, want [bad ok]", got)
	}
}

func TestAssembleCellContent_UnmaterializedBundleHoldsTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	missing := pipelinesTenant("missing")
	missing.Spec.Pipelines.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "absent", Namespace: "catalog-ns"}
	pending := pipelinesTenant("pending")
	crdstore.MustSeed(client.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-bundle", Namespace: "catalog-ns"},
		Status:     v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundlePending},
	})
	pending.Spec.Pipelines.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "pending-bundle", Namespace: "catalog-ns"}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{good, missing, pending}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the good folder pushed, got %v", got)
	}
	if strings.Join(manifest.Retain, ",") != "missing,pending" {
		t.Errorf("expected retain [missing pending], got %v", manifest.Retain)
	}
	if !strings.Contains(heldMessage(state.Warnings, "pending"), "not materialized") {
		t.Errorf("expected an ItemsHeld warning for pending, got %+v", state.Warnings)
	}
}

// libraryTenantItemsYAML is a tenant items.yaml with one folder library on
// a folder the tenant declares. The library's git source names gitlab-lib.
const libraryTenantItemsYAML = `items:
  - kind: folder
    name: apps
    properties:
      - folderLibraries:
          libraries:
            - libraryConfiguration:
                name: platform-lib
                defaultVersion: v1.0.0
                retriever:
                  modernSCM:
                    scm:
                      git:
                        remote: https://gitlab.example.com/platform/jenkins-lib.git
                        credentialsId: gitlab-lib
`

// libraryCoreSet pins the three plugins that the library needs.
var libraryCoreSet = []pluginlock.PluginEntry{
	{ArtifactID: "pipeline-groovy-lib", Version: "1"},
	{ArtifactID: "git", Version: "1"},
	{ArtifactID: "github-branch-source", Version: "1"},
}

func TestAssembleCellContent_LibraryWithoutPluginHoldsTenant(t *testing.T) {
	msg := assertHeldAlone(t, libraryTenantItemsYAML)
	if !strings.Contains(msg, "pipeline-groovy-lib") {
		t.Errorf("expected the hold to name pipeline-groovy-lib, got %q", msg)
	}
}

func TestAssembleCellContent_LibraryUnknownCredentialHoldsTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	payments := pipelinesTenant("payments")
	seedTenantBundle(client, payments, "payments-bundle", libraryTenantItemsYAML)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{payments}, libraryCoreSet, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if got := topLevelNames(parseCellItems(t, state.ItemsYAML)); len(got) != 0 {
		t.Errorf("expected no pushed folder, got %v", got)
	}
	msg := heldMessage(state.Warnings, "payments")
	if !strings.Contains(msg, "gitlab-lib") {
		t.Fatalf("expected a hold naming gitlab-lib, got %q", msg)
	}
	if len(state.FolderCredentials) != 0 {
		t.Errorf("expected no folder credentials carried, got %+v", state.FolderCredentials)
	}
}

func TestAssembleCellContent_LibraryProjectedCredentialPasses(t *testing.T) {
	client := newTestClient()
	seedTenantCredentialSecret(t, client, "gitlab-cred",
		map[string]string{
			"varroa.dev/credential-id":   "gitlab-lib",
			"varroa.dev/credential-kind": "usernamePassword",
		},
		map[string][]byte{"username": []byte("bob"), "password": []byte("hunter2")},
	)
	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	payments := pipelinesTenant("payments")
	seedTenantBundle(client, payments, "payments-bundle", libraryTenantItemsYAML)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{payments}, libraryCoreSet, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if msg := heldMessage(state.Warnings, "payments"); msg != "" {
		t.Fatalf("expected no hold, got %q", msg)
	}
	if !strings.Contains(state.ItemsYAML, "credentialsId: gitlab-lib") {
		t.Errorf("expected ItemsYAML to name gitlab-lib:\n%s", state.ItemsYAML)
	}
}

// tenantTemplateBodyFixtureYAML is tenantTemplateFixtureYAML with the
// jenkinsfile body given by the caller.
func tenantTemplateBodyFixtureYAML(name, body string) string {
	return fmt.Sprintf(`templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: %s
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "%s"
`, name, body)
}

// TestAssembleCellContent_TemplateJobDSLStepHoldsOnlyThatTenant proves a
// template body that runs a Job DSL step holds the tenant that delivers it,
// and no other tenant, and that its fragment is not carried.
func TestAssembleCellContent_TemplateJobDSLStepHoldsOnlyThatTenant(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", cleanPipelineItemsYAML)
	client.configMapData["bad-bundle-content"]["pipeline-templates.yaml"] =
		tenantTemplateBodyFixtureYAML("seed-tmpl", "jobDsl targets: 'jobs/*.groovy'")

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{good, bad}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 1 || held[0] != "bad" {
		t.Fatalf("expected held [bad], got %v: %+v", held, state.Warnings)
	}
	msg := heldMessage(state.Warnings, "bad")
	if !strings.Contains(msg, "no-job-dsl") || !strings.Contains(msg, "seed-tmpl@1.0.0") {
		t.Errorf("expected the message to name no-job-dsl and seed-tmpl@1.0.0, got %q", msg)
	}
	if heldMessage(state.Warnings, "good") != "" {
		t.Errorf("expected good not held, got %+v", state.Warnings)
	}
	for _, f := range state.TemplateFragments {
		if strings.Contains(f.TemplatesYAML, "jobDsl") {
			t.Errorf("expected no fragment carrying the Job DSL body, got %+v", f)
		}
	}
}

// TestAssembleCellContent_TemplateLintDropsOnlyFailingTemplate proves a
// failing template is dropped from a tenant's fragment and the clean
// template stays, and that the tenant is held.
func TestAssembleCellContent_TemplateLintDropsOnlyFailingTemplate(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("payments")
	seedTenantBundle(client, tenant, "payments-bundle", cleanPipelineItemsYAML)
	client.configMapData["payments-bundle-content"]["pipeline-templates.yaml"] = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: go-build
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline { stages { stage('build') { steps { echo 'hi' } } } }"
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: seed-tmpl
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "jobDsl targets: 'jobs/*.groovy'"
`

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 1 || held[0] != "payments" {
		t.Fatalf("expected held [payments], got %v: %+v", held, state.Warnings)
	}
	if len(state.TemplateFragments) != 1 {
		t.Fatalf("expected one fragment, got %+v", state.TemplateFragments)
	}
	frag := state.TemplateFragments[0].TemplatesYAML
	if !strings.Contains(frag, "go-build") || strings.Contains(frag, "seed-tmpl") {
		t.Errorf("expected go-build kept and seed-tmpl dropped, got:\n%s", frag)
	}
}

// TestAssembleCellContent_SCMDefinitionTemplatePassesLint proves a template
// that names a Jenkinsfile in SCM has no body to lint and is not held.
func TestAssembleCellContent_SCMDefinitionTemplatePassesLint(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("scm")
	seedTenantBundle(client, tenant, "scm-bundle", cleanPipelineItemsYAML)
	client.configMapData["scm-bundle-content"]["pipeline-templates.yaml"] = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: scm-build
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      scmDefinition:
        remote: https://git.example.test/team/build.git
        credentialsId: git-cred
        scriptPath: Jenkinsfile
`

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if held := parseCellItems(t, state.ItemsYAML).Retain; len(held) != 0 {
		t.Errorf("expected no held tenant, got %v: %+v", held, state.Warnings)
	}
}

// TestAssembleCellContent_CleanTemplateFragmentUnchanged proves a fragment
// that passes the lint reaches the cell byte for byte.
func TestAssembleCellContent_CleanTemplateFragmentUnchanged(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	tenant := pipelinesTenant("clean")
	seedTenantBundle(client, tenant, "clean-bundle", cleanPipelineItemsYAML)
	fixture := tenantTemplateFixtureYAML("go-build")
	client.configMapData["clean-bundle-content"]["pipeline-templates.yaml"] = fixture

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{tenant}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if len(state.TemplateFragments) != 1 {
		t.Fatalf("expected one fragment, got %+v", state.TemplateFragments)
	}
	if got := state.TemplateFragments[0].TemplatesYAML; got != fixture {
		t.Errorf("expected the fragment unchanged, got:\n%s", got)
	}
}

func TestAssembleCellContent_UnknownKeyHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, `items:
  - kind: freeStyle
    name: build
    builders:
      - jobDsl:
          targets: x
`)
	for _, want := range []string{"jobDsl", `"build"`, "builders[0].jobDsl"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected the message to contain %q, got %q", want, msg)
		}
	}
}

func TestAssembleCellContent_DuplicateKeyHoldsOnlyThatTenant(t *testing.T) {
	msg := assertHeldAlone(t, `items:
  - kind: freeStyle
    name: one
    name: two
`)
	if !strings.Contains(msg, "parse tenant items.yaml") {
		t.Errorf("expected a parse fault in the message, got %q", msg)
	}
}

// assertDroppedItemKeys seeds two tenants with clean items and a dropped-key
// list keysYAML on bad. It returns the assembled state.
func assertDroppedItemKeys(t *testing.T, keysYAML string) assembleCellState {
	t.Helper()
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	good := pipelinesTenant("good")
	seedTenantBundle(client, good, "good-bundle", cleanPipelineItemsYAML)
	bad := pipelinesTenant("bad")
	seedTenantBundle(client, bad, "bad-bundle", cleanPipelineItemsYAML)
	client.configMapData["bad-bundle-content"][bundle.DroppedItemKeysConfigMapKey] = keysYAML

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{good, bad}, nil, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	return state
}

func TestAssembleCellContent_DroppedUnknownTopLevelKeyHoldsOnlyThatTenant(t *testing.T) {
	state := assertDroppedItemKeys(t, "- source: itemRef \"x\"\n  key: item\n")
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the good folder pushed, got %v", got)
	}
	msg := heldMessage(state.Warnings, "bad")
	if !strings.Contains(msg, `"item"`) || !strings.Contains(msg, `itemRef "x"`) {
		t.Errorf("expected the message to name key item and its source, got %q", msg)
	}
}

func TestAssembleCellContent_DroppedRootHoldsOnlyThatTenant(t *testing.T) {
	state := assertDroppedItemKeys(t, "- source: git input[0] (repo)\n  key: root\n")
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the good folder pushed, got %v", got)
	}
	msg := heldMessage(state.Warnings, "bad")
	if !strings.Contains(msg, `"root"`) {
		t.Errorf("expected the message to name root, got %q", msg)
	}
}

func TestAssembleCellContent_DroppedRemoveStrategyWarnsWithoutHold(t *testing.T) {
	state := assertDroppedItemKeys(t, "- source: itemRef \"x\"\n  key: removeStrategy\n")
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 2 {
		t.Errorf("expected both folders pushed, got %v", got)
	}
	if heldMessage(state.Warnings, "bad") != "" {
		t.Errorf("expected no hold for removeStrategy, got %+v", state.Warnings)
	}
	var found bool
	for _, w := range state.Warnings {
		if w.Tenant == "bad" && w.Reason == v1alpha1.TenantReasonBundleKeysIgnored && strings.Contains(w.Message, `"removeStrategy"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a BundleKeysIgnored warning for removeStrategy, got %+v", state.Warnings)
	}
}

func TestAssembleCellContent_UnparseableDroppedItemKeysHoldsTenant(t *testing.T) {
	state := assertDroppedItemKeys(t, "- [unclosed\n")
	manifest := parseCellItems(t, state.ItemsYAML)
	if got := topLevelNames(manifest); len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the good folder pushed, got %v", got)
	}
	if heldMessage(state.Warnings, "bad") == "" {
		t.Errorf("expected the bad tenant held, got %+v", state.Warnings)
	}
}

// seedBigSecretFiles seeds n tenant secretFile Secrets of 64 KiB each in
// tenant's agent namespace. Their IDs are "<tenant>-f<i>".
func seedBigSecretFiles(t *testing.T, client *testClient, tenant string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-f%d", tenant, i)
		seedTenantCredentialSecretIn(t, client, tenant+"-ns", id,
			map[string]string{"varroa.dev/credential-id": id, "varroa.dev/credential-kind": "secretFile"},
			map[string][]byte{"file": make([]byte, maxBinaryCredentialBytes)})
	}
}

// carriedCredentialIDs lists the IDs of the credentials state carries for
// folder, in order.
func carriedCredentialIDs(state assembleCellState, folder string) []string {
	var ids []string
	for _, cf := range state.FolderCredentials {
		if cf.Folder != folder {
			continue
		}
		for _, e := range cf.Entries {
			for _, c := range e.Credentials {
				id, _ := folderCredentialIDKind(c)
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// carriedWireBytes sums the wire bytes of every credential state carries.
func carriedWireBytes(t *testing.T, state assembleCellState) int {
	t.Helper()
	total := 0
	for _, cf := range state.FolderCredentials {
		for _, e := range cf.Entries {
			for _, c := range e.Credentials {
				total += budgetTestSize(t, c)
			}
		}
	}
	return total
}

func TestAssembleCellContent_CredentialBudgetSkipsInTenantNameOrder(t *testing.T) {
	client := newTestClient()
	cr := cellControllerFixture()
	beta, alpha := pipelinesTenant("beta"), pipelinesTenant("alpha")
	seedTenantBundleContent(client, beta, "beta-bundle", nil)
	seedTenantBundleContent(client, alpha, "alpha-bundle", nil)
	seedBigSecretFiles(t, client, "beta", 4)
	seedBigSecretFiles(t, client, "alpha", 4)
	rec := newTestReconciler(client)

	run := func() assembleCellState {
		state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{beta, alpha}, nil, "")
		if err != nil {
			t.Fatalf("assembleCellContent: %v", err)
		}
		return state
	}
	state := run()

	if got := carriedCredentialIDs(state, "alpha"); len(got) != 4 {
		t.Fatalf("expected all four of alpha's credentials carried, got %v", got)
	}
	betaKept := carriedCredentialIDs(state, "beta")
	skipped := tenantWarningsFor(state.Warnings, "beta", string(v1alpha1.TenantReasonCredentialSkipped))
	if len(betaKept) == 0 || len(betaKept) == 4 {
		t.Fatalf("expected beta to keep some but not all credentials, kept %v", betaKept)
	}
	if len(skipped) != 4-len(betaKept) {
		t.Errorf("expected %d CredentialSkipped warnings for beta, got %v", 4-len(betaKept), skipped)
	}
	if total := carriedWireBytes(t, state); total > cellCredentialBudgetBytes {
		t.Errorf("carried %d wire bytes, over the %d-byte budget", total, cellCredentialBudgetBytes)
	}
	if again := carriedCredentialIDs(run(), "beta"); !slices.Equal(again, betaKept) {
		t.Errorf("expected the same beta skips on a second run, got %v then %v", betaKept, again)
	}
}

func TestAssembleCellContent_CredentialBudgetSkipHoldsTenantWhoseLibraryNamesIt(t *testing.T) {
	client := newTestClient()
	cr := cellControllerFixture()
	alpha := pipelinesTenant("alpha")
	seedTenantBundleContent(client, alpha, "alpha-bundle", nil)
	// Six 64 KiB files: the budget carries five and skips the sixth.
	seedBigSecretFiles(t, client, "alpha", 6)

	zeta := pipelinesTenant("zeta")
	seedTenantBundle(client, zeta, "zeta-bundle", libraryTenantItemsYAML)
	seedTenantCredentialSecretIn(t, client, "zeta-ns", "gitlab-lib-file",
		map[string]string{"varroa.dev/credential-id": "gitlab-lib", "varroa.dev/credential-kind": "secretFile"},
		map[string][]byte{"file": make([]byte, maxBinaryCredentialBytes)})

	rec := newTestReconciler(client)
	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{zeta, alpha}, libraryCoreSet, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	// Alpha's five carried files leave too little budget for zeta's file.
	if got := carriedCredentialIDs(state, "alpha"); len(got) != 5 {
		t.Fatalf("expected alpha to carry five files and use the budget, got %v", got)
	}
	if got := tenantWarningsFor(state.Warnings, "zeta", string(v1alpha1.TenantReasonCredentialSkipped)); len(got) != 0 {
		t.Errorf("expected no skip warning for a held tenant, got %v", got)
	}
	msg := heldMessage(state.Warnings, "zeta")
	if !strings.Contains(msg, "gitlab-lib") {
		t.Fatalf("expected zeta held naming gitlab-lib, got %q", msg)
	}
}

func TestAssembleCellContent_HeldTenantRefundsCredentialBudget(t *testing.T) {
	client := newTestClient()
	cr := cellControllerFixture()
	// alpha's library names a credential that no Secret carries, so alpha
	// is held after its credentials are charged to the budget.
	alpha := pipelinesTenant("alpha")
	seedTenantBundle(client, alpha, "alpha-bundle", libraryTenantItemsYAML)
	seedBigSecretFiles(t, client, "alpha", 6)

	zeta := pipelinesTenant("zeta")
	seedTenantBundleContent(client, zeta, "zeta-bundle", nil)
	seedBigSecretFiles(t, client, "zeta", 5)

	rec := newTestReconciler(client)
	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Tenant{alpha, zeta}, libraryCoreSet, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}
	if msg := heldMessage(state.Warnings, "alpha"); msg == "" {
		t.Fatalf("expected alpha held by its library check")
	}
	if got := carriedCredentialIDs(state, "zeta"); len(got) != 5 {
		t.Errorf("expected all five of zeta's credentials after alpha's refund, got %v", got)
	}
}
