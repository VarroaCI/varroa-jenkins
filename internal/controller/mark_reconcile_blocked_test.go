package controller

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestMarkReconcileBlocked_SetsConditionAndStatus asserts that calling
// markReconcileBlocked on a controller with no pre-existing
// ConditionReconcileBlocked sets the condition to True, stamps
// LastReconcileError/LastReconcileErrorAt, and triggers exactly one
// status-patch call.
func TestMarkReconcileBlocked_SetsConditionAndStatus(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()
	cr := testController("test-ctrl", "ns1", v1alpha1.ControllerPhasePending)

	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, "spec.composedBundleRef is required")

	// Verify condition is set.
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond == nil {
		t.Fatal("ConditionReconcileBlocked not found")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Status=True, got %q", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonReconcileBlockedBundleUnreadable {
		t.Errorf("expected Reason=%q, got %q", v1alpha1.ReasonReconcileBlockedBundleUnreadable, cond.Reason)
	}
	if cond.Message != "spec.composedBundleRef is required" {
		t.Errorf("expected Message=%q, got %q", "spec.composedBundleRef is required", cond.Message)
	}

	// Verify status fields.
	if cr.Status.LastReconcileError != "spec.composedBundleRef is required" {
		t.Errorf("LastReconcileError = %q", cr.Status.LastReconcileError)
	}
	if cr.Status.LastReconcileErrorAt == nil {
		t.Fatal("LastReconcileErrorAt is nil")
	}

	// Verify exactly one status-patch call happened.
	if client.lastPatchedStatus == nil {
		t.Fatal("PatchControllerStatus was not called")
	}
	// The patched status should carry the same fields we set.
	if client.lastPatchedStatus.LastReconcileError != "spec.composedBundleRef is required" {
		t.Errorf("patched LastReconcileError = %q", client.lastPatchedStatus.LastReconcileError)
	}
	if client.lastPatchedStatus.LastReconcileErrorAt == nil {
		t.Error("patched LastReconcileErrorAt is nil")
	}
}

// TestMarkReconcileBlocked_ClassNotFoundSite_ExactlyOnePatch guards the
// regression where the ClassNotFound error path in handleProvisioning could
// end up calling PatchControllerStatus twice in a single reconcile pass (once
// for the ClassResolved condition and once inside markReconcileBlocked). The
// test drives markReconcileBlocked directly — the key assertion is that the
// testClient records exactly one patch call, not two.
func TestMarkReconcileBlocked_ClassNotFoundSite_ExactlyOnePatch(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()
	cr := testController("test-ctrl", "ns1", v1alpha1.ControllerPhasePending)

	// Simulate what handleProvisioning does: set the ClassResolved condition,
	// then call markReconcileBlocked.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionClassResolved,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ReasonClassNotFound,
		Message: `ControllerClass "myclass" not found`,
	})
	// Record the patch count before — must be zero so the count after is
	// attributable solely to markReconcileBlocked.
	if client.patchCount != 0 {
		t.Fatalf("PatchControllerStatus was already called %d time(s) before markReconcileBlocked — test precondition violated", client.patchCount)
	}

	// Now call markReconcileBlocked — it does its own persistStatusDiagnostics.
	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedClassResolutionFailed, `ControllerClass "myclass" not found`)

	// Verify that exactly one status persist happened total — the one inside
	// markReconcileBlocked — i.e. it folded the condition into a single patch
	// rather than double-patching in one pass.
	if client.patchCount != 1 {
		t.Errorf("expected exactly 1 PatchControllerStatus call, got %d", client.patchCount)
	}
	if client.lastPatchedStatus == nil {
		t.Fatal("PatchControllerStatus was not called at all")
	}

	// Both conditions must be present in the patched status.
	cond := findCondition(client.lastPatchedStatus.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("ConditionReconcileBlocked not set in patched status")
	}
	classCond := findCondition(client.lastPatchedStatus.Conditions, v1alpha1.ConditionClassResolved)
	if classCond == nil || classCond.Status != metav1.ConditionFalse {
		t.Error("ConditionClassResolved not preserved in patched status")
	}
}

// TestMarkReconcileBlocked_MessageTruncation asserts that a message longer than
// 2048 bytes is truncated to exactly 2048 bytes in both the condition's
// Message and Status.LastReconcileError.
func TestMarkReconcileBlocked_MessageTruncation(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()
	cr := testController("test-ctrl", "ns1", v1alpha1.ControllerPhasePending)

	longMsg := strings.Repeat("x", 3000)
	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, longMsg)

	// In-memory condition message must be truncated.
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond == nil {
		t.Fatal("ConditionReconcileBlocked not found")
	}
	if len(cond.Message) != 2048 {
		t.Errorf("condition Message length = %d, want 2048", len(cond.Message))
	}
	if cr.Status.LastReconcileError != cond.Message {
		t.Errorf("LastReconcileError (%d bytes) != condition Message (%d bytes)", len(cr.Status.LastReconcileError), len(cond.Message))
	}

	// Patched status also truncated.
	if client.lastPatchedStatus == nil {
		t.Fatal("PatchControllerStatus not called")
	}
	if len(client.lastPatchedStatus.LastReconcileError) != 2048 {
		t.Errorf("patched LastReconcileError length = %d, want 2048", len(client.lastPatchedStatus.LastReconcileError))
	}
}

// TestMarkReconcileBlocked_LastTransitionTimeStable asserts that calling
// markReconcileBlocked twice in a row while the condition is already True
// leaves LastTransitionTime unchanged while LastReconcileErrorAt advances.
func TestMarkReconcileBlocked_LastTransitionTimeStable(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()
	cr := testController("test-ctrl", "ns1", v1alpha1.ControllerPhasePending)

	// First call establishes the condition.
	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, "first error")
	cond1 := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond1 == nil {
		t.Fatal("condition not set after first call")
	}
	firstTransition := cond1.LastTransitionTime
	firstErrAt := *cr.Status.LastReconcileErrorAt

	// Small sleep so timestamps can differ (setCondition uses metav1.Now()).
	time.Sleep(10 * time.Millisecond)

	// Second call with the same reason but different message.
	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, "second error")
	cond2 := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond2 == nil {
		t.Fatal("condition not found after second call")
	}
	secondTransition := cond2.LastTransitionTime
	secondErrAt := *cr.Status.LastReconcileErrorAt

	// LastTransitionTime must be stable (status didn't flip).
	if !firstTransition.Time.Equal(secondTransition.Time) {
		t.Errorf("LastTransitionTime changed from %v to %v — should be stable", firstTransition.Time, secondTransition.Time)
	}

	// LastReconcileErrorAt must advance (it always reflects the latest call).
	if !secondErrAt.After(firstErrAt.Time) {
		t.Errorf("LastReconcileErrorAt did not advance: first=%v, second=%v", firstErrAt.Time, secondErrAt.Time)
	}

	// Message must be updated.
	if cond2.Message != "second error" {
		t.Errorf("condition Message = %q, want %q", cond2.Message, "second error")
	}
}

// TestMarkReconcileBlocked_TruncatesOnRuneBoundary asserts that the byte-length
// message cap never splits a multi-byte UTF-8 rune: a message whose 2048th byte
// lands mid-rune is trimmed back to a valid boundary before being persisted,
// so the Kubernetes status string stays valid UTF-8.
func TestMarkReconcileBlocked_TruncatesOnRuneBoundary(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()
	cr := testController("utf8-ctrl", "ns1", v1alpha1.ControllerPhasePending)

	// 2047 ASCII bytes + '€' (E2 82 AC, 3 bytes): a cut at the 2048-byte cap
	// splits the euro sign after its first byte, leaving a dangling E2.
	msg := strings.Repeat("a", reconcileBlockedMessageCap-1) + "€"
	if len(msg) <= reconcileBlockedMessageCap {
		t.Fatalf("precondition: message must exceed cap, got len %d", len(msg))
	}

	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, msg)

	got := cr.Status.LastReconcileError
	if len(got) > reconcileBlockedMessageCap {
		t.Errorf("message exceeds cap: len %d > %d", len(got), reconcileBlockedMessageCap)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("persisted LastReconcileError is not valid UTF-8: %q", got)
	}
	// The boundary-split euro must be dropped whole, not left partial.
	if strings.ContainsRune(got, '€') {
		t.Error("expected the boundary-split '€' rune to be dropped")
	}
	if !strings.HasSuffix(got, "a") {
		t.Error("expected the truncated message to end at the last full rune")
	}
	// The same valid-UTF-8 value must reach the persisted patch.
	if client.lastPatchedStatus == nil || !utf8.ValidString(client.lastPatchedStatus.LastReconcileError) {
		t.Error("patched LastReconcileError is missing or not valid UTF-8")
	}
}
