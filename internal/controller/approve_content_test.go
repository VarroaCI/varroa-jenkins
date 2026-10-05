package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// pendingRestartFor answers the pending restart that the tick raises for
// the current inputs of cr, so an approve test starts from a record that
// matches the state the approve builds.
func pendingRestartFor(t *testing.T, rec *Reconciler, cr *v1alpha1.Controller) *v1alpha1.PendingRestart {
	t.Helper()
	desired, err := rec.assembleApproveState(context.Background(), cr)
	if err != nil {
		t.Fatalf("assemble the approve state: %v", err)
	}
	return &v1alpha1.PendingRestart{
		DetectedAt:       metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
		DesiredStateHash: computeDesiredStateHash(desired),
		ContentHash:      computeDesiredContentHash(desired),
		Changes:          []string{"config"},
	}
}

func getController(t *testing.T, client *testClient, name string) *v1alpha1.Controller {
	t.Helper()
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, name, "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	return got
}

// TestApproveRestart_RefusesWhenTheContentChangedAfterTheRaise pins that an
// approval sends nothing when the bundle changed after the pending restart
// was raised. It records the new content as the pending restart instead.
func TestApproveRestart_RefusesWhenTheContentChangedAfterTheRaise(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	old := pendingRestartFor(t, rec, cr)
	cr.Status.PendingRestart = old
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	client.configMapData["test-bundle-content"]["jenkins.yaml"] = "jenkins:\n  systemMessage: \"Changed\""
	fresh := pendingRestartFor(t, rec, cr)

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", "")
	var changed *PendingRestartChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want a PendingRestartChangedError", err)
	}
	if len(changed.Changes) == 0 || changed.Changes[0] != "config" {
		t.Fatalf("changes = %v, want config first", changed.Changes)
	}
	if tr.lastDesired != nil {
		t.Fatal("a refused approval sent a desired state")
	}
	got := getController(t, client, "mine")
	p := got.Status.PendingRestart
	if p == nil || p.ContentHash != fresh.ContentHash || p.DesiredStateHash != fresh.DesiredStateHash {
		t.Fatalf("pending restart = %+v, want the new content %s", p, fresh.ContentHash)
	}
	if p.ContentHash == old.ContentHash {
		t.Fatal("the pending restart still holds the old content")
	}
	if !p.DetectedAt.After(old.DetectedAt.Time) {
		t.Fatalf("detectedAt = %v, want a time after the old raise %v", p.DetectedAt, old.DetectedAt)
	}
	if got.Status.DesiredStateHash != "" {
		t.Fatalf("desiredStateHash = %q, want no change", got.Status.DesiredStateHash)
	}
}

// TestApproveRestart_ProceedsWhenOnlyTenantHomesChanged pins that tenant
// homes stay outside the content check: a home move after the raise does
// not refuse the approval, and the approval sends the current homes.
func TestApproveRestart_ProceedsWhenOnlyTenantHomesChanged(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	homes := map[string]int32{"build": 0}
	rec.homesFunc = func(context.Context, cells.HomesRequest) (cells.HomesResult, error) {
		return cells.HomesResult{JobHomes: homes}, nil
	}
	crdstore.MustSeed(client.store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}},
	})
	cr := testController("cell", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}}
	old := pendingRestartFor(t, rec, cr)
	cr.Status.PendingRestart = old
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	homes = map[string]int32{"build": 1}
	fresh := pendingRestartFor(t, rec, cr)
	if fresh.DesiredStateHash == old.DesiredStateHash || fresh.ContentHash != old.ContentHash {
		t.Fatalf("test setup: the home move must change only the full hash")
	}

	if err := rec.ApproveRestart(context.Background(), "", "ns", "cell", "approve", old.ContentHash); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if tr.lastDesired == nil {
		t.Fatal("the approval sent no desired state")
	}
	if len(tr.lastDesired.TenantHomes) != 1 || tr.lastDesired.TenantHomes[0].JobHomes["build"] != 1 {
		t.Fatalf("tenant homes = %v, want the current home slot 1", tr.lastDesired.TenantHomes)
	}
	got := getController(t, client, "cell")
	if got.Status.PendingRestart != nil {
		t.Fatalf("pending restart = %+v, want none", got.Status.PendingRestart)
	}
	if got.Status.DesiredStateHash != fresh.DesiredStateHash {
		t.Fatalf("desiredStateHash = %q, want the hash with the current homes %q", got.Status.DesiredStateHash, fresh.DesiredStateHash)
	}
}

// TestApproveRestart_MatchingContentClearsThePendingRestart pins that every
// action that sends a desired state proceeds when the content matches.
func TestApproveRestart_MatchingContentClearsThePendingRestart(t *testing.T) {
	for _, action := range []string{"approve", "restart", "force", "reload"} {
		t.Run(action, func(t *testing.T) {
			client := newTestClientWithBundle()
			rec := newTestReconciler(client)
			cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
			pending := pendingRestartFor(t, rec, cr)
			cr.Status.PendingRestart = pending
			crdstore.MustSeed(client.store, cr)
			tr := &captureTransport{}
			rec.miteTransport = tr

			if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", action, ""); err != nil {
				t.Fatalf("approve %s: %v", action, err)
			}
			if tr.lastDesired == nil {
				t.Fatal("the approval sent no desired state")
			}
			got := getController(t, client, "mine")
			if got.Status.PendingRestart != nil {
				t.Fatalf("pending restart = %+v, want none", got.Status.PendingRestart)
			}
			if got.Status.DesiredStateHash != pending.DesiredStateHash {
				t.Fatalf("desiredStateHash = %q, want %q", got.Status.DesiredStateHash, pending.DesiredStateHash)
			}
		})
	}
}

// TestApproveRestart_RefusesAPendingRestartWithoutAContentHash pins that a
// pending restart with no content hash fails closed: the approval refuses
// and records the content hash.
func TestApproveRestart_RefusesAPendingRestartWithoutAContentHash(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	want := pendingRestartFor(t, rec, cr)
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{DesiredStateHash: want.DesiredStateHash}
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", "")
	var changed *PendingRestartChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want a PendingRestartChangedError", err)
	}
	if tr.lastDesired != nil {
		t.Fatal("a refused approval sent a desired state")
	}
	if p := getController(t, client, "mine").Status.PendingRestart; p == nil || p.ContentHash != want.ContentHash {
		t.Fatalf("pending restart = %+v, want content hash %s", p, want.ContentHash)
	}
}

// TestApproveRestart_RecordsTheContentHashOfAReflaggedPendingRestart pins
// that a pending restart for the applied hash with no content hash gets the
// content hash on a refusal, so the next approval proceeds.
func TestApproveRestart_RecordsTheContentHashOfAReflaggedPendingRestart(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	want := pendingRestartFor(t, rec, cr)
	cr.Status.DesiredStateHash = want.DesiredStateHash
	cr.Status.PendingRestart = &v1alpha1.PendingRestart{DesiredStateHash: want.DesiredStateHash}
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", "")
	var changed *PendingRestartChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want a PendingRestartChangedError", err)
	}
	if p := getController(t, client, "mine").Status.PendingRestart; p == nil || p.ContentHash != want.ContentHash {
		t.Fatalf("pending restart = %+v, want content hash %s", p, want.ContentHash)
	}
	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", ""); err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if tr.lastDesired == nil {
		t.Fatal("the second approval sent no desired state")
	}
}

// TestApproveRestart_RefusesAnExpectedHashThatDiffers pins the caller's
// precondition: an approval for content that the pending restart no longer
// holds refuses, sends nothing, and leaves the pending restart unchanged.
func TestApproveRestart_RefusesAnExpectedHashThatDiffers(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	pending := pendingRestartFor(t, rec, cr)
	pending.Changes = []string{"rbac"}
	cr.Status.PendingRestart = pending
	crdstore.MustSeed(client.store, cr)
	tr := &captureTransport{}
	rec.miteTransport = tr

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "force", "seen-before")
	var changed *PendingRestartChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want a PendingRestartChangedError", err)
	}
	if len(changed.Changes) != 1 || changed.Changes[0] != "rbac" {
		t.Fatalf("changes = %v, want the pending sections [rbac]", changed.Changes)
	}
	if tr.lastDesired != nil {
		t.Fatal("a refused approval sent a desired state")
	}
	p := getController(t, client, "mine").Status.PendingRestart
	if p == nil || p.ContentHash != pending.ContentHash || !p.DetectedAt.Equal(&pending.DetectedAt) {
		t.Fatalf("pending restart = %+v, want it unchanged", p)
	}
}

// TestApproveRestart_NoPendingRestartIsTyped pins that the refusal for a
// controller with no pending restart wraps ErrNoPendingRestart and keeps
// its message.
func TestApproveRestart_NoPendingRestartIsTyped(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	crdstore.MustSeed(client.store, testController("mine", "ns", v1alpha1.ControllerPhaseConnected))
	rec.miteTransport = &captureTransport{}

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "approve", "")
	if !errors.Is(err, ErrNoPendingRestart) || err.Error() != "no pending restart for ns/mine" {
		t.Fatalf("err = %v, want ErrNoPendingRestart with the message", err)
	}
}

// TestApproveRestart_PluginRollWithNoPendingRollSendsNothing pins that a
// plugin-roll approval with no pending roll is refused with a typed error,
// and that it does not release a pending restart to the mite.
func TestApproveRestart_PluginRollWithNoPendingRollSendsNothing(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingRestart = pendingRestartFor(t, rec, cr)
	crdstore.MustSeed(client.store, cr)
	transport := &captureTransport{}
	rec.miteTransport = transport

	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", "")
	if !errors.Is(err, ErrNoPendingPluginRoll) {
		t.Fatalf("err = %v, want ErrNoPendingPluginRoll", err)
	}
	if transport.lastDesired != nil {
		t.Fatal("the refused approval sent a desired state to the mite")
	}
	got := getController(t, client, "mine")
	if got.Status.PendingRestart == nil {
		t.Fatal("the refused approval cleared the pending restart")
	}
	if got.Status.ApprovedPluginRollChecksum != "" {
		t.Fatalf("approved = %q, want none", got.Status.ApprovedPluginRollChecksum)
	}
}

// TestRaisePendingRestart_KeepsTheRecordForTheSameContent pins that a raise
// for the same content keeps DetectedAt and Changes and updates the full
// hash, and that a raise for new content replaces the record.
func TestRaisePendingRestart_KeepsTheRecordForTheSameContent(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-time.Hour))
	later := metav1.NewTime(time.Now())
	var status v1alpha1.ControllerStatus

	raisePendingRestart(&status, first, "full-1", "content-1", []string{"config"})
	raisePendingRestart(&status, later, "full-2", "content-1", []string{"unknown"})
	p := status.PendingRestart
	if !p.DetectedAt.Equal(&first) || p.DesiredStateHash != "full-2" || len(p.Changes) != 1 || p.Changes[0] != "config" {
		t.Fatalf("pending restart = %+v, want the first record with full hash full-2", p)
	}

	raisePendingRestart(&status, later, "full-3", "content-2", []string{"rbac"})
	p = status.PendingRestart
	if !p.DetectedAt.Equal(&later) || p.ContentHash != "content-2" || p.Changes[0] != "rbac" {
		t.Fatalf("pending restart = %+v, want a new record for content-2", p)
	}
}

// TestComputeDesiredContentHash_IgnoresTenantHomes pins that the content
// hash changes with the JCasC and not with the tenant homes.
func TestComputeDesiredContentHash_IgnoresTenantHomes(t *testing.T) {
	a := baseHomesCommand([]*mitev1.TenantHomes{{Tenant: "t", JobHomes: map[string]int{"j": 0}}})
	b := baseHomesCommand([]*mitev1.TenantHomes{{Tenant: "t", JobHomes: map[string]int{"j": 1}}})
	if computeDesiredContentHash(a) != computeDesiredContentHash(b) {
		t.Fatal("a tenant home move changed the content hash")
	}
	b.JcascYaml += "\n# changed"
	if computeDesiredContentHash(a) == computeDesiredContentHash(b) {
		t.Fatal("a JCasC change did not change the content hash")
	}
}

// TestComputeDesiredContentHash_FramesEachSection pins that the same text
// in another section answers another hash.
func TestComputeDesiredContentHash_FramesEachSection(t *testing.T) {
	inJcasc := &mitev1.DesiredStateCommand{JcascYaml: "x: 1"}
	inRbac := &mitev1.DesiredStateCommand{RbacYaml: "x: 1"}
	if computeDesiredContentHash(inJcasc) == computeDesiredContentHash(inRbac) {
		t.Fatal("the same text in the JCasC and in the RBAC answers the same hash")
	}
}

// TestReconcileConnected_ManualModeKeepsDetectedAtForTheSameContent pins
// that the manual-mode tick records the content hash and keeps DetectedAt
// while the content stays the same.
func TestReconcileConnected_ManualModeKeepsDetectedAtForTheSameContent(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Provisioning reconcile: %v", err)
	}
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
	cr.Status.DesiredStateHash = ""

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("first Connected reconcile: %v", err)
	}
	p := cr.Status.PendingRestart
	if p == nil || p.ContentHash == "" {
		t.Fatalf("pending restart = %+v, want one with a content hash", p)
	}
	old := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	p.DetectedAt = old
	content := p.ContentHash

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("second Connected reconcile: %v", err)
	}
	if p := cr.Status.PendingRestart; p == nil || !p.DetectedAt.Equal(&old) || p.ContentHash != content {
		t.Fatalf("pending restart = %+v, want the same record detected at %v", p, old)
	}

	client.configMapData["test-bundle-content"]["jenkins.yaml"] = "jenkins:\n  systemMessage: \"Changed\""
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("third Connected reconcile: %v", err)
	}
	if p := cr.Status.PendingRestart; p == nil || p.DetectedAt.Equal(&old) || p.ContentHash == content {
		t.Fatalf("pending restart = %+v, want a new record for the new content", p)
	}
}
