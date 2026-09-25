package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedTenantBundle seeds a Ready ComposedBundle plus its materialized
// content ConfigMap for a tenant, and points the Team's
// spec.pipelines.composedBundleRef at it.
func seedTenantBundle(client *testClient, team *v1alpha1.Team, bundleName, itemsYAML string) {
	crdstore.MustSeed(client.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: bundleName, Namespace: "catalog-ns"},
		Status:     v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundleReady, ContentRef: bundleName + "-content"},
	})
	client.configMapData[bundleName+"-content"] = map[string]string{"items.yaml": itemsYAML}
	team.Spec.Pipelines.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: bundleName, Namespace: "catalog-ns"}
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

	clean := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "clean"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"clean-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "clean-ns"},
		},
	}
	seedTenantBundle(client, clean, "clean-bundle", cleanPipelineItemsYAML)

	draining := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "draining"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"draining-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "draining-ns"},
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: cr.Name, CellNamespace: cr.Namespace},
			Conditions: []v1alpha1.TeamCondition{
				{Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TeamReasonTenantMoving},
			},
		},
	}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{clean, draining}, "http://cell1-svc.ns1.svc.cluster.local:8080")
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

// TestAssembleCellContent_LintRejectionBlocksTheWholePush is the other half
// of task 8.12's end-to-end acceptance: over three tenants — one draining,
// one with a rejected item (a pollSCM trigger), and one clean — the
// rejected tenant's finding fails the whole assembly. The operator pushes
// no items, and the error names the offending item's path.
func TestAssembleCellContent_LintRejectionBlocksTheWholePush(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	clean := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "clean"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"clean-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "clean-ns"},
		},
	}
	seedTenantBundle(client, clean, "clean-bundle", cleanPipelineItemsYAML)

	draining := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "draining"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"draining-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "draining-ns"},
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: cr.Name, CellNamespace: cr.Namespace},
			Conditions: []v1alpha1.TeamCondition{
				{Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TeamReasonTenantMoving},
			},
		},
	}

	rejected := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "rejected"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"rejected-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "rejected-ns"},
		},
	}
	seedTenantBundle(client, rejected, "rejected-bundle", offendingPipelineItemsYAML)

	_, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{clean, draining, rejected},
		"http://cell1-svc.ns1.svc.cluster.local:8080")
	if err == nil {
		t.Fatal("expected the rejected tenant's finding to fail the whole assembly")
	}
	if !strings.Contains(err.Error(), "no-poll-scm") {
		t.Errorf("expected the error to name the rule no-poll-scm, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rejected/offender") {
		t.Errorf("expected the error to name the offending item path rejected/offender, got: %v", err)
	}

	// applyCellContent must push nothing when assembly fails. It reads
	// teams through the crdstore by status.cell.tenants, same as production.
	crdstore.MustSeed(client.store, clean)
	crdstore.MustSeed(client.store, draining)
	crdstore.MustSeed(client.store, rejected)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"clean", "draining", "rejected"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: untouched\n"}
	if err := rec.applyCellContent(context.Background(), cr, mat, "http://cell1-svc.ns1.svc.cluster.local:8080"); err == nil {
		t.Fatal("expected applyCellContent to fail too")
	}
	if mat.ItemsYAML != "" {
		t.Errorf("expected no items pushed on a lint failure, got %q", mat.ItemsYAML)
	}
}

// TestAssembleCellContent_TemplateFragmentsGetOwnFolders is task 3.7's
// acceptance for task 3.6: two placed tenants each carry their own template
// content, and each gets its own Folder value in the returned fragments.
func TestAssembleCellContent_TemplateFragmentsGetOwnFolders(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	teamA := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, teamA, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	teamB := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}
	seedTenantBundle(client, teamB, "team-b-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-b-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("node-service")

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{teamA, teamB},
		"http://cell1-svc.ns1.svc.cluster.local:8080")
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
// and lands in the rendered jenkins.yaml, folder-stamped with the Team name
// (design D7, tenant-isolation).
func TestResolveBundleForController_CellTenantTemplateBodySurvivesInjection(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, team, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")
	crdstore.MustSeed(client.store, team)
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

	teamA := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	seedTenantBundle(client, teamA, "team-a-bundle", cleanPipelineItemsYAML)
	client.configMapData["team-a-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	teamB := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}
	seedTenantBundle(client, teamB, "team-b-bundle", cleanPipelineItemsYAML)
	// Same name, version, and content as team-a's fragment: only the
	// folder differs once each fragment is stamped.
	client.configMapData["team-b-bundle-content"]["pipeline-templates.yaml"] = tenantTemplateFixtureYAML("go-service")

	crdstore.MustSeed(client.store, teamA)
	crdstore.MustSeed(client.store, teamB)
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

	teamA := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-a-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-a-ns"},
		},
	}
	teamB := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"team-b-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "team-b-ns"},
		},
	}

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{teamA, teamB},
		"http://cell1-svc.ns1.svc.cluster.local:8080")
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
func seedTenantBundleContent(client *testClient, team *v1alpha1.Team, bundleName string, extra map[string]string) {
	seedTenantBundle(client, team, bundleName, cleanPipelineItemsYAML)
	for k, v := range extra {
		client.configMapData[bundleName+"-content"][k] = v
	}
}

// pipelinesTeam builds a pipelines tenant whose agent namespace is
// "<name>-ns".
func pipelinesTeam(name string) *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{name + "-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: name + "-ns"},
		},
	}
}

// markDraining gives team the Placed=False/TenantMoving condition and a
// placement on cr, which makes it a draining tenant of cr.
func markDraining(team *v1alpha1.Team, cr *v1alpha1.Controller) {
	team.Status = v1alpha1.TeamStatus{
		Placement: &v1alpha1.TeamPlacementStatus{Cell: cr.Name, CellNamespace: cr.Namespace},
		Conditions: []v1alpha1.TeamCondition{
			{Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse, Reason: v1alpha1.TeamReasonTenantMoving},
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

	golang := pipelinesTeam("golang")
	seedTenantBundleContent(client, golang, "golang-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})

	residue := pipelinesTeam("residue")
	seedTenantBundleContent(client, residue, "residue-bundle", map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: hi\n",
	})

	plugins := pipelinesTeam("plugins")
	seedTenantBundleContent(client, plugins, "plugins-bundle", map[string]string{"plugins.yaml": "plugins:\n  - git\n"})

	skipped := pipelinesTeam("skipped")
	seedTenantBundleContent(client, skipped, "skipped-bundle", map[string]string{
		"jenkins.yaml": minimalCloud("- name: default\n- name: node\n"),
	})

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{golang, residue, plugins, skipped}, "")
	if err != nil {
		t.Fatalf("assembleCellContent: %v", err)
	}

	if got := cloudTemplateNames(state, "golang"); len(got) != 2 || got[0] != "golang-default" || got[1] != "golang-golang" {
		t.Errorf("golang cloud templates = %v, want [golang-default golang-golang]", got)
	}
	if got := tenantWarningsFor(state.Warnings, "golang", v1alpha1.TeamReasonBundleKeysIgnored); len(got) != 0 {
		t.Errorf("expected no BundleKeysIgnored for a pod-template-only jenkins.yaml, got %q", got)
	}

	wantResidue := []string{`tenant bundle key "jenkins.yaml" is ignored on a cell except its pod templates: jenkins.systemMessage`}
	if got := tenantWarningsFor(state.Warnings, "residue", v1alpha1.TeamReasonBundleKeysIgnored); !equalStrings(got, wantResidue) {
		t.Errorf("residue warnings = %q, want %q", got, wantResidue)
	}

	wantPlugins := []string{`tenant bundle key "plugins.yaml" is ignored on a cell`}
	if got := tenantWarningsFor(state.Warnings, "plugins", v1alpha1.TeamReasonBundleKeysIgnored); !equalStrings(got, wantPlugins) {
		t.Errorf("plugins warnings = %q, want %q", got, wantPlugins)
	}

	if got := cloudTemplateNames(state, "skipped"); len(got) != 2 || got[1] != "skipped-node" {
		t.Errorf("skipped cloud templates = %v, want [skipped-default skipped-node]", got)
	}
	wantSkip := []string{`pod template "default": the name "default" is reserved`}
	if got := tenantWarningsFor(state.Warnings, "skipped", v1alpha1.TeamReasonPodTemplateSkipped); !equalStrings(got, wantSkip) {
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

	draining := pipelinesTeam("draining")
	seedTenantBundleContent(client, draining, "draining-bundle", map[string]string{
		"jenkins.yaml": "unclassified: {}\n" + minimalCloud(golangPodTemplate),
	})
	markDraining(draining, cr)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{draining}, "")
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
	if got := tenantWarningsFor(state.Warnings, "draining", v1alpha1.TeamReasonBundleKeysIgnored); !equalStrings(got, want) {
		t.Errorf("draining warnings = %q, want %q", got, want)
	}
}

func TestAssembleCellContent_DrainingTenantUnreadableBundle(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	draining := pipelinesTeam("draining")
	seedTenantBundleContent(client, draining, "draining-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})
	delete(client.configMapData, "draining-bundle-content")
	markDraining(draining, cr)

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{draining}, "")
	if err != nil {
		t.Fatalf("expected the assembly to succeed, got %v", err)
	}
	if got := cloudTemplateNames(state, "draining"); len(got) != 1 || got[0] != "draining-default" {
		t.Errorf("draining cloud templates = %v, want [draining-default]", got)
	}
}

func TestAssembleCellContent_ActiveTenantUnreadableBundleFails(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	active := pipelinesTeam("active")
	seedTenantBundleContent(client, active, "active-bundle", nil)
	delete(client.configMapData, "active-bundle-content")

	if _, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{active}, ""); err == nil {
		t.Fatal("expected the unreadable bundle of a non-draining tenant to fail the assembly")
	}
}

func TestAssembleCellContent_UnparseableJenkinsYAML(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellControllerFixture()

	broken := pipelinesTeam("broken")
	seedTenantBundleContent(client, broken, "broken-bundle", map[string]string{"jenkins.yaml": "jenkins: [unclosed\n"})

	state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{broken}, "")
	if err != nil {
		t.Fatalf("expected the assembly to succeed, got %v", err)
	}
	if got := cloudTemplateNames(state, "broken"); len(got) != 1 || got[0] != "broken-default" {
		t.Errorf("broken cloud templates = %v, want [broken-default]", got)
	}
	got := tenantWarningsFor(state.Warnings, "broken", v1alpha1.TeamReasonBundleKeysIgnored)
	if len(got) != 1 || !strings.HasPrefix(got[0], `tenant bundle key "jenkins.yaml" does not parse: `) {
		t.Errorf("unexpected warnings %q", got)
	}
}

func TestAssembleCellContent_EveryMemberCellRendersTheSameTemplates(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	team := pipelinesTeam("payments")
	seedTenantBundleContent(client, team, "payments-bundle", map[string]string{"jenkins.yaml": minimalCloud(golangPodTemplate)})
	slot0, slot1 := int32(0), int32(1)
	team.Status.Placement = &v1alpha1.TeamPlacementStatus{
		Cell: "cell-0", CellNamespace: "ns1",
		Members: []v1alpha1.TeamMemberStatus{
			{Cell: "cell-0", CellNamespace: "ns1", Role: v1alpha1.TeamMemberRoleActive, Slot: &slot0},
			{Cell: "cell-1", CellNamespace: "ns1", Role: v1alpha1.TeamMemberRoleActive, Slot: &slot1},
			{Cell: "cell-2", CellNamespace: "ns1", Role: v1alpha1.TeamMemberRoleStandby},
		},
	}

	for _, cell := range []string{"cell-0", "cell-1", "cell-2"} {
		cr := cellControllerFixture()
		cr.Name = cell
		state, err := rec.assembleCellContent(context.Background(), cr, []*v1alpha1.Team{team}, "")
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

	evil := pipelinesTeam("evil")
	seedTenantBundleContent(client, evil, "evil-bundle", map[string]string{
		"jenkins.yaml": minimalCloud("- name: steal\n  yaml: ${readFile:/x}\n"),
	})
	good := pipelinesTeam("good")
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
	got := tenantWarningsFor(cr.Status.Cell.TenantWarnings, "evil", v1alpha1.TeamReasonPodTemplateSkipped)
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

	team := pipelinesTeam("payments")
	seedTenantBundleContent(client, team, "payments-bundle", map[string]string{
		"jenkins.yaml":   minimalCloud("- name: golang\n  containers:\n    - name: golang\n      image: ${golang_image}\n"),
		"variables.yaml": "golang_image: golang:1.26-bookworm\n",
	})
	crdstore.MustSeed(client.store, team)
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}

	mat, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	if !strings.Contains(mat.JenkinsYAML, "image: golang:1.26-bookworm") {
		t.Errorf("expected the resolved image in the cell jenkins.yaml, got: %s", mat.JenkinsYAML)
	}
}
