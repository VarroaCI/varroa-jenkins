package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// deliveredBeforeTheMove is the full hash of a delivered desired state
// whose tenant homes differ from the current ones.
const deliveredBeforeTheMove = "delivered-before-the-home-move"

// manualAfterDeliveredPush answers a manual-mode controller whose last
// delivered push holds the current content but other tenant homes. The
// first tick runs in automatic mode, so it delivers and stamps both hashes.
func manualAfterDeliveredPush(t *testing.T, name string) (*Reconciler, *scriptedSendTransport, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	tr := &scriptedSendTransport{Transport: rec.miteTransport, epoch: 1}
	rec.miteTransport = tr

	cr := newGateCtrl(name, 0, "", nil)
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 || cr.Status.DesiredStateHash == "" || cr.Status.DesiredContentHash == "" {
		t.Fatalf("first push: sends=%d hash=%q content=%q, want one delivered push with both hashes",
			tr.sends, cr.Status.DesiredStateHash, cr.Status.DesiredContentHash)
	}
	if cr.Status.DesiredContentHash != computeDesiredContentHash(tr.last) {
		t.Fatalf("desiredContentHash = %q, want the content hash of the pushed state", cr.Status.DesiredContentHash)
	}
	cr.Spec.ReconciliationPolicy.Mode = v1alpha1.ReconciliationModeManual
	cr.Status.DesiredStateHash = deliveredBeforeTheMove
	return rec, tr, cr
}

// TestReconcileConnected_ManualModePushesAPlacementOnlyChange pins that a
// manual-mode tick pushes a change of placement data at once. The content
// is the delivered and applied content, so no approval is due, and a stale
// pending restart is cleared.
func TestReconcileConnected_ManualModePushesAPlacementOnlyChange(t *testing.T) {
	rec, tr, cr := manualAfterDeliveredPush(t, "ctl-placement-push")
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: deliveredBeforeTheMove, Succeeded: true}
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{
		DesiredStateHash: "stale",
		ContentHash:      cr.Status.DesiredContentHash,
		Changes:          []string{"unknown"},
	}

	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want 2: the placement-only change must go out", tr.sends)
	}
	if cr.Status.PendingRestart != nil {
		t.Fatalf("pending restart = %+v, want none", cr.Status.PendingRestart)
	}
	if tr.last.ApplyWhen != "" {
		t.Fatalf("applyWhen = %q, want an immediate apply", tr.last.ApplyWhen)
	}
	if cr.Status.DesiredStateHash != tr.last.DesiredStateHash {
		t.Fatalf("desiredStateHash = %q, want the pushed hash %q", cr.Status.DesiredStateHash, tr.last.DesiredStateHash)
	}
}

// TestReconcileConnected_ManualModeHoldsAPlacementOnlyChangeWhileTheDeliveredPushIsPending
// pins that a placement-only change does not replace a delivered push with
// no apply result yet. That push can be an approval that waits for idle
// builds, and a newer push would apply its content at once.
func TestReconcileConnected_ManualModeHoldsAPlacementOnlyChangeWhileTheDeliveredPushIsPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *v1alpha1.ApplyResult
	}{
		{"no result", nil},
		{"result for an older push", &v1alpha1.ApplyResult{Hash: "older", Succeeded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr, cr := manualAfterDeliveredPush(t, "ctl-placement-hold")
			cr.Status.LastApplyResult = tc.result

			_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
			if tr.sends != 1 {
				t.Fatalf("sends = %d, want 1: the tick must hold", tr.sends)
			}
			if cr.Status.PendingRestart != nil {
				t.Fatalf("pending restart = %+v, want none", cr.Status.PendingRestart)
			}
			if cr.Status.DesiredStateHash != deliveredBeforeTheMove {
				t.Fatalf("desiredStateHash = %q, want no change", cr.Status.DesiredStateHash)
			}
		})
	}
}

// TestReconcileConnected_ManualModeRaisesAPlacementOnlyChangeAfterAFailedApply
// pins that a failed apply of the delivered content needs a new approval,
// also when only the placement data changed after it.
func TestReconcileConnected_ManualModeRaisesAPlacementOnlyChangeAfterAFailedApply(t *testing.T) {
	rec, tr, cr := manualAfterDeliveredPush(t, "ctl-placement-failed")
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: deliveredBeforeTheMove, Succeeded: false}

	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 {
		t.Fatalf("sends = %d, want 1", tr.sends)
	}
	if cr.Status.PendingRestart == nil {
		t.Fatal("no pending restart after a failed apply")
	}
}

// TestReconcileConnected_ManualModeRaisesAContentChange pins that a change
// of the content still needs an approval, and that a controller with no
// delivered content hash keeps that rule.
func TestReconcileConnected_ManualModeRaisesAContentChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"other delivered content", "older-content"},
		{"no delivered content hash", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr, cr := manualAfterDeliveredPush(t, "ctl-content-change")
			cr.Status.DesiredContentHash = tc.content
			cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: deliveredBeforeTheMove, Succeeded: true}

			_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
			if tr.sends != 1 {
				t.Fatalf("sends = %d, want 1: a content change waits for approval", tr.sends)
			}
			p := cr.Status.PendingRestart
			if p == nil || p.ContentHash == "" {
				t.Fatalf("pending restart = %+v, want one with a content hash", p)
			}
		})
	}
}

// TestReconcileConnected_ManualCellPushesAJobHomeMove pins the cell case
// end to end: a job home move changes only the tenant homes, and the tick
// pushes the new homes with no approval.
func TestReconcileConnected_ManualCellPushesAJobHomeMove(t *testing.T) {
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	homes := map[string]int32{"build": 0}
	rec.homesFunc = func(context.Context, cells.HomesRequest) (cells.HomesResult, error) {
		return cells.HomesResult{JobHomes: homes}, nil
	}
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}},
	})
	tr := &scriptedSendTransport{Transport: rec.miteTransport, epoch: 1}
	rec.miteTransport = tr

	cr := newGateCtrl("cell-home-move", 0, "", nil)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}}
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 || len(tr.last.TenantHomes) != 1 {
		t.Fatalf("first push: sends=%d, want one push with one tenant home entry", tr.sends)
	}
	cr.Spec.ReconciliationPolicy.Mode = v1alpha1.ReconciliationModeManual
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: cr.Status.DesiredStateHash, Succeeded: true}
	before := cr.Status.DesiredStateHash

	homes = map[string]int32{"build": 1}
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want 2: the home move must go out", tr.sends)
	}
	if got := tr.last.TenantHomes[0].JobHomes["build"]; got != 1 {
		t.Fatalf("job home = %d, want the new slot 1", got)
	}
	if cr.Status.PendingRestart != nil {
		t.Fatalf("pending restart = %+v, want none", cr.Status.PendingRestart)
	}
	if cr.Status.DesiredStateHash == before {
		t.Fatal("desiredStateHash did not move to the pushed state")
	}
}

// TestControllerStatusPatch_CarriesDesiredContentHash pins that the patch
// writes the delivered content hash and, like desiredStateHash, omits an
// empty one.
func TestControllerStatusPatch_CarriesDesiredContentHash(t *testing.T) {
	patch := controllerStatusPatch(&v1alpha1.ControllerStatus{DesiredStateHash: "full", DesiredContentHash: "content"})
	if patch["desiredContentHash"] != "content" {
		t.Fatalf("desiredContentHash = %v, want content", patch["desiredContentHash"])
	}
	patch = controllerStatusPatch(&v1alpha1.ControllerStatus{})
	if _, ok := patch["desiredContentHash"]; ok {
		t.Fatal("an empty desiredContentHash is in the patch")
	}
}

// manualCellWithPendingRehydration answers a manual-mode cell whose last
// delivered push holds the current desired state, with one tenant whose
// rehydration is not applied. The first tick runs in automatic mode, so it
// delivers and stamps both hashes. records answers the record store.
func manualCellWithPendingRehydration(t *testing.T, name string, records cells.RecordsFunc) (*Reconciler, *scriptedSendTransport, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	rec.homesFunc = func(context.Context, cells.HomesRequest) (cells.HomesResult, error) {
		return cells.HomesResult{JobHomes: map[string]int32{"build": 0}}, nil
	}
	rec.SetRecordsFunc(records)
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: name, CellNamespace: "default", Generation: 1},
		},
	})
	tr := &scriptedSendTransport{Transport: rec.miteTransport, epoch: 1}
	rec.miteTransport = tr

	cr := newGateCtrl(name, 0, "", nil)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}, Instance: "uid-1"}
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 || cr.Status.DesiredStateHash == "" || cr.Status.DesiredContentHash == "" {
		t.Fatalf("first push: sends=%d hash=%q content=%q, want one delivered push with both hashes",
			tr.sends, cr.Status.DesiredStateHash, cr.Status.DesiredContentHash)
	}
	cr.Spec.ReconciliationPolicy.Mode = v1alpha1.ReconciliationModeManual
	return rec, tr, cr
}

// oneJobRecords answers a record store with one job of team-a.
func oneJobRecords(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
	return cells.RecordsResult{Jobs: []cells.JobRecords{{JobPath: "team-a/build", NextBuildNumber: 5}}}, nil
}

// TestReconcileConnected_ManualCellPushesAPendingRehydrationBundle pins
// that a manual-mode cell pushes a pending rehydration bundle with no
// approval. The bundle stays outside the full hash, so the hash does not
// change, and the delivered push applied. A stale pending restart is
// cleared.
func TestReconcileConnected_ManualCellPushesAPendingRehydrationBundle(t *testing.T) {
	rec, tr, cr := manualCellWithPendingRehydration(t, "cell-rehydration-push", oneJobRecords)
	delivered := cr.Status.DesiredStateHash
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: delivered, Succeeded: true}
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{
		DesiredStateHash: "stale",
		ContentHash:      "other-content",
		Changes:          []string{"config"},
	}

	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want 2: the pending rehydration bundle must go out", tr.sends)
	}
	if tr.last.Rehydration == nil {
		t.Fatal("the push carries no rehydration bundle")
	}
	if tr.last.ApplyWhen != "" {
		t.Fatalf("applyWhen = %q, want an immediate apply", tr.last.ApplyWhen)
	}
	if cr.Status.PendingRestart != nil {
		t.Fatalf("pending restart = %+v, want none", cr.Status.PendingRestart)
	}
	if cr.Status.DesiredStateHash != delivered {
		t.Fatalf("desiredStateHash = %q, want the unchanged hash %q", cr.Status.DesiredStateHash, delivered)
	}
	entries := cr.Status.Cell.Rehydration
	if len(entries) != 1 || entries[0].CommandID != tr.last.CommandId {
		t.Fatalf("rehydration entries = %+v, want one entry for the sent command %q", entries, tr.last.CommandId)
	}
}

// TestReconcileConnected_ManualCellHoldsRehydrationWhileTheDeliveredPushIsPending
// pins that a rehydration bundle does not replace a delivered push with no
// apply result yet. That push can be an approval that waits for idle
// builds, and a newer push would apply its content at once.
func TestReconcileConnected_ManualCellHoldsRehydrationWhileTheDeliveredPushIsPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *v1alpha1.ApplyResult
	}{
		{"no result", nil},
		{"result for an older push", &v1alpha1.ApplyResult{Hash: "older", Succeeded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr, cr := manualCellWithPendingRehydration(t, "cell-rehydration-hold", oneJobRecords)
			cr.Status.LastApplyResult = tc.result

			_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
			if tr.sends != 1 {
				t.Fatalf("sends = %d, want 1: the tick must hold", tr.sends)
			}
			if cr.Status.PendingRestart != nil {
				t.Fatalf("pending restart = %+v, want none", cr.Status.PendingRestart)
			}
		})
	}
}

// TestReconcileConnected_ManualCellHoldsRehydrationAfterAFailedApply pins
// that a failed apply of the delivered content stops the rehydration
// push. The user approves the content again first.
func TestReconcileConnected_ManualCellHoldsRehydrationAfterAFailedApply(t *testing.T) {
	rec, tr, cr := manualCellWithPendingRehydration(t, "cell-rehydration-failed", oneJobRecords)
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: cr.Status.DesiredStateHash, Succeeded: false}

	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 {
		t.Fatalf("sends = %d, want 1: the tick must hold", tr.sends)
	}
}

// TestReconcileConnected_ManualCellSendsNothingWithoutABundle pins that a
// tenant that is pending with no bundle sends no push. Such a push would
// only send the delivered state again.
func TestReconcileConnected_ManualCellSendsNothingWithoutABundle(t *testing.T) {
	failing := func(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
		return cells.RecordsResult{}, errors.New("record store unavailable")
	}
	rec, tr, cr := manualCellWithPendingRehydration(t, "cell-rehydration-nobundle", failing)
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{Hash: cr.Status.DesiredStateHash, Succeeded: true}

	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
	if tr.sends != 1 {
		t.Fatalf("sends = %d, want 1: a pending tenant with no bundle sends nothing", tr.sends)
	}
}
