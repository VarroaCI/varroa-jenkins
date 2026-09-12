package crdstore

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	for key, zero := range ClearStatusFields(gvr) {
		if _, ok := statusMap[key]; !ok {
			statusMap[key] = zero
		}
	}
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
	for key, zero := range ClearStatusFields(gvr) {
		statusMap[key] = zero
	}
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
