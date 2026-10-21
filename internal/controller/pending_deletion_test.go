package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func deleteItemResult(id, path string, success bool, errText string) *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId:   id,
		Success:     success,
		Error:       errText,
		CommandType: mitev1.CommandTypeDeleteItem,
		JobPath:     path,
	}
}

func deletionPendingStatus(t *testing.T, st *v1alpha1.ControllerStatus) metav1.ConditionStatus {
	t.Helper()
	for _, c := range st.Conditions {
		if c.Type == v1alpha1.ConditionDeletionPending {
			return c.Status
		}
	}
	t.Fatal("DeletionPending condition missing")
	return ""
}

func TestApplyDeleteItemResult_SuccessRemovesTheEntry(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{
			{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c1"}},
		},
	}
	if !applyDeleteItemResult(st, deleteItemResult("c1", "team/job", true, ""), metav1Now()) {
		t.Fatal("success result changed nothing")
	}
	if len(st.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %v, want none", st.PendingItemDeletions)
	}
}

func TestApplyDeleteItemResult_SuccessSetsDeletionPendingFalse(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{{Path: "team/job"}},
		Conditions:           []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionDeletionPending, Status: metav1.ConditionTrue}},
	}
	applyDeleteItemResult(st, deleteItemResult("c1", "team/job", true, ""), metav1Now())
	if got := deletionPendingStatus(t, st); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False once the list is empty", got)
	}
}

func TestApplyDeleteItemResult_FailureKeepsTheEntryAndRecordsTheError(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{
			{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c1", ApprovedAt: metav1Now()}},
		},
	}
	if !applyDeleteItemResult(st, deleteItemResult("c1", "team/job", false, "boom"), metav1Now()) {
		t.Fatal("failure result changed nothing")
	}
	if len(st.PendingItemDeletions) != 1 {
		t.Fatalf("pending deletions = %v, want the entry kept", st.PendingItemDeletions)
	}
	approval := st.PendingItemDeletions[0].Approval
	if approval == nil || approval.Error != "boom" || approval.FailedAt == nil {
		t.Fatalf("approval = %+v, want the error and the failure time", approval)
	}
	if approval.CommandID != "c1" {
		t.Fatalf("approval command id = %q, want c1", approval.CommandID)
	}
}

func TestApplyDeleteItemResult_FailureFromAnOlderApprovalIsIgnored(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{
			{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c2"}},
		},
	}
	if applyDeleteItemResult(st, deleteItemResult("c1", "team/job", false, "old"), metav1Now()) {
		t.Fatal("a result for an older approval changed the entry")
	}
	if got := st.PendingItemDeletions[0].Approval; got.Error != "" || got.CommandID != "c2" {
		t.Fatalf("approval = %+v, want the newer approval untouched", got)
	}
}

func TestApplyDeleteItemResult_FailureWithoutAnApprovalRecordsIt(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{{Path: "team/job"}},
	}
	if !applyDeleteItemResult(st, deleteItemResult("c9", "team/job", false, "shard moved"), metav1Now()) {
		t.Fatal("failure without an approval changed nothing")
	}
	approval := st.PendingItemDeletions[0].Approval
	if approval == nil || approval.CommandID != "c9" || approval.Error != "shard moved" {
		t.Fatalf("approval = %+v, want the result recorded", approval)
	}
	if approval.ApprovedAt.IsZero() {
		t.Fatal("approval time is zero; the API would serialize a fabricated timestamp")
	}
}

func TestApplyDeleteItemResult_UnknownPathAndOtherTypesChangeNothing(t *testing.T) {
	st := &v1alpha1.ControllerStatus{
		PendingItemDeletions: []v1alpha1.PendingDeletion{{Path: "team/job"}},
	}
	if applyDeleteItemResult(st, deleteItemResult("c1", "team/other", true, ""), metav1Now()) {
		t.Fatal("unknown path changed the list")
	}
	restart := deleteItemResult("c1", "team/job", true, "")
	restart.CommandType = mitev1.CommandTypeSafeRestart
	if applyDeleteItemResult(st, restart, metav1Now()) {
		t.Fatal("a non-delete result changed the list")
	}
	noPath := deleteItemResult("c1", "", true, "")
	if applyDeleteItemResult(st, noPath, metav1Now()) {
		t.Fatal("a result without a job path changed the list")
	}
	if len(st.PendingItemDeletions) != 1 || st.PendingItemDeletions[0].Approval != nil {
		t.Fatalf("pending deletions = %+v, want unchanged", st.PendingItemDeletions)
	}
}

func TestRebuildPendingDeletions_KeepsApprovalAndDetectedAt(t *testing.T) {
	detected := metav1.Unix(1000, 0)
	old := []v1alpha1.PendingDeletion{
		{Path: "team/job", Reason: "build in progress", DetectedAt: detected,
			Approval: &v1alpha1.DeletionApproval{CommandID: "c1"}},
	}
	now := metav1.Unix(2000, 0)
	reported := []*mitev1.DeferredDeletion{{Path: "team/job", Reason: "build in progress"}}

	got := rebuildPendingDeletions(old, reported, now)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if !got[0].DetectedAt.Equal(&detected) {
		t.Fatalf("DetectedAt = %v, want the original %v", got[0].DetectedAt, detected)
	}
	if got[0].Approval == nil || got[0].Approval.CommandID != "c1" {
		t.Fatalf("approval = %+v, want the kept approval", got[0].Approval)
	}
}

func TestRebuildPendingDeletions_DropsAPathTheResultDoesNotList(t *testing.T) {
	old := []v1alpha1.PendingDeletion{{Path: "team/gone"}}
	now := metav1.Unix(2000, 0)
	reported := []*mitev1.DeferredDeletion{{Path: "team/new", Reason: "build in progress"}}

	got := rebuildPendingDeletions(old, reported, now)
	if len(got) != 1 || got[0].Path != "team/new" {
		t.Fatalf("entries = %+v, want only team/new", got)
	}
	if !got[0].DetectedAt.Equal(&now) {
		t.Fatalf("DetectedAt = %v, want the rebuild time for a new entry", got[0].DetectedAt)
	}
	if got[0].Approval != nil {
		t.Fatalf("new entry carries an approval: %+v", got[0].Approval)
	}
}

func TestReconcile_DeleteItemResultClearsThePendingEntry(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{
		{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c1"}},
	}
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionDeletionPending, Status: metav1.ConditionTrue}}
	transport.drainResults = []*mitev1.CommandResult{deleteItemResult("c1", "team/job", true, "")}

	_ = rec.reconcileController(context.Background(), cr)

	if len(cr.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %+v, want none after the mite confirmed", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False", got)
	}
}

func TestReconcile_FailedDeleteItemResultKeepsTheEntry(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{
		{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c1"}},
	}
	transport.drainResults = []*mitev1.CommandResult{deleteItemResult("c1", "team/job", false, "item old-job deleted, but the record was not updated")}

	_ = rec.reconcileController(context.Background(), cr)

	if len(cr.Status.PendingItemDeletions) != 1 {
		t.Fatalf("pending deletions = %+v, want the entry kept", cr.Status.PendingItemDeletions)
	}
	approval := cr.Status.PendingItemDeletions[0].Approval
	if approval == nil || approval.Error == "" {
		t.Fatalf("approval = %+v, want the mite error recorded", approval)
	}
}
