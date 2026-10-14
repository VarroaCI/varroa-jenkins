package api

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// tenantLintFindingItems holds a freeStyle job with a pollSCM trigger. The
// cell lint rule no-poll-scm rejects it, so the cell holds the tenant.
const tenantLintFindingItems = `items:
  - kind: freeStyle
    name: job
    triggers:
      - pollSCM: {}
`

// tenantCleanItems declares one freeStyle job that passes every cell check.
const tenantCleanItems = `items:
  - kind: freeStyle
    name: job
    builders:
      - shell:
          command: echo hi
`

// tenantFailingTemplates delivers one template whose inline jenkinsfile calls
// a Job DSL step. The cell lint rule no-job-dsl rejects it.
const tenantFailingTemplates = `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: bad
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "node { jobDsl targets: 'jobs/*.groovy' }"
`

// droppedKeysYAML returns the dropped-item-keys.yaml value for one key.
func droppedKeysYAML(key string) string {
	return "- source: itemRef \"x\"\n  key: " + key + "\n"
}

// placedHoldRef is the placed Team payments on the cell cell-1.
var placedHoldRef = TenantRef{Folder: "payments", Team: "payments", Controller: "cell-1", Namespace: "test-ns"}

// TestControllerItemManifest_PlacedTeamLintFindingGivesNoManifest asserts a
// placed Team whose items fail a cell lint rule declares no job, as the cell
// holds that tenant.
func TestControllerItemManifest_PlacedTeamLintFindingGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantLintFindingItems}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the cell holds a tenant whose items fail no-poll-scm", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamDroppedUnknownKeyGivesNoManifest
// asserts a dropped unknown top-level key holds the read as it holds the cell.
func TestControllerItemManifest_PlacedTeamDroppedUnknownKeyGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{
		"items.yaml":                       tenantCleanItems,
		bundle.DroppedItemKeysConfigMapKey: droppedKeysYAML("item"),
	}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the cell drops an unknown top-level key", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamDroppedRootGivesNoManifest asserts a
// tenant that sets root declares no job.
func TestControllerItemManifest_PlacedTeamDroppedRootGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{
		"items.yaml":                       tenantCleanItems,
		bundle.DroppedItemKeysConfigMapKey: droppedKeysYAML("root"),
	}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: a tenant may not set root", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamDroppedDeclaredKeyKeepsManifest
// asserts a declared key that the cell ignores keeps the definition.
func TestControllerItemManifest_PlacedTeamDroppedDeclaredKeyKeepsManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{
		"items.yaml":                       tenantCleanItems,
		bundle.DroppedItemKeysConfigMapKey: droppedKeysYAML("removeStrategy"),
	}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one re-rooted folder", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamTemplateLintGivesNoManifest asserts a
// template that fails a cell lint rule holds the tenant's declared jobs.
func TestControllerItemManifest_PlacedTeamTemplateLintGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{
		"items.yaml":              tenantCleanItems,
		"pipeline-templates.yaml": tenantFailingTemplates,
	}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the cell holds a tenant whose template fails no-job-dsl", manifest)
	}
}

// TestControllerItemManifest_SyntheticTenantSkipsCellLint asserts a
// single-controller tenant keeps its definition even when its items would
// fail a cell lint rule: only a placed Team runs the cell checks.
func TestControllerItemManifest_SyntheticTenantSkipsCellLint(t *testing.T) {
	client := newMultibranchControllerFixture()
	client.configMaps["bundle-a-content"] = map[string]string{"items.yaml": tenantLintFindingItems}
	srv := definitionTestServer(client)

	ref := TenantRef{Controller: "ctrl-a", Namespace: "test-ns"}
	_, _, manifest, err := srv.controllerItemManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("manifest = nil, want the synthetic tenant's definition")
	}
}

// TestControllerItemManifest_MemberPlacedTeamLintFindingGivesNoManifest
// asserts the member-placed read applies the same cell lint hold.
func TestControllerItemManifest_MemberPlacedTeamLintFindingGivesNoManifest(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": tenantLintFindingItems}},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the member cell holds a tenant whose items fail no-poll-scm", manifest)
	}
}

// TestControllerItemManifest_MemberPlacedTeamDroppedRootGivesNoManifest
// asserts the member-placed read refuses a tenant that sets root.
func TestControllerItemManifest_MemberPlacedTeamDroppedRootGivesNoManifest(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{
			"items.yaml":                       tenantCleanItems,
			bundle.DroppedItemKeysConfigMapKey: droppedKeysYAML("root"),
		}},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: a tenant may not set root", manifest)
	}
}

// TestListPipelines_PlacedTeamLintHoldDeclaresNoJob asserts the pipelines
// listing declares no job for a placed Team the cell holds for a lint fault.
func TestListPipelines_PlacedTeamLintHoldDeclaresNoJob(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantLintFindingItems}
	reader := newFakeRecordReader()
	reader.jobSummaries["core/payments"] = []recordstore.JobSummary{
		{JobPath: "payments/job", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, resolverAuthorizer("payments"), reader, nil)

	resp := listPipelinesFor(t, srv, "payments")
	if got := kindOf(t, resp, "payments/job"); got != "unknown" {
		t.Fatalf("kind = %q, want unknown: the held tenant declares no job", got)
	}
}
