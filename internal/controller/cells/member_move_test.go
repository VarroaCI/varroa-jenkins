package cells

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// oldProfileCell is appliedCell on a profile the tenant no longer asks for.
func oldProfileCell(name string, tenants ...string) CellCandidate {
	c := appliedCell(name, tenants...)
	c.ResolvedProfile = "old-profile"
	return c
}

// drainedOldCell is an old-profile cell that still lists the tenant in its
// tenant list but has dropped the tenant's folder. It runs a pod, so only
// the folder test decides its drain.
func drainedOldCell(name string) CellCandidate {
	c := readyCell(name, "payments")
	c.ResolvedProfile = "old-profile"
	c.Instance = "uid-" + name
	return c
}

// threeMemberStatus builds a placed 2+1 tenant: slot 0 holds slot0, slot 1 holds cell-b, and the standby holds cell-c.
func threeMemberStatus(slot0 string) *v1alpha1.TenantStatus {
	status := placedStatus(3)
	status.Placement.Cell = slot0
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus(slot0, v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
		memberStatus("cell-c", v1alpha1.TenantMemberRoleStandby, nil),
	}
	return status
}

// setMoving marks the Placed condition as a voluntary move in progress.
func setMoving(status *v1alpha1.TenantStatus) {
	status.Conditions = []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionFalse,
		Reason: v1alpha1.TenantReasonTenantMoving, Message: tenantMovingMessage,
		LastTransitionTime: metav1.NewTime(fixedNow),
	}}
}

// dropsTenant reports whether a cell patch removes the test tenant.
func dropsTenant(p *CellPatch) bool {
	return p != nil && !containsString(p.Tenants, "payments")
}

// memberBindingsOf converts a stored member list for the test helpers.
func memberBindingsOf(status *v1alpha1.TenantStatus) []MemberBinding {
	out := make([]MemberBinding, 0, len(status.Placement.Members))
	for _, m := range status.Placement.Members {
		out = append(out, MemberBinding{Cell: m.Cell, Namespace: m.CellNamespace, Role: m.Role, Slot: m.Slot})
	}
	return out
}

// TestPlaceKeepsTenantMovingForAMultiMemberTenant asserts a profile change
// on a 2+1 tenant keeps the voluntary move in progress across ticks. Before
// the fix, the second tick restarted the move.
func TestPlaceKeepsTenantMovingForAMultiMemberTenant(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	in.Cells = []CellCandidate{
		oldProfileCell("cell-a", "payments"), oldProfileCell("cell-b", "payments"),
		oldProfileCell("cell-c", "payments"),
		readyCell("cell-d"), readyCell("cell-e"), readyCell("cell-f"),
	}
	status := threeMemberStatus("cell-a")
	status.Conditions = []v1alpha1.TenantCondition{{
		Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionTrue,
		Reason: v1alpha1.TenantReasonMembersApplied,
	}}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 1: %v", err)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Fatalf("tick 1: want TenantMoving, got %+v", cond)
	}

	in.Now = fixedClock(fixedNow.Add(time.Minute))
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Fatalf("tick 2: want TenantMoving, got %+v", cond)
	}
	if !cond.LastTransitionTime.Time.Equal(fixedNow) {
		t.Errorf("tick 2: LastTransitionTime = %v, want %v (the wait began at tick 1)",
			cond.LastTransitionTime.Time, fixedNow)
	}
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Errorf("tick 2: placement = %s gen %d, want cell-a gen 3",
			status.Placement.Cell, status.Placement.Generation)
	}
}

// TestPlaceCompletesAMultiMemberVoluntaryMove asserts a drained move places
// every member on the new profile, drops the tenant from every old cell,
// and announces the three-member list once.
func TestPlaceCompletesAMultiMemberVoluntaryMove(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	in.Cells = []CellCandidate{
		drainedOldCell("cell-a"), drainedOldCell("cell-b"), drainedOldCell("cell-c"),
		readyCell("cell-d"), readyCell("cell-e"), readyCell("cell-f"),
	}
	status := threeMemberStatus("cell-a")
	setMoving(status)

	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Cell != "cell-d" || status.Placement.Generation != 4 {
		t.Fatalf("placement = %s gen %d, want cell-d gen 4", status.Placement.Cell, status.Placement.Generation)
	}
	got := map[string]bool{}
	for _, m := range status.Placement.Members {
		got[m.Cell] = true
	}
	for _, want := range []string{"cell-d", "cell-e", "cell-f"} {
		if !got[want] {
			t.Errorf("members %v missing %s", got, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("members = %v, want exactly {cell-d, cell-e, cell-f}", got)
	}
	for _, old := range []string{"cell-a", "cell-b", "cell-c"} {
		if !dropsTenant(patchFor(out, old)) {
			t.Errorf("cell %s: want a patch that drops the tenant, got %+v", old, patchFor(out, old))
		}
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoved {
		t.Errorf("want TenantMoved, got %+v", cond)
	}
	if len(seen) != 1 || len(seen[0].Members) != 3 {
		t.Errorf("want one announce with 3 members, got %d announces", len(seen))
	}
}

// TestPlaceWaitsForAStaleMemberToDrain asserts the move waits while a stale
// member cell still holds the tenant's pending deletion.
func TestPlaceWaitsForAStaleMemberToDrain(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	b := drainedOldCell("cell-b")
	b.PendingDeletionPaths = []string{"payments/api"}
	in.Cells = []CellCandidate{
		drainedOldCell("cell-a"), b, drainedOldCell("cell-c"),
		readyCell("cell-d"), readyCell("cell-e"), readyCell("cell-f"),
	}
	status := threeMemberStatus("cell-a")
	setMoving(status)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Cell != "cell-a" || status.Placement.Generation != 3 {
		t.Errorf("placement = %s gen %d, want cell-a gen 3", status.Placement.Cell, status.Placement.Generation)
	}
	if len(out.CellPatches) != 0 {
		t.Errorf("want no cell patches while a stale member drains, got %+v", out.CellPatches)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Errorf("want TenantMoving, got %+v", cond)
	}
}

// TestPlaceDoesNotWaitForAStaleMemberWithNoPod asserts a stale member cell
// that runs no pod does not hold the move. The cell still names the folder
// and a pending deletion, so only the no-pod rule counts it drained.
func TestPlaceDoesNotWaitForAStaleMemberWithNoPod(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	b := oldProfileCell("cell-b", "payments")
	b.PendingDeletionPaths = []string{"payments/api"}
	b.Instance = ""
	in.Cells = []CellCandidate{
		drainedOldCell("cell-a"), b, drainedOldCell("cell-c"),
		readyCell("cell-d"), readyCell("cell-e"), readyCell("cell-f"),
	}
	status := threeMemberStatus("cell-a")
	setMoving(status)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Cell != "cell-d" || status.Placement.Generation != 4 {
		t.Fatalf("placement = %s gen %d, want cell-d gen 4", status.Placement.Cell, status.Placement.Generation)
	}
	if !dropsTenant(patchFor(out, "cell-b")) {
		t.Errorf("cell-b: want a patch that drops the tenant, got %+v", patchFor(out, "cell-b"))
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonTenantMoved {
		t.Errorf("want TenantMoved, got %+v", cond)
	}
}

// TestPlaceKeepsAStaleMemberUntilAReplacementExists asserts a stale slot
// and a stale standby keep their cells while no free cell exists.
func TestPlaceKeepsAStaleMemberUntilAReplacementExists(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-d", "payments"),
		oldProfileCell("cell-b", "payments"), oldProfileCell("cell-c", "payments"),
	}
	status := threeMemberStatus("cell-d")

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Generation != 3 {
		t.Errorf("generation = %d, want 3", status.Placement.Generation)
	}
	if patchFor(out, "cell-b") != nil || patchFor(out, "cell-c") != nil {
		t.Errorf("want no patch for the stale members, got %+v", out.CellPatches)
	}
	if len(status.Placement.Members) != 3 {
		t.Errorf("members = %d, want 3: %+v", len(status.Placement.Members), status.Placement.Members)
	}
	wantStale := []bool{false, true, true}
	for i, m := range status.Placement.Members {
		if m.Stale != wantStale[i] {
			t.Errorf("member %d (%s) stale = %t, want %t", i, m.Cell, m.Stale, wantStale[i])
		}
	}
	assertMembersStale(t, status, "slot 1 cells-ns/cell-b", "standby cells-ns/cell-c", `pool "jenkins-lts"`)
	if strings.Contains(placedMessage(status), "cell-d") {
		t.Errorf("message names the fresh member cell-d: %q", placedMessage(status))
	}
}

// assertMembersStale checks that Placed reports MembersStale as True and
// that its message contains each wanted fragment.
func assertMembersStale(t *testing.T, status *v1alpha1.TenantStatus, fragments ...string) {
	t.Helper()
	var cond *v1alpha1.TenantCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TenantConditionPlaced {
			cond = &status.Conditions[i]
		}
	}
	if cond == nil {
		t.Fatalf("no Placed condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TenantReasonMembersStale {
		t.Fatalf("Placed = %s/%s, want True/MembersStale", cond.Status, cond.Reason)
	}
	for _, f := range fragments {
		if !strings.Contains(cond.Message, f) {
			t.Errorf("message %q lacks %q", cond.Message, f)
		}
	}
}

// placedMessage answers the Placed condition message, or an empty string.
func placedMessage(status *v1alpha1.TenantStatus) string {
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced {
			return c.Message
		}
	}
	return ""
}

// TestPlaceRefreshesTheStaleFlagEachTick asserts the stale flag follows
// this tick's cells without an announcement, and that the Placed reason
// follows the flags.
func TestPlaceRefreshesTheStaleFlagEachTick(t *testing.T) {
	tenant := slotTenant(2, 1)
	status := threeMemberStatus("cell-d")
	status.Placement.Members[1].Stale = true

	in := testInputs()
	in.Cells = []CellCandidate{
		appliedCell("cell-d", "payments"),
		appliedCell("cell-b", "payments"),
		oldProfileCell("cell-c", "payments"),
	}
	in.Announce = failingAnnounce()
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 1: %v", err)
	}
	if status.Placement.Members[1].Stale {
		t.Errorf("tick 1: cell-b is current, want stale=false")
	}
	if !status.Placement.Members[2].Stale {
		t.Errorf("tick 1: cell-c serves the old profile, want stale=true")
	}
	assertMembersStale(t, status, "standby cells-ns/cell-c")

	in.Cells[2] = appliedCell("cell-c", "payments")
	in.Announce = succeedingAnnounce()
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	for i, m := range status.Placement.Members {
		if m.Stale {
			t.Errorf("tick 2: member %d (%s) stale, want false", i, m.Cell)
		}
	}
	if got := placedReason(status); got != v1alpha1.TenantReasonMembersApplied {
		t.Errorf("tick 2 reason = %q, want MembersApplied", got)
	}
}

// TestPlaceKeepsTheMemberReasonTransitionTime asserts a repeat tick on an
// unchanged MembersStale state keeps the Placed transition time.
func TestPlaceKeepsTheMemberReasonTransitionTime(t *testing.T) {
	tenant := slotTenant(2, 1)
	status := threeMemberStatus("cell-d")
	in := testInputs()
	in.Cells = []CellCandidate{
		appliedCell("cell-d", "payments"),
		appliedCell("cell-b", "payments"),
		oldProfileCell("cell-c", "payments"),
	}
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 1: %v", err)
	}
	in.Now = fixedClock(fixedNow.Add(time.Minute))
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced && !c.LastTransitionTime.Time.Equal(fixedNow) {
			t.Errorf("Placed transition = %s, want %s", c.LastTransitionTime.Time, fixedNow)
		}
	}
}

// TestPlaceReplacesStaleMembersWithFreshCells asserts each replaceable
// stale member moves to a fresh cell in one tick, and each released cell
// takes a cell-scoped purge.
func TestPlaceReplacesStaleMembersWithFreshCells(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-d", "payments"),
		oldProfileCell("cell-b", "payments"), oldProfileCell("cell-c", "payments"),
		readyCell("cell-e"), readyCell("cell-f"),
	}
	status := threeMemberStatus("cell-d")

	var purges []PurgeRequest
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		purges = append(purges, req)
		return true, nil
	}

	// Tick 1: b and c are stale and still hold the folder. Both drain, and
	// no replacement binds.
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Generation != 3 {
		t.Errorf("tick 1 generation = %d, want 3", status.Placement.Generation)
	}
	if !storedMemberOf(status, "cell-b").Draining || !storedMemberOf(status, "cell-c").Draining {
		t.Errorf("stale members b and c should drain, got %+v", status.Placement.Members)
	}
	for _, name := range []string{"cell-b", "cell-c", "cell-e", "cell-f"} {
		if patchFor(out, name) != nil || claimedCells(out)[name] {
			t.Errorf("tick 1 must not patch or claim %s: %+v", name, out.CellPatches)
		}
	}
	if n := strings.Count(placedMessage(status), "(draining)"); n != 2 {
		t.Errorf("tick 1 message should mark two draining members, got %d: %q", n, placedMessage(status))
	}

	// Tick 2: cell-b still owes an item deletion, so it keeps draining.
	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-b", func(c *CellCandidate) {
		c.AppliedTenants = nil
		c.PendingDeletionPaths = []string{"payments/api"}
	})
	out, err = Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	if !storedMemberOf(status, "cell-b").Draining || storedMemberOf(status, "cell-e").Cell != "" {
		t.Errorf("tick 2 should hold the drain of cell-b, got %+v", status.Placement.Members)
	}

	// Tick 3: cell-b is clear and cell-c drops the folder. Both release.
	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-b", func(c *CellCandidate) { c.PendingDeletionPaths = nil })
	editCell(&in, "cell-c", func(c *CellCandidate) { simulateMemberDrain(status, c) })
	out, err = Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 3: %v", err)
	}
	if status.Placement.Generation != 4 {
		t.Errorf("generation = %d, want 4", status.Placement.Generation)
	}
	if !dropsTenant(patchFor(out, "cell-b")) || !dropsTenant(patchFor(out, "cell-c")) {
		t.Errorf("want drained members b and c patched without the tenant, got %+v", out.CellPatches)
	}
	slot1 := findMember(memberBindingsOf(status), "cell-e")
	if slot1 == nil || slot1.Slot == nil || *slot1.Slot != 1 {
		t.Errorf("slot 1 should hold cell-e, got %+v", status.Placement.Members)
	}
	standby := findMember(memberBindingsOf(status), "cell-f")
	if standby == nil || standby.Role != v1alpha1.TenantMemberRoleStandby {
		t.Errorf("standby should hold cell-f, got %+v", status.Placement.Members)
	}
	scoped := 0
	for _, p := range purges {
		if p.Cell != "" {
			scoped++
		}
	}
	if scoped != 2 {
		t.Errorf("cell-scoped purges = %d, want 2: %+v", scoped, purges)
	}
}

// TestPlaceReplacesAMemberOnAPoolChange asserts a slot on a cell of the old
// pool moves to a free cell of the new pool.
func TestPlaceReplacesAMemberOnAPoolChange(t *testing.T) {
	in := testInputs()
	in.Pools = []CellPoolSummary{
		{Name: "small", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1},
		{Name: "big", ProfileRef: "jenkins-lts", MaxTenantsPerCell: 10},
	}
	tenant := slotTenant(2, 0)
	tenant.Spec.Pipelines.CellPool = "big"
	d := poolCell("cell-d", "big", "payments")
	d.AppliedTenants = []string{"payments"}
	b := poolCell("cell-b", "small", "payments")
	b.AppliedTenants = []string{"payments"}
	b.Instance = "uid-cell-b"
	in.Cells = []CellCandidate{d, b, poolCell("cell-e", "big")}
	status := placedStatus(3)
	status.Placement.Cell = "cell-d"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-d", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	// Tick 1: cell-b is on the old pool and still holds the folder, so it drains.
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(status.Placement.Members) != 2 || status.Placement.Members[1].Cell != "cell-b" ||
		!status.Placement.Members[1].Draining {
		t.Fatalf("slot 1 should keep cell-b while it drains, got %+v", status.Placement.Members)
	}

	// Tick 2: cell-b has dropped the folder, so slot 1 moves to cell-e.
	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-b", func(c *CellCandidate) { c.AppliedTenants = nil })
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	if len(status.Placement.Members) != 2 || status.Placement.Members[1].Cell != "cell-e" {
		t.Fatalf("slot 1 should move to cell-e, got %+v", status.Placement.Members)
	}
}

// TestSlotZeroPromotionSkipsAStaleStandby asserts an old-profile standby
// never takes slot 0 when the slot's cell evacuates.
func TestSlotZeroPromotionSkipsAStaleStandby(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(1, 1)
	evacuating := appliedCell("cell-a", "payments")
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{
		evacuating, oldProfileCell("cell-c", "payments"), readyCell("cell-d"),
	}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-c", v1alpha1.TenantMemberRoleStandby, nil),
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Cell != "cell-d" {
		t.Fatalf("slot 0 should move to cell-d, got %s", status.Placement.Cell)
	}
}

// TestPlaceKeepsCellNotReadyForAMultiMemberTenant asserts CellNotReady on
// slot 0 survives the member reason of a multi-member tenant.
func TestPlaceKeepsCellNotReadyForAMultiMemberTenant(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	notReady := appliedCell("cell-a", "payments")
	notReady.CellReady = false
	in.Cells = []CellCandidate{notReady, appliedCell("cell-b", "payments")}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if cond := condition(status); cond == nil || cond.Reason != v1alpha1.TenantReasonCellNotReady {
		t.Fatalf("want CellNotReady, got %+v", cond)
	}
}

// TestDesiredMembersReleasesAStaleStandbyFirst asserts a scale-down of the
// standbys releases a stale standby before a current one.
func TestDesiredMembersReleasesAStaleStandbyFirst(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(1, 1)
	slot0 := appliedCell("cell-a", "payments")
	in.Cells = []CellCandidate{
		slot0, drainedOldCell("cell-b"), appliedCell("cell-c", "payments"),
	}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleStandby, nil),
		memberStatus("cell-c", v1alpha1.TenantMemberRoleStandby, nil),
	}

	members, _ := desiredMembers(in, tenant, status, &slot0, fixedNow)
	if findMember(members, "cell-b") != nil {
		t.Errorf("the stale standby cell-b should be released, got %+v", members)
	}
	if findMember(members, "cell-c") == nil {
		t.Errorf("the current standby cell-c should be kept, got %+v", members)
	}
}
