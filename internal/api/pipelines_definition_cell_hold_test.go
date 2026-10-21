package api

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
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

// itemsHeldOn returns a cell status that records an ItemsHeld hold for tenant.
func itemsHeldOn(tenant string) *v1alpha1.CellStatus {
	return &v1alpha1.CellStatus{TenantWarnings: []v1alpha1.TenantWarning{{
		Tenant: tenant, Reason: v1alpha1.TeamReasonItemsHeld, Message: "tenant folder held: plugin not pinned",
	}}}
}

// TestControllerItemManifest_PlacedTeamCellHoldGivesNoManifest asserts a
// placed Team whose items pass every local check declares no job while its
// cell records an ItemsHeld hold for the tenant.
func TestControllerItemManifest_PlacedTeamCellHoldGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-1"].Status.Cell = itemsHeldOn("payments")
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the cell holds the tenant", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamOtherTenantHoldKeepsManifest asserts
// a hold that names another tenant does not change this tenant's read.
func TestControllerItemManifest_PlacedTeamOtherTenantHoldKeepsManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-1"].Status.Cell = itemsHeldOn("other")
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("manifest = nil, want the payments definition")
	}
}

// TestControllerItemManifest_PlacedTeamNonHoldWarningKeepsManifest asserts a
// warning that is not an ItemsHeld hold does not hide the tenant's jobs.
func TestControllerItemManifest_PlacedTeamNonHoldWarningKeepsManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-1"].Status.Cell = &v1alpha1.CellStatus{TenantWarnings: []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonBundleKeysIgnored, Message: "key ignored",
	}}}
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("manifest = nil, want the payments definition")
	}
}

// TestControllerItemManifest_MemberPlacedTeamHeldGivesNoManifest asserts the
// member-placed read declares no job while the member's cell holds the Team.
func TestControllerItemManifest_MemberPlacedTeamHeldGivesNoManifest(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": tenantCleanItems}, held: true},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the member cell holds the tenant", manifest)
	}
}

// TestListPipelines_PlacedTeamCellHoldDeclaresNoJob asserts the pipelines
// listing declares no job for a placed Team whose cell holds the tenant.
func TestListPipelines_PlacedTeamCellHoldDeclaresNoJob(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-1"].Status.Cell = itemsHeldOn("payments")
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

// newPlacedTeamMembersFixture places payments with an active slot-1 member on
// cell-2 and a standby member on cell-3, both in test-ns, and cell-1 as slot 0.
func newPlacedTeamMembersFixture(t *testing.T) *fakeResourceClient {
	t.Helper()
	client := newPlacedTeamFixture(t)
	client.controllers["cell-2"] = &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-2", Namespace: "test-ns"}}
	client.controllers["cell-3"] = &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-3", Namespace: "test-ns"}}
	team, err := crdstore.Get[v1alpha1.Team](context.Background(), client.crdStore, "payments", "")
	if err != nil {
		t.Fatalf("get team: %v", err)
	}
	slot1 := int32(1)
	team.Status.Placement.Members = []v1alpha1.TeamMemberStatus{
		{Cell: "cell-1", CellNamespace: "test-ns", Slot: new(int32)},
		{Cell: "cell-2", CellNamespace: "test-ns", Slot: &slot1},
		{Cell: "cell-3", CellNamespace: "test-ns"},
	}
	if err := crdstore.UpdateStatus(context.Background(), client.crdStore, team); err != nil {
		t.Fatalf("place team members: %v", err)
	}
	return client
}

// TestControllerItemManifest_PlacedTeamHeldOnNonSlotZeroMemberGivesNoManifest
// asserts a hold on an active member cell other than slot 0 gives no manifest.
func TestControllerItemManifest_PlacedTeamHeldOnNonSlotZeroMemberGivesNoManifest(t *testing.T) {
	client := newPlacedTeamMembersFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-2"].Status.Cell = itemsHeldOn("payments")
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: a member cell holds the tenant", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamHeldOnStandbyMemberGivesNoManifest
// asserts a hold on a standby member cell gives no manifest.
func TestControllerItemManifest_PlacedTeamHeldOnStandbyMemberGivesNoManifest(t *testing.T) {
	client := newPlacedTeamMembersFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-3"].Status.Cell = itemsHeldOn("payments")
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: a standby cell holds the tenant", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamNoMemberHoldKeepsManifest asserts no
// hold on any member cell keeps the manifest.
func TestControllerItemManifest_PlacedTeamNoMemberHoldKeepsManifest(t *testing.T) {
	client := newPlacedTeamMembersFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantCleanItems}
	client.controllers["cell-2"].Status.Cell = itemsHeldOn("other")
	srv := definitionTestServer(client)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), placedHoldRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("manifest = nil, want the payments definition")
	}
}
