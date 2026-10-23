package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestBranchScanFailedWarnings(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}}
	scans := []*mitev1.RehydrationFailedScan{
		{Folder: "payments/mb", IndexResult: "FAILURE", DeferredJobs: 2},
		{Folder: "payments/repos", IndexResult: "ABORTED", DeferredJobs: 1},
		{Folder: "search/app", IndexResult: "FAILURE", DeferredJobs: 1},
		{Folder: "orphan/app", IndexResult: "FAILURE", DeferredJobs: 1},
	}

	got := branchScanFailedWarnings(cell, scans)
	want := []v1alpha1.TenantWarning{
		{
			Tenant: "payments",
			Reason: v1alpha1.TenantReasonBranchScanFailed,
			Message: "the last branch scan of payments/mb ended with FAILURE; 2 branch job(s) were deferred until a scan succeeds or rehydrateDeadline passes; " +
				"the last branch scan of payments/repos ended with ABORTED; 1 branch job(s) were deferred until a scan succeeds or rehydrateDeadline passes",
		},
		{
			Tenant:  "search",
			Reason:  v1alpha1.TenantReasonBranchScanFailed,
			Message: "the last branch scan of search/app ended with FAILURE; 1 branch job(s) were deferred until a scan succeeds or rehydrateDeadline passes",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("branchScanFailedWarnings =\n%+v\nwant\n%+v", got, want)
	}
	if out := branchScanFailedWarnings(nil, scans); out != nil {
		t.Errorf("nil cell: got %v, want nil", out)
	}
}

func TestBranchScanFailedAfter(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}}
	stale := v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "old"}
	other := v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "held"}
	searchOld := v1alpha1.TenantWarning{Tenant: "search", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "search old"}
	existing := []v1alpha1.TenantWarning{stale, other, searchOld}
	matched := map[string]bool{"payments": true}
	ok := &mitev1.DesiredStateResult{RehydrationSuccess: true}

	t.Run("replaces only the matched tenant", func(t *testing.T) {
		d := &mitev1.DesiredStateResult{
			RehydrationSuccess: true,
			RehydrationFailedScans: []*mitev1.RehydrationFailedScan{
				{Folder: "payments/mb", IndexResult: "FAILURE", DeferredJobs: 1},
			},
		}
		got := branchScanFailedAfter(existing, cell, d, matched)
		if len(got) != 3 {
			t.Fatalf("got %d warnings, want 3: %+v", len(got), got)
		}
		if got[0] != other {
			t.Errorf("other reason changed: %+v", got[0])
		}
		if got[1] != searchOld {
			t.Errorf("search warning changed: %+v", got[1])
		}
		if got[2].Tenant != "payments" || got[2].Reason != v1alpha1.TenantReasonBranchScanFailed || got[2].Message == "old" {
			t.Errorf("payments warning not replaced: %+v", got[2])
		}
	})

	t.Run("empty fresh list clears the matched tenant", func(t *testing.T) {
		got := branchScanFailedAfter(existing, cell, ok, matched)
		want := []v1alpha1.TenantWarning{other, searchOld}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("keeps the list when the inventory is unknown", func(t *testing.T) {
		d := &mitev1.DesiredStateResult{RehydrationSuccess: true, RehydrationBranchScansUnknown: true}
		if got := branchScanFailedAfter(existing, cell, d, matched); !reflect.DeepEqual(got, existing) {
			t.Errorf("got %+v, want unchanged", got)
		}
	})

	t.Run("keeps the list when rehydration did not succeed", func(t *testing.T) {
		d := &mitev1.DesiredStateResult{RehydrationSuccess: false}
		if got := branchScanFailedAfter(existing, cell, d, matched); !reflect.DeepEqual(got, existing) {
			t.Errorf("got %+v, want unchanged", got)
		}
	})

	t.Run("no matched tenant keeps the list", func(t *testing.T) {
		if got := branchScanFailedAfter(existing, cell, ok, nil); !reflect.DeepEqual(got, existing) {
			t.Errorf("got %+v, want unchanged", got)
		}
	})
}

func TestPruneBranchScanFailedWarnings(t *testing.T) {
	cell := &v1alpha1.CellStatus{
		TenantWarnings: []v1alpha1.TenantWarning{
			{Tenant: "gone", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "no entry"},
			{Tenant: "idle", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "empty deferred"},
			{Tenant: "deadline", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "deadline give-up"},
			{Tenant: "gone", Reason: v1alpha1.TenantReasonItemsHeld, Message: "other reason stays"},
		},
		Rehydration: []v1alpha1.CellRehydrationEntry{
			{Tenant: "idle"},
			{Tenant: "deadline", Deferred: []string{"deadline/mb/main"}, DeadlineExceeded: true},
		},
	}

	pruneBranchScanFailedWarnings(cell)

	want := []v1alpha1.TenantWarning{
		{Tenant: "deadline", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "deadline give-up"},
		{Tenant: "gone", Reason: v1alpha1.TenantReasonItemsHeld, Message: "other reason stays"},
	}
	if !reflect.DeepEqual(cell.TenantWarnings, want) {
		t.Errorf("pruned warnings = %+v, want %+v", cell.TenantWarnings, want)
	}
	pruneBranchScanFailedWarnings(nil)
}

func TestJoinScanMessagesKeepsCountOfDroppedFolders(t *testing.T) {
	long := strings.Repeat("x", 300)
	msgs := []string{long + "-a", long + "-b", long + "-c", long + "-d"}
	got := joinScanMessages(msgs)
	if len(got) > 1024 {
		t.Fatalf("message is %d bytes, want at most 1024", len(got))
	}
	if !strings.Contains(got, "and 1 more folder(s) with a failed scan") {
		t.Errorf("expected a count of the dropped folder, got %q", got)
	}
	if short := joinScanMessages([]string{"one", "two"}); short != "one; two" {
		t.Errorf("short join = %q, want %q", short, "one; two")
	}
}
