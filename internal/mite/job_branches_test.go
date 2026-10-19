package mite

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// oneBranchSet is a branch inventory with one project and one head.
func oneBranchSet() *mitev1.JobBranchSet {
	return &mitev1.JobBranchSet{
		TakenAtMillis: testTakenAtMillis,
		Hash:          "bh1",
		Items: []*mitev1.JobBranchProject{{
			JobPath:   "team-a/mb",
			IndexedAt: "2026-10-12T10:00:00Z",
			Heads: []*mitev1.JobBranchHead{{
				Name:    "PR-7",
				JobPath: "team-a/mb/PR-7",
				Kind:    "changeRequest",
				ChangeRequest: &mitev1.ChangeRequestInfo{
					Id: "7", Title: "Fix", SourceBranch: "fix", TargetBranch: "main", URL: "https://forge/7",
				},
			}},
		}},
		Total: 1,
	}
}

// TestBranchScanInputOfCarriesHeadPaths asserts each scan head keeps its
// job path on the way to the recorder payload, and a nil scan maps to nil.
func TestBranchScanInputOfCarriesHeadPaths(t *testing.T) {
	if got := branchScanInputOf(nil); got != nil {
		t.Fatalf("branchScanInputOf(nil) = %+v, want nil", got)
	}
	scan := &mitev1.BranchScanResult{
		ScanId: "s1",
		OrphanedHeadRefs: []*mitev1.BranchScanHead{
			{Name: "old", JobPath: "payments/mb/old"},
		},
	}
	got := branchScanInputOf(scan)
	if len(got.OrphanedHeadRefs) != 1 || got.OrphanedHeadRefs[0].Name != "old" ||
		got.OrphanedHeadRefs[0].JobPath != "payments/mb/old" {
		t.Fatalf("orphaned refs = %+v", got.OrphanedHeadRefs)
	}
	if len(got.NewHeadRefs) != 0 || len(got.ChangedHeadRefs) != 0 {
		t.Fatalf("empty lists carried heads: %+v", got)
	}
}

// TestOnSnapshotStripsJobBranches asserts the branch set leaves the
// snapshot KV copy, while the caller's own snapshot keeps it.
func TestOnSnapshotStripsJobBranches(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/mb": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	snap := &mitev1.StateSnapshot{ActualStateHash: "hash-1", JobBranches: oneBranchSet()}
	h.OnSnapshot("ctrl1", "ns1", snap)

	key := bus.SnapshotKey(h.cluster, "ns1", "ctrl1")
	waitFor(t, func() bool {
		data, gerr := h.kv.Get(key)
		return gerr == nil && len(data) > 0
	}, 5*time.Second, "snapshot KV value never written")
	kvData, err := h.kv.Get(key)
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	var kvSnap mitev1.StateSnapshot
	if err := json.Unmarshal(kvData, &kvSnap); err != nil {
		t.Fatalf("unmarshal kv snapshot: %v", err)
	}
	if kvSnap.JobBranches != nil {
		t.Errorf("KV snapshot carries job branches, want stripped")
	}
	if snap.JobBranches == nil {
		t.Error("OnSnapshot mutated the caller's snapshot; want the branch set left intact")
	}
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("publishedCount() = %d, want one job.branches envelope", got)
	}
}

// TestPublishJobBranchesAllOrNothing asserts one unresolved project
// rejects the whole envelope.
func TestPublishJobBranchesAllOrNothing(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/mb": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	set := oneBranchSet()
	set.Items = append(set.Items, &mitev1.JobBranchProject{JobPath: "unknown/mb"})
	if ok := h.publishJobBranches("ns1", "ctrl1", set); ok {
		t.Fatalf("publishJobBranches reported success with an unresolved project")
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("publishedCount() = %d, want 0 (one unresolved project publishes nothing)", got)
	}
}

// TestPublishJobBranchesRetainsOnFailure asserts a failed publish keeps
// the set for the next tick, and a later tick publishes it.
func TestPublishJobBranchesRetainsOnFailure(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("jetstream down")}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/mb": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)
	h.scheduleStreamMissingTTL = time.Millisecond

	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{JobBranches: oneBranchSet()})
	h.scheduleMu.Lock()
	retained := h.branchRetained["ns1/ctrl1"]
	h.scheduleMu.Unlock()
	if retained == nil {
		t.Fatalf("the failed set is not retained for the next tick")
	}

	time.Sleep(5 * time.Millisecond)
	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("publishedCount() = %d after the retry, want 1", got)
	}
	h.scheduleMu.Lock()
	_, stillRetained := h.branchRetained["ns1/ctrl1"]
	h.scheduleMu.Unlock()
	if stillRetained {
		t.Fatalf("the published set is still retained")
	}
}

// TestPublishJobBranchesCarriesTakenTime asserts the envelope time is the
// time the cell took the branch inventory.
func TestPublishJobBranchesCarriesTakenTime(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/mb": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	if ok := h.publishJobBranches("ns1", "ctrl1", oneBranchSet()); !ok {
		t.Fatalf("publishJobBranches reported failure")
	}
	want := time.UnixMilli(testTakenAtMillis).UTC()
	if got := pub.envelopeAt(0).OccurredAt; !got.Equal(want) {
		t.Errorf("OccurredAt = %v, want the taken time %v", got, want)
	}
}

// TestPublishJobBranchesMissingTakenTimeRejectsEnvelope asserts a branch
// set with no taken time publishes nothing.
func TestPublishJobBranchesMissingTakenTimeRejectsEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/mb": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	set := oneBranchSet()
	set.TakenAtMillis = 0
	if ok := h.publishJobBranches("ns1", "ctrl1", set); ok {
		t.Fatalf("publishJobBranches reported success with no taken time")
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("publishedCount() = %d, want 0", got)
	}
}
