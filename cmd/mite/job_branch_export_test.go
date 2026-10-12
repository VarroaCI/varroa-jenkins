package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// branchFixtureBody is a plugin branch-inventory response with two
// projects. The second project's heads arrive in a different order, and
// lastScan is null, as the plugin sends it before the scan path.
const branchFixtureBody = `{
  "projects": [
    {"jobPath": "payments/mb", "indexedAt": "2026-10-12T10:00:00Z", "indexResult": "SUCCESS",
     "heads": [
       {"name": "main", "jobPath": "payments/mb/main", "kind": "branch", "orphaned": false, "revision": "abc"},
       {"name": "PR-7", "jobPath": "payments/mb/PR-7", "kind": "changeRequest", "orphaned": true, "revision": "def",
        "changeRequest": {"id": "7", "title": "Fix", "sourceBranch": "fix", "targetBranch": "main", "url": "https://forge/7"}}
     ],
     "headsTruncated": false, "lastScan": null},
    {"jobPath": "payments/other", "indexedAt": "", "indexResult": "",
     "heads": [{"name": "v1", "jobPath": "payments/other/v1", "kind": "tag", "orphaned": false, "revision": "1"}],
     "headsTruncated": true, "lastScan": null}
  ],
  "total": 2,
  "truncated": false
}`

// TestPollJobBranchesDecodes asserts the poll decodes the projects, heads,
// change request, and bounds into the JobBranchSet.
func TestPollJobBranchesDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobBranchesPath {
			t.Errorf("path = %q, want %q", r.URL.Path, jobBranchesPath)
		}
		_, _ = w.Write([]byte(branchFixtureBody))
	}))
	defer srv.Close()

	a := newJobScheduleTestAgent(t, srv.URL)
	set, err := a.pollJobBranches(context.Background())
	if err != nil {
		t.Fatalf("poll job branches: %v", err)
	}
	if set.Total != 2 || set.Truncated {
		t.Fatalf("total = %d truncated = %v, want 2 and false", set.Total, set.Truncated)
	}
	if len(set.Items) != 2 {
		t.Fatalf("projects = %d, want 2", len(set.Items))
	}
	mb := set.Items[0]
	if mb.JobPath != "payments/mb" || mb.IndexedAt != "2026-10-12T10:00:00Z" || mb.IndexResult != "SUCCESS" {
		t.Fatalf("project = %+v, want payments/mb indexed and SUCCESS", mb)
	}
	// Heads sort by job path, so PR-7 (payments/mb/PR-7) precedes main.
	pr := mb.Heads[0]
	if pr.Name != "PR-7" || pr.Kind != "changeRequest" || !pr.Orphaned {
		t.Fatalf("head = %+v, want orphaned change request PR-7", pr)
	}
	if pr.ChangeRequest == nil || pr.ChangeRequest.Id != "7" || pr.ChangeRequest.SourceBranch != "fix" || pr.ChangeRequest.URL != "https://forge/7" {
		t.Fatalf("change request = %+v, want id 7 from fix with its URL", pr.ChangeRequest)
	}
	if mb.LastScan != nil {
		t.Fatalf("lastScan = %+v, want nil until the scan path fills it", mb.LastScan)
	}
	other := set.Items[1]
	if !other.HeadsTruncated || other.Heads[0].Kind != "tag" {
		t.Fatalf("other = %+v, want truncated heads with a tag", other)
	}
}

// TestJobBranchHashStable asserts the hash ignores the plugin's listing
// order and changes when a head's revision changes.
func TestJobBranchHashStable(t *testing.T) {
	first := []jobBranchProjectRow{
		{JobPath: "a/mb", Heads: []jobBranchHeadRow{{Name: "b", JobPath: "a/mb/b"}, {Name: "a", JobPath: "a/mb/a"}}},
		{JobPath: "b/mb", Heads: []jobBranchHeadRow{{Name: "main", JobPath: "b/mb/main", Revision: "1"}}},
	}
	reordered := []jobBranchProjectRow{
		{JobPath: "b/mb", Heads: []jobBranchHeadRow{{Name: "main", JobPath: "b/mb/main", Revision: "1"}}},
		{JobPath: "a/mb", Heads: []jobBranchHeadRow{{Name: "a", JobPath: "a/mb/a"}, {Name: "b", JobPath: "a/mb/b"}}},
	}
	if computeJobBranchHash(first) != computeJobBranchHash(reordered) {
		t.Fatalf("hash differs under reordering, want stable")
	}

	changed := []jobBranchProjectRow{
		{JobPath: "a/mb", Heads: []jobBranchHeadRow{{Name: "a", JobPath: "a/mb/a"}, {Name: "b", JobPath: "a/mb/b"}}},
		{JobPath: "b/mb", Heads: []jobBranchHeadRow{{Name: "main", JobPath: "b/mb/main", Revision: "2"}}},
	}
	if computeJobBranchHash(first) == computeJobBranchHash(changed) {
		t.Fatalf("hash unchanged after a revision change, want a new hash")
	}
}

// TestMaybeAttachJobBranchesGate asserts the send gate: an unsent set is
// carried, an unchanged set is not, a reconnect or a changed hash forces
// a send, and a closed export gate sends nothing.
func TestMaybeAttachJobBranchesGate(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	a.jobScheduleResendAfter = 60 * 60 * 1e9 // one hour
	set := &mitev1.JobBranchSet{Hash: "h1"}
	a.jobBranchSet = set

	first := &mitev1.StateSnapshot{}
	a.maybeAttachJobBranches(first)
	if first.JobBranches != set {
		t.Fatalf("first snapshot carries no branch set, want the unsent set")
	}

	again := &mitev1.StateSnapshot{}
	a.maybeAttachJobBranches(again)
	if again.JobBranches != nil {
		t.Fatalf("unchanged set carried again, want no send")
	}

	a.markStreamReconnected()
	reconnect := &mitev1.StateSnapshot{}
	a.maybeAttachJobBranches(reconnect)
	if reconnect.JobBranches != set {
		t.Fatalf("reconnect did not force a send of the last set")
	}

	a.jobBranchSet = &mitev1.JobBranchSet{Hash: "h2"}
	changed := &mitev1.StateSnapshot{}
	a.maybeAttachJobBranches(changed)
	if changed.JobBranches == nil || changed.JobBranches.Hash != "h2" {
		t.Fatalf("changed hash not sent, got %+v", changed.JobBranches)
	}

	a.jobScheduleExportOpen.Store(false)
	closed := &mitev1.StateSnapshot{}
	a.jobBranchSet = &mitev1.JobBranchSet{Hash: "h3"}
	a.maybeAttachJobBranches(closed)
	if closed.JobBranches != nil {
		t.Fatalf("closed export gate sent a branch set")
	}
}
