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

// deferredResult builds a desired-state result for the deferred-deletion
// tests. Each deferred path is reported with the same reason text.
func deferredResult(itemsOK bool, failures []string, deferred ...string) *mitev1.DesiredStateResult {
	d := &mitev1.DesiredStateResult{ItemsSuccess: itemsOK}
	for _, path := range failures {
		d.ItemFailures = append(d.ItemFailures, &mitev1.ItemFailure{Path: path})
	}
	for _, path := range deferred {
		d.DeferredItemDeletions = append(d.DeferredItemDeletions, &mitev1.DeferredDeletion{Path: path, Reason: "in use"})
	}
	return d
}

// pendingDeletionController returns a controller with one approved pending
// deletion and DeletionPending set to True.
func pendingDeletionController(t0 metav1.Time) *v1alpha1.Controller {
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{
		{Path: "team/job", DetectedAt: t0, Approval: &v1alpha1.DeletionApproval{CommandID: "c1", Error: "boom"}},
	}
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionDeletionPending, Status: metav1.ConditionTrue}}
	return cr
}

func TestApplyDeferredDeletionsResult_AbortedWalkKeepsTheList(t *testing.T) {
	t0 := metav1Now()
	cr := pendingDeletionController(t0)
	d := deferredResult(false, nil)
	d.ItemsError = "parse items: bad"

	applyDeferredDeletionsResult(cr, d, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 1 {
		t.Fatalf("pending deletions = %+v, want the entry kept", cr.Status.PendingItemDeletions)
	}
	entry := cr.Status.PendingItemDeletions[0]
	if entry.Approval == nil || entry.Approval.Error != "boom" || !entry.DetectedAt.Equal(&t0) {
		t.Fatalf("entry = %+v, want the approval and DetectedAt kept", entry)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionTrue {
		t.Fatalf("DeletionPending = %s, want True", got)
	}
}

func TestApplyDeferredDeletionsResult_ResultWithNoItemsStepKeepsTheList(t *testing.T) {
	cr := pendingDeletionController(metav1Now())

	applyDeferredDeletionsResult(cr, &mitev1.DesiredStateResult{}, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 1 {
		t.Fatalf("pending deletions = %+v, want the entry kept", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionTrue {
		t.Fatalf("DeletionPending = %s, want True", got)
	}
}

func TestApplyDeferredDeletionsResult_CompletedWalkWithNoDeferralsClears(t *testing.T) {
	cr := pendingDeletionController(metav1Now())

	applyDeferredDeletionsResult(cr, deferredResult(true, nil), metav1Now())

	if len(cr.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %+v, want none", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False", got)
	}
}

func TestApplyDeferredDeletionsResult_RefusedWalkWithNoDeferralsClears(t *testing.T) {
	cr := pendingDeletionController(metav1Now())

	d := deferredResult(false, []string{"x"})
	d.ItemsRemovalReached = true

	applyDeferredDeletionsResult(cr, d, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %+v, want none", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False", got)
	}
}

// TestApplyDeferredDeletionsResult_RefusedWalkOnUnreadableCacheKeepsTheList
// refuses an item on an unreadable managed-items cache. The remove step did
// not run, so the pending entry keeps its approval.
func TestApplyDeferredDeletionsResult_RefusedWalkOnUnreadableCacheKeepsTheList(t *testing.T) {
	t0 := metav1Now()
	cr := pendingDeletionController(t0)
	d := deferredResult(false, []string{"x"})
	d.ItemsError = "create x: 500; read managed-items cache /mite/managed-items.json: bad"

	applyDeferredDeletionsResult(cr, d, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 1 {
		t.Fatalf("pending deletions = %+v, want the entry kept", cr.Status.PendingItemDeletions)
	}
	kept := cr.Status.PendingItemDeletions[0]
	if kept.Path != "team/job" || kept.Approval == nil || kept.Approval.CommandID != "c1" || !kept.DetectedAt.Equal(&t0) {
		t.Fatalf("entry = %+v, want team/job with its approval and DetectedAt", kept)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionTrue {
		t.Fatalf("DeletionPending = %s, want True", got)
	}
}

// TestApplyDeferredDeletionsResult_FailedSaveWalkWithNoDeferralsClears covers
// a walk that ran the remove step, then failed to save the cache. The walk
// reached the remove step, so the pending list clears.
func TestApplyDeferredDeletionsResult_FailedSaveWalkWithNoDeferralsClears(t *testing.T) {
	cr := pendingDeletionController(metav1Now())
	d := &mitev1.DesiredStateResult{ItemsSuccess: false, ItemsRemovalReached: true}

	applyDeferredDeletionsResult(cr, d, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %+v, want none", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False", got)
	}
}

func TestApplyDeferredDeletionsResult_FailedWalkWithDeferralsRebuilds(t *testing.T) {
	t0 := metav1Now()
	cr := pendingDeletionController(t0)

	applyDeferredDeletionsResult(cr, deferredResult(false, nil, "team/job", "team/other"), metav1Now())

	if len(cr.Status.PendingItemDeletions) != 2 {
		t.Fatalf("pending deletions = %+v, want two entries", cr.Status.PendingItemDeletions)
	}
	kept := cr.Status.PendingItemDeletions[0]
	if kept.Path != "team/job" || kept.Approval == nil || kept.Approval.CommandID != "c1" || !kept.DetectedAt.Equal(&t0) {
		t.Fatalf("entry = %+v, want team/job with its approval and DetectedAt", kept)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionTrue {
		t.Fatalf("DeletionPending = %s, want True", got)
	}
}

func TestApplyDeferredDeletionsResult_SupersededCellInstanceChangesNothing(t *testing.T) {
	results := map[string]*mitev1.DesiredStateResult{
		"walk without deferrals": deferredResult(true, nil),
		"deferrals":              deferredResult(false, nil, "team/other"),
	}
	for name, d := range results {
		t.Run(name, func(t *testing.T) {
			cr := pendingDeletionController(metav1Now())
			cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
			cr.Status.Cell = &v1alpha1.CellStatus{Instance: "new"}
			d.CellInstance = "old"

			applyDeferredDeletionsResult(cr, d, metav1Now())

			if len(cr.Status.PendingItemDeletions) != 1 || cr.Status.PendingItemDeletions[0].Path != "team/job" {
				t.Fatalf("pending deletions = %+v, want the list unchanged", cr.Status.PendingItemDeletions)
			}
			if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionTrue {
				t.Fatalf("DeletionPending = %s, want True", got)
			}
		})
	}
}

func TestApplyDeferredDeletionsResult_CurrentCellInstanceApplies(t *testing.T) {
	cr := pendingDeletionController(metav1Now())
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Instance: "new"}
	d := deferredResult(true, nil)
	d.CellInstance = "new"

	applyDeferredDeletionsResult(cr, d, metav1Now())

	if len(cr.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pending deletions = %+v, want none", cr.Status.PendingItemDeletions)
	}
	if got := deletionPendingStatus(t, &cr.Status); got != metav1.ConditionFalse {
		t.Fatalf("DeletionPending = %s, want False", got)
	}
}

func TestReconcile_AbortedItemsWalkKeepsThePendingDeletions(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{
		{Path: "team/job", Approval: &v1alpha1.DeletionApproval{CommandID: "c1"}},
	}
	transport.drainResults = []*mitev1.CommandResult{mkDSR(true, true, true, false, "", "", "", "parse items: bad yaml", "hash-1")}

	_ = rec.reconcileController(context.Background(), cr)

	if len(cr.Status.PendingItemDeletions) != 1 || cr.Status.PendingItemDeletions[0].Approval == nil {
		t.Fatalf("pending deletions = %+v, want the entry and its approval kept", cr.Status.PendingItemDeletions)
	}
}
