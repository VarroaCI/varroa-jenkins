package crdstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestRunnerRegistered proves the Runner kind resolves through the registry
// as a cluster-scoped kind.
func TestRunnerRegistered(t *testing.T) {
	gvr, err := GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor[Runner]: %v", err)
	}
	if gvr.Group != "varroa.dev" || gvr.Version != "v1alpha1" || gvr.Resource != "runners" {
		t.Errorf("unexpected GVR: %+v", gvr)
	}

	info, err := gvrInfo[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("gvrInfo[Runner]: %v", err)
	}
	if info.namespaced {
		t.Error("Runner must be cluster-scoped")
	}
	if info.kind != "Runner" {
		t.Errorf("kind = %q, want Runner", info.kind)
	}
}

// TestClearStatusFields_RunnerClearsCellFields implements task 2.5's
// acceptance: a unit test writes a status with both fields, then patches a
// status without them, and reads both back empty. cell and cellNamespace are
// omitempty, so a plain marshal of an empty RunnerStatus drops both keys and
// a merge patch would leave the stale values in place — the merge patch must
// force-null them instead, mirroring ClientsetClient.PatchObjectStatus's
// "fill in every clearStatusFields key absent from the marshaled status"
// step.
func TestClearStatusFields_RunnerClearsCellFields(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	gvr, err := GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor[Runner]: %v", err)
	}

	MustSeed(f, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-win-1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "ssh", RemoteFS: "/home/jenkins"},
	})

	// Write a status with both fields set.
	if err := PatchStatus[v1alpha1.Runner](ctx, f, "payments-win-1", "", &v1alpha1.RunnerStatus{
		Cell:          "cell-a",
		CellNamespace: "varroa-system",
	}); err != nil {
		t.Fatalf("PatchStatus (set): %v", err)
	}
	got, err := Get[v1alpha1.Runner](ctx, f, "payments-win-1", "")
	if err != nil {
		t.Fatalf("Get (after set): %v", err)
	}
	if got.Status.Cell != "cell-a" || got.Status.CellNamespace != "varroa-system" {
		t.Fatalf("status after set = %+v, want cell=cell-a cellNamespace=varroa-system", got.Status)
	}

	// Patch a status that omits both fields, force-nulling the clearable
	// keys the way ClientsetClient.PatchObjectStatus does.
	empty := &v1alpha1.RunnerStatus{}
	statusJSON, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty status: %v", err)
	}
	statusMap := map[string]any{}
	if err := json.Unmarshal(statusJSON, &statusMap); err != nil {
		t.Fatalf("unmarshal empty status: %v", err)
	}
	ApplyStatusClears(gvr, statusMap)
	if err := f.PatchObjectStatus(ctx, gvr, "", "payments-win-1", statusMap); err != nil {
		t.Fatalf("PatchObjectStatus (clear): %v", err)
	}

	got, err = Get[v1alpha1.Runner](ctx, f, "payments-win-1", "")
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if got.Status.Cell != "" {
		t.Errorf("status.cell = %q, want empty", got.Status.Cell)
	}
	if got.Status.CellNamespace != "" {
		t.Errorf("status.cellNamespace = %q, want empty", got.Status.CellNamespace)
	}
}

// TestClearStatusFields_TeamClearsPipelines asserts a Team status patch that
// omits pipelines force-nulls it: the door token and secret of a tenant
// that left pipelines must not survive in etcd to become valid again when
// pipelines is re-enabled.
func TestClearStatusFields_TeamClearsPipelines(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	gvr, err := GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor[Team]: %v", err)
	}
	MustSeed(f, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}})
	if err := PatchStatus[v1alpha1.Team](ctx, f, "payments", "", &v1alpha1.TeamStatus{
		Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec"},
	}); err != nil {
		t.Fatalf("PatchStatus (set): %v", err)
	}
	statusMap := map[string]any{}
	ApplyStatusClears(gvr, statusMap)
	if err := f.PatchObjectStatus(ctx, gvr, "", "payments", statusMap); err != nil {
		t.Fatalf("PatchObjectStatus (clear): %v", err)
	}
	got, err := Get[v1alpha1.Team](ctx, f, "payments", "")
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if got.Status.Pipelines != nil {
		t.Fatalf("status.pipelines = %+v, want nil after the clearing patch", got.Status.Pipelines)
	}
}

// TestCellPoolRegistered proves the CellPool kind resolves through the
// registry as a cluster-scoped kind.
func TestCellPoolRegistered(t *testing.T) {
	gvr, err := GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor[CellPool]: %v", err)
	}
	if gvr.Group != "varroa.dev" || gvr.Version != "v1alpha1" || gvr.Resource != "cellpools" {
		t.Errorf("unexpected GVR: %+v", gvr)
	}

	info, err := gvrInfo[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("gvrInfo[CellPool]: %v", err)
	}
	if info.namespaced {
		t.Error("CellPool must be cluster-scoped")
	}
	if info.kind != "CellPool" {
		t.Errorf("kind = %q, want CellPool", info.kind)
	}
}

// TestCellPoolRoundTrip covers the cluster-scoped create, get, list, and
// status patch a pool reconciler performs each tick.
func TestCellPoolRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	pool := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lts-2-555"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        "lts-2-555",
			Warm:              2,
			MaxTenantsPerCell: 10,
		},
	}
	if err := Create(ctx, f, pool); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := Get[v1alpha1.CellPool](ctx, f, "lts-2-555", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.ProfileRef != "lts-2-555" || got.Spec.Warm != 2 {
		t.Fatalf("spec after create = %+v", got.Spec)
	}

	pools, err := List[v1alpha1.CellPool](ctx, f, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("List returned %d pools, want 1", len(pools))
	}

	if err := PatchStatus[v1alpha1.CellPool](ctx, f, "lts-2-555", "", &v1alpha1.CellPoolStatus{
		WarmCells:       2,
		PlacedCells:     1,
		FreeTenantSlots: 8,
		Tenants:         []string{"alpha", "beta"},
	}); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}
	got, err = Get[v1alpha1.CellPool](ctx, f, "lts-2-555", "")
	if err != nil {
		t.Fatalf("Get (after status): %v", err)
	}
	if got.Status.WarmCells != 2 || got.Status.PlacedCells != 1 || got.Status.FreeTenantSlots != 8 {
		t.Fatalf("status after patch = %+v", got.Status)
	}
	if len(got.Status.Tenants) != 2 {
		t.Fatalf("status.tenants = %v, want two entries", got.Status.Tenants)
	}
}

// TestClearStatusFields_CellPoolClearsTenants asserts a status patch that
// omits tenants force-nulls it. tenants is omitempty, so a merge patch would
// otherwise leave the last tenant list in etcd after the pool empties.
func TestClearStatusFields_CellPoolClearsTenants(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	gvr, err := GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor[CellPool]: %v", err)
	}

	MustSeed(f, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lts-2-555"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-555"},
	})

	if err := PatchStatus[v1alpha1.CellPool](ctx, f, "lts-2-555", "", &v1alpha1.CellPoolStatus{
		PlacedCells: 1,
		Tenants:     []string{"alpha"},
	}); err != nil {
		t.Fatalf("PatchStatus (set): %v", err)
	}

	empty := &v1alpha1.CellPoolStatus{}
	statusJSON, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty status: %v", err)
	}
	statusMap := map[string]any{}
	if err := json.Unmarshal(statusJSON, &statusMap); err != nil {
		t.Fatalf("unmarshal empty status: %v", err)
	}
	ApplyStatusClears(gvr, statusMap)
	if err := f.PatchObjectStatus(ctx, gvr, "", "lts-2-555", statusMap); err != nil {
		t.Fatalf("PatchObjectStatus (clear): %v", err)
	}

	got, err := Get[v1alpha1.CellPool](ctx, f, "lts-2-555", "")
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if len(got.Status.Tenants) != 0 {
		t.Errorf("status.tenants = %v, want empty", got.Status.Tenants)
	}
}

// TestApplyStatusClears_VarroaRoleBindingClearsFederation asserts a status
// patch with no federation entries deletes the stored list.
func TestApplyStatusClears_VarroaRoleBindingClearsFederation(t *testing.T) {
	gvr, err := GVRFor[v1alpha1.VarroaRoleBinding]()
	if err != nil {
		t.Fatalf("GVRFor[VarroaRoleBinding]: %v", err)
	}
	statusMap := map[string]any{"observedGeneration": float64(2)}
	ApplyStatusClears(gvr, statusMap)
	value, sent := statusMap["federation"]
	if !sent || value != nil {
		t.Fatalf("statusMap = %#v, want federation sent as null", statusMap)
	}
}

// TestApplyStatusClears_VarroaRoleClearsFederation asserts a role status
// patch with no federation entries deletes the stored list.
func TestApplyStatusClears_VarroaRoleClearsFederation(t *testing.T) {
	gvr, err := GVRFor[v1alpha1.VarroaRole]()
	if err != nil {
		t.Fatalf("GVRFor[VarroaRole]: %v", err)
	}
	statusMap := map[string]any{"observedGeneration": float64(2)}
	ApplyStatusClears(gvr, statusMap)
	value, sent := statusMap["federation"]
	if !sent || value != nil {
		t.Fatalf("statusMap = %#v, want federation sent as null", statusMap)
	}
}

// TestApplyStatusClears_TeamClearsThePendingPurgeKeys asserts a Team status
// patch that still carries a pipelines object clears the three pendingPurge
// keys inside it.
//
// The move-purge retry writes the zero value of each key into a pipelines
// object that stays present. All three are omitempty, so a plain marshal
// drops them, the merge patch leaves the server's copies in place, and the
// tick repeats a purge that already succeeded on every reconciliation that
// follows.
func TestApplyStatusClears_TeamClearsThePendingPurgeKeys(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	gvr, err := GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor[Team]: %v", err)
	}
	MustSeed(f, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}})

	generation := int64(4)
	if err := PatchStatus[v1alpha1.Team](ctx, f, "payments", "", &v1alpha1.TeamStatus{
		Pipelines: &v1alpha1.TeamPipelinesStatus{
			WebhookToken:              "tok",
			PendingPurgeGeneration:    &generation,
			PendingPurgeCell:          "cell-a",
			PendingPurgeCellNamespace: "cells",
		},
	}); err != nil {
		t.Fatalf("PatchStatus (set): %v", err)
	}

	// The status the retry computes once the purge succeeds: the pipelines
	// object stands, and every pendingPurge field holds its zero value.
	cleared := &v1alpha1.TeamStatus{
		Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok"},
	}
	statusJSON, err := json.Marshal(cleared)
	if err != nil {
		t.Fatalf("marshal cleared status: %v", err)
	}
	statusMap := map[string]any{}
	if err := json.Unmarshal(statusJSON, &statusMap); err != nil {
		t.Fatalf("unmarshal cleared status: %v", err)
	}
	ApplyStatusClears(gvr, statusMap)

	pipelines, ok := statusMap["pipelines"].(map[string]any)
	if !ok {
		t.Fatalf("the patch body dropped the pipelines object: %+v", statusMap)
	}
	for _, key := range []string{"pendingPurgeGeneration", "pendingPurgeCell", "pendingPurgeCellNamespace"} {
		value, sent := pipelines[key]
		if !sent {
			t.Fatalf("the patch body omits %s, so the server keeps its value: %+v", key, pipelines)
		}
		if value != nil {
			t.Fatalf("%s = %v, want null", key, value)
		}
	}

	if err := f.PatchObjectStatus(ctx, gvr, "", "payments", statusMap); err != nil {
		t.Fatalf("PatchObjectStatus (clear): %v", err)
	}
	got, err := Get[v1alpha1.Team](ctx, f, "payments", "")
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if got.Status.Pipelines == nil {
		t.Fatal("status.pipelines went, and it must stand")
	}
	if got.Status.Pipelines.PendingPurgeGeneration != nil {
		t.Errorf("pendingPurgeGeneration = %v, want cleared", *got.Status.Pipelines.PendingPurgeGeneration)
	}
	if got.Status.Pipelines.PendingPurgeCell != "" || got.Status.Pipelines.PendingPurgeCellNamespace != "" {
		t.Errorf("the pending purge cell stands: %+v", got.Status.Pipelines)
	}
	if got.Status.Pipelines.WebhookToken != "tok" {
		t.Errorf("webhookToken = %q, want tok: the clear reaches the three keys alone",
			got.Status.Pipelines.WebhookToken)
	}
}

// TestRegistryTeamAgent proves the TeamAgent kind resolves as a namespaced
// kind and clears its status fields on an apply.
func TestRegistryTeamAgent(t *testing.T) {
	gvr, err := GVRFor[v1alpha1.TeamAgent]()
	if err != nil {
		t.Fatalf("GVRFor[TeamAgent]: %v", err)
	}
	if gvr.Group != "varroa.dev" || gvr.Version != "v1alpha1" || gvr.Resource != "teamagents" {
		t.Errorf("unexpected GVR: %+v", gvr)
	}
	info, err := gvrInfo[v1alpha1.TeamAgent]()
	if err != nil {
		t.Fatalf("gvrInfo[TeamAgent]: %v", err)
	}
	if !info.namespaced {
		t.Error("TeamAgent must be namespaced")
	}
	if info.kind != "TeamAgent" {
		t.Errorf("kind = %q, want TeamAgent", info.kind)
	}
	cleared := clearStatusFields[gvr]
	if _, ok := cleared["lastSession"]; !ok {
		t.Error("lastSession must be a cleared status field")
	}
	if v, ok := cleared["instructionsSHA"]; !ok || v != "" {
		t.Errorf("instructionsSHA clear value = %v, want empty string", v)
	}
}

// TestRegistry_StatusSubresourceMatchesCRD pins each registry entry to its
// CRD in charts/varroa/crds. The Fake reads statusSubresource to decide
// whether .status is kept, so a drift here changes what the tests see.
func TestRegistry_StatusSubresourceMatchesCRD(t *testing.T) {
	paths, err := filepath.Glob("../../charts/varroa/crds/*.yaml")
	if err != nil {
		t.Fatalf("glob CRDs: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no CRD files found under charts/varroa/crds")
	}
	crds := make(map[string]apiextensionsv1.CustomResourceDefinition)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		if crd.Spec.Group != "varroa.dev" {
			continue
		}
		crds[crd.Spec.Names.Plural] = crd
	}

	for _, info := range registry {
		crd, ok := crds[info.gvr.Resource]
		if !ok {
			t.Errorf("%s: no CRD for resource %q", info.kind, info.gvr.Resource)
			continue
		}
		if crd.Spec.Names.Kind != info.kind {
			t.Errorf("%s: CRD kind = %q", info.kind, crd.Spec.Names.Kind)
		}
		if namespaced := crd.Spec.Scope == apiextensionsv1.NamespaceScoped; namespaced != info.namespaced {
			t.Errorf("%s: CRD namespaced = %v, registry says %v", info.kind, namespaced, info.namespaced)
		}
		var version *apiextensionsv1.CustomResourceDefinitionVersion
		for i := range crd.Spec.Versions {
			if crd.Spec.Versions[i].Name == info.gvr.Version {
				version = &crd.Spec.Versions[i]
			}
		}
		if version == nil {
			t.Errorf("%s: CRD has no version %q", info.kind, info.gvr.Version)
			continue
		}
		hasStatus := version.Subresources != nil && version.Subresources.Status != nil
		if hasStatus != info.statusSubresource {
			t.Errorf("%s: CRD status subresource = %v, registry statusSubresource = %v",
				info.kind, hasStatus, info.statusSubresource)
		}
	}
}
