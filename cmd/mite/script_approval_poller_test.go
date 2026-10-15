package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestPollScriptApprovals_DecodesRows proves the poller converts the plugin
// response into the wire set, requesters included.
func TestPollScriptApprovals_DecodesRows(t *testing.T) {
	resp := scriptApprovalResponse{
		Pending: []pendingSignatureRow{{
			Signature: "method java.lang.System exit int",
			Dangerous: true,
			Requesters: []pendingRequesterRow{
				{Tenant: "team-a", JobPath: "team-a/build", BuildNumber: 4, RunID: "r4"},
			},
		}},
		Total: 1,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != scriptApprovalsPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	a := newJobScheduleTestAgent(t, srv.URL)
	set, err := a.pollScriptApprovals(context.Background())
	if err != nil {
		t.Fatalf("pollScriptApprovals: %v", err)
	}
	if set.Total != 1 || len(set.Items) != 1 {
		t.Fatalf("set = %+v, want one item and total 1", set)
	}
	item := set.Items[0]
	if item.Signature != "method java.lang.System exit int" || !item.Dangerous {
		t.Fatalf("item = %+v", item)
	}
	if len(item.Requesters) != 1 || item.Requesters[0].JobPath != "team-a/build" || item.Requesters[0].RunId != "r4" {
		t.Fatalf("requesters = %+v", item.Requesters)
	}
}

// TestAttachPendingSignatures_NilBeforePoll proves a snapshot carries no
// pending set until the first successful poll.
func TestAttachPendingSignatures_NilBeforePoll(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://127.0.0.1:1")
	a.setAppliedTenants(true, nil)
	snap := &mitev1.StateSnapshot{}
	a.attachPendingSignatures(snap)
	if snap.PendingSignatures != nil {
		t.Fatalf("pending set attached before any poll: %+v", snap.PendingSignatures)
	}
	a.storeScriptApprovalSet(&mitev1.PendingSignatureSet{Total: 2})
	a.attachPendingSignatures(snap)
	if snap.PendingSignatures == nil || snap.PendingSignatures.Total != 2 {
		t.Fatalf("cached set not attached: %+v", snap.PendingSignatures)
	}
}

// TestScriptApprovalPoller_SkipsNonCell proves the poller makes no request
// while the tenant gate reports no cell.
func TestScriptApprovalPoller_SkipsNonCell(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a := newJobScheduleTestAgent(t, srv.URL)
	a.scriptApprovalInterval = time.Millisecond
	a.setAppliedTenants(false, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.startScriptApprovalPoller(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if n := hits.Load(); n != 0 {
		t.Fatalf("poller made %d requests on a non-cell", n)
	}
}

// TestAttachPendingSignatures_SkipsNonCell proves a snapshot carries no
// pending set once the tenant gate reports a non-cell, even with a cached set.
func TestAttachPendingSignatures_SkipsNonCell(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://127.0.0.1:1")
	a.storeScriptApprovalSet(&mitev1.PendingSignatureSet{Total: 1})
	a.setAppliedTenants(false, nil)
	snap := &mitev1.StateSnapshot{}
	a.attachPendingSignatures(snap)
	if snap.PendingSignatures != nil {
		t.Fatalf("non-cell snapshot carries a pending set: %+v", snap.PendingSignatures)
	}
}
