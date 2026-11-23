package mite

import (
	"errors"
	"fmt"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// senderKinds lists each kind of a COMPLETED group that carries only the
// sender's facts, with the event content that makes the group carry it.
// The recorder merges each of these kinds with no owner check, except
// run.stage and run.changes on a cell run.
var senderKinds = []struct {
	kind string
	add  func(evt *mitev1.BuildEvent)
}{
	{kindRunStage, func(evt *mitev1.BuildEvent) {
		evt.Stages = []*mitev1.BuildStage{{StageId: "s1", Name: "build", NodeType: "stage", Status: "FAILURE"}}
	}},
	{kindRunChanges, func(evt *mitev1.BuildEvent) {
		evt.Changes = []*mitev1.BuildChange{{CommitId: "abc123"}}
	}},
	{kindRunTests, func(evt *mitev1.BuildEvent) {
		evt.TestTotal, evt.TestFailed = 5, 2
	}},
	{kindRunQuality, func(evt *mitev1.BuildEvent) {
		evt.CoverageReports = []*mitev1.BuildCoverageReport{{Id: "jacoco"}}
	}},
	{kindRunArtifacts, func(evt *mitev1.BuildEvent) {
		evt.Artifacts = []*mitev1.BuildArtifact{{Path: "a.txt", BlobKey: "artifacts/c/n/cell2/a.txt"}}
	}},
}

// fullCompletedGroup is the kind order of the group that
// fullCompletedBuildEvent produces.
var fullCompletedGroup = []string{
	kindRunStage, kindRunChanges, kindRunTests, kindRunQuality, kindRunArtifacts, kindRunLog, kindRunCompleted,
}

// fullCompletedBuildEvent returns lossyCompletedBuildEvent with one item
// of each sender kind.
func fullCompletedBuildEvent(runID string) *mitev1.BuildEvent {
	evt := lossyCompletedBuildEvent(runID)
	for _, sk := range senderKinds {
		sk.add(evt)
	}
	return evt
}

// queuedKinds returns the kind of each envelope that OnBuildEvent queued
// on the build worker of cell2, in queue order.
func queuedKinds(t *testing.T, h *BusHandler) []string {
	t.Helper()
	envs := queuedEnvelopes(t, h)
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.Kind)
	}
	return out
}

// TestOnBuildEventCompletedFromANonOwnerWithholdsEachSenderKind proves
// that a COMPLETED event publishes no envelope of a sender kind when its
// ledger answer names another cell instance as the owner. The recorder
// checks no owner for most of these kinds, so each would replace or merge
// the owner's record. The run.completed envelope still goes out, so the recorder
// records the foreign completion. The owner's event of the same content
// publishes the kind, so the event really carries it.
func TestOnBuildEventCompletedFromANonOwnerWithholdsEachSenderKind(t *testing.T) {
	foreign := []struct {
		name   string
		answer ownerLookupResponse
	}{
		{name: "foreign cell", answer: ownerAnswer()},
		{
			name:   "stale instance of the same cell",
			answer: ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-old"},
		},
	}
	for _, sk := range senderKinds {
		t.Run(sk.kind, func(t *testing.T) {
			event := func(runID string) *mitev1.BuildEvent {
				evt := lossyCompletedBuildEvent(runID)
				sk.add(evt)
				return evt
			}

			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, cell2Owner())
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			h.logFinalizeWait = time.Hour
			if err := h.OnBuildEvent("cell2", "ns1", 0, event(runID)); err != nil {
				t.Fatalf("OnBuildEvent from the owner: %v", err)
			}
			want := []string{sk.kind, kindRunLog, kindRunCompleted}
			if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("owner group = %v, want %v", got, want)
			}

			for _, fc := range foreign {
				t.Run(fc.name, func(t *testing.T) {
					owner := newFakeOwnerRequester()
					runID := newTestRunID()
					owner.setAnswer(runID, fc.answer)
					h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})

					if err := h.OnBuildEvent("cell2", "ns1", 0, event(runID)); err != nil {
						t.Fatalf("OnBuildEvent: %v", err)
					}
					want := []string{kindRunCompleted}
					if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(want) {
						t.Fatalf("non-owner group = %v, want %v: the gateway holds no log fact", got, want)
					}
				})
			}
		})
	}
}

// TestOnBuildEventCompletedFromAStaleOwnerWithholdsEverySenderKind proves
// the main case of a stale cell for a group that carries every sender
// kind. The old owner's match is cached from its chunks, and a takeover
// moved the run with no grant through this gateway. The fresh ledger
// answer names the new owner, so the group keeps only run.completed and
// the gateway's finalized fact.
func TestOnBuildEventCompletedFromAStaleOwnerWithholdsEverySenderKind(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, _ := newCompletedOwnerTestHandler(t, owner, sender)
	storeFinalizedRun(t, h, sender, runID)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	if err := h.OnBuildEvent("cell2", "ns1", 0, fullCompletedBuildEvent(runID)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	want := []string{kindRunLog, kindRunCompleted}
	if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stale owner group = %v, want %v", got, want)
	}
	env, _ := queuedRunLog(t, h)
	if len(env.Payload) != 1 || env.Payload["finalized"] != true {
		t.Fatalf("run.log payload = %v, want only the gateway's finalized fact", env.Payload)
	}
}

// TestOnBuildEventCompletedKeepsTheSenderKindsWithoutAnotherOwner proves
// that every answer which names no other owner instance keeps the whole
// group. The cases are the legitimate senders of a COMPLETED event that a
// cached entry can mistake for a non-owner: a run that a resume or a
// takeover moved to this cell, a Jenkins restart in the same pod (the
// boot reconcile sends the event again under the same pod identity), and
// a ledger outage. A run with no ledger row, an owner with no instance,
// and a gateway with no log store also keep the group.
func TestOnBuildEventCompletedKeepsTheSenderKindsWithoutAnotherOwner(t *testing.T) {
	cases := []struct {
		name      string
		answer    *ownerLookupResponse
		lookupErr error
		prepare   func(h *BusHandler, runID string)
	}{
		{
			name:   "owner",
			answer: ptrOwnerAnswer(cell2Owner()),
			prepare: func(h *BusHandler, runID string) {
				h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")
			},
		},
		{
			name:   "moved run with a refusal cached before the move",
			answer: ptrOwnerAnswer(cell2Owner()),
			prepare: func(h *BusHandler, runID string) {
				h.ownerCache.set(runID, ownerCacheEntry{
					found:   true,
					tuple:   ownerTuple{cluster: "core", namespace: "ns1", cell: "cell1", instance: "inst1"},
					expires: time.Now().Add(ownerRefuseTTL),
				})
			},
		},
		{
			name:   "moved run with the old owner's match cached",
			answer: ptrOwnerAnswer(cell2Owner()),
			prepare: func(h *BusHandler, runID string) {
				h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
			},
		},
		{
			name:   "jenkins restart in the same pod",
			answer: ptrOwnerAnswer(cell2Owner()),
			prepare: func(h *BusHandler, _ string) {
				h.RecordCellInstance("cell2", "ns1", "inst2")
			},
		},
		{
			name:      "owner lookup failed",
			lookupErr: errors.New("deadline exceeded"),
		},
		{
			name:      "owner lookup failed with a refusal cached",
			lookupErr: errors.New("deadline exceeded"),
			prepare: func(h *BusHandler, runID string) {
				h.ownerCache.set(runID, ownerCacheEntry{
					found:   true,
					tuple:   ownerTuple{cluster: "core", namespace: "ns1", cell: "cell1", instance: "inst1"},
					expires: time.Now().Add(ownerRefuseTTL),
				})
			},
		},
		{name: "no ledger row"},
		{
			name:   "owner with no instance",
			answer: &ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2"},
		},
		{
			name: "no log store",
			prepare: func(h *BusHandler, _ string) {
				h.logStore = nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			owner.err = tc.lookupErr
			runID := newTestRunID()
			if tc.answer != nil {
				owner.setAnswer(runID, *tc.answer)
			}
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			h.logFinalizeWait = time.Hour
			if tc.prepare != nil {
				tc.prepare(h, runID)
			}

			if err := h.OnBuildEvent("cell2", "ns1", 0, fullCompletedBuildEvent(runID)); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(fullCompletedGroup) {
				t.Fatalf("group = %v, want the whole group %v", got, fullCompletedGroup)
			}
			assertQueuedSenderSummary(t, h)
		})
	}
}

// TestWithholdSenderFactsKeepsOnlyTheCompletionAndTheGatewayLogFacts
// proves that the non-owner cut keeps run.completed and the gateway's
// log facts alone. A kind that the group does not carry today is dropped
// too, so a kind added to the group later reaches no record of a run
// that its sender does not own.
func TestWithholdSenderFactsKeepsOnlyTheCompletionAndTheGatewayLogFacts(t *testing.T) {
	h := newLogTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	runID := newTestRunID()
	st := h.getOrCreateRunState(runID, "ns1", "cell1", "tenant-a/build")
	st.mu.Lock()
	st.finalDurable = true
	st.mu.Unlock()

	kps := []kindPayload{
		{kindRunStage, map[string]interface{}{"stage_id": "s1"}},
		{kindRunChanges, map[string]interface{}{"changes": []changeEntry{}}},
		{kindRunTests, map[string]interface{}{"total": 1}},
		{kindRunQuality, map[string]interface{}{"coverage": []coverageReportEntry{}}},
		{kindRunArtifacts, map[string]interface{}{"path": "a.txt"}},
		{"run.future", map[string]interface{}{"k": "v"}},
		{kindRunLog, map[string]interface{}{"chunk_count": 9, "incomplete": true}},
		{kindRunCompleted, map[string]interface{}{"result": "SUCCESS"}},
	}
	out := h.withholdSenderFacts(runID, kps)
	kinds := make([]string, 0, len(out))
	for _, kp := range out {
		kinds = append(kinds, kp.kind)
	}
	if want := []string{kindRunLog, kindRunCompleted}; fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("kept kinds = %v, want %v", kinds, want)
	}
	if len(out[0].payload) != 1 || out[0].payload["finalized"] != true {
		t.Fatalf("run.log payload = %v, want only the gateway's finalized fact", out[0].payload)
	}
	if out[1].payload["result"] != "SUCCESS" {
		t.Fatalf("run.completed payload = %v, want it unchanged", out[1].payload)
	}
}

func ptrOwnerAnswer(a ownerLookupResponse) *ownerLookupResponse { return &a }
