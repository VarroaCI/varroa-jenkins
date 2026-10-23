package mite

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// envelopePayload decodes the payload of one published run.state envelope.
func envelopePayload(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Payload map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.Payload
}

// twoChunkRunStateChunk builds chunk seq of the two-chunk build.xml file at the
// given push generation.
func twoChunkRunStateChunk(t *testing.T, runID string, gen, seq int64, part []byte, objectSize int64) *mitev1.RunStateChunk {
	t.Helper()
	gz, err := gzipBytes(part)
	if err != nil {
		t.Fatalf("gzip chunk: %v", err)
	}
	return &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: "jobs/team/builds/1/build.xml",
		StateGen: gen, Seq: seq, Total: 2, GzipContent: gz, RawSize: int64(len(part)),
		ObjectSize: objectSize, PusherEpoch: oneChunkEpoch,
	}
}

// TestOnRunStateChunk_EnvelopePublishedBeforeStoredAck proves a stored mesh
// file publishes its envelope before the STORED ack leaves the gateway.
func TestOnRunStateChunk_EnvelopePublishedBeforeStoredAck(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &acksBeforePublishPublisher{sender: sender}
	h.publisher = pub

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/team/builds/1/build.xml", 1, []byte("hello")))

	if pub.published != 1 {
		t.Fatalf("published %d envelopes, want 1", pub.published)
	}
	if len(pub.acksAt) != 1 || pub.acksAt[0] != 0 {
		t.Fatalf("acks sent before the publish = %v, want [0]", pub.acksAt)
	}
	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusStored {
		t.Fatalf("acks = %+v, want one STORED ack", acks)
	}
}

// TestOnRunStateChunk_PublishFailureAnswersBusy proves a failed envelope
// publish answers BUSY and records nothing, so the retry is not read as a
// redelivery.
func TestOnRunStateChunk_PublishFailureAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats unavailable")}
	h.publisher = pub

	const relpath = "jobs/team/builds/1/build.xml"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	h.OnRunStateChunk("cell1", "ns1", chunk)

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusBusy ||
		acks[0].Reason != stateReasonBudgetFull || acks[0].PauseMs <= 0 {
		t.Fatalf("acks = %+v, want one BUSY gateway-budget-full ack with a pause", acks)
	}
	h.stateMu.Lock()
	e := h.stateRuns[runID]
	recorded := e != nil && (e.storedSizes[relpath] != 0 || e.completed[relpath] != (storedPush{}) || e.storedTotal != 0)
	h.stateMu.Unlock()
	if recorded {
		t.Fatalf("the failed push was recorded, want it left for the retry")
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want none", got)
	}

	h.OnRunStateChunk("cell1", "ns1", chunk)
	acks = runStateAcksFrom(sender)
	if last := acks[len(acks)-1]; last.Status != stateAckStatusStored {
		t.Fatalf("retry ack = %+v, want STORED", last)
	}
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes after the retry, want 1", got)
	}
}

// TestOnRunStateChunk_PublishFailureOnMultiChunkFileRecovers proves a failed
// publish of a multi-chunk file recovers through a full re-push. The replayed
// final chunk alone answers REJECTED incomplete, and the next generation
// stores the file.
func TestOnRunStateChunk_PublishFailureOnMultiChunkFileRecovers(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats unavailable")}
	h.publisher = pub

	part0, part1 := []byte("first half "), []byte("second half")
	whole := append(append([]byte(nil), part0...), part1...)
	size := int64(len(whole))

	h.OnRunStateChunk("cell1", "ns1", twoChunkRunStateChunk(t, runID, 1, 0, part0, size))
	last := twoChunkRunStateChunk(t, runID, 1, 1, part1, size)
	h.OnRunStateChunk("cell1", "ns1", last)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("final chunk ack = %+v, want BUSY after the failed publish", ack)
	}

	h.OnRunStateChunk("cell1", "ns1", last)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonIncomplete {
		t.Fatalf("replayed final chunk ack = %+v, want REJECTED incomplete", ack)
	}

	h.OnRunStateChunk("cell1", "ns1", twoChunkRunStateChunk(t, runID, 2, 0, part0, size))
	h.OnRunStateChunk("cell1", "ns1", twoChunkRunStateChunk(t, runID, 2, 1, part1, size))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusStored {
		t.Fatalf("re-push final ack = %+v, want STORED", ack)
	}
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	if gen := envelopePayload(t, pub.published[0].data)["state_gen"]; gen != float64(2) {
		t.Fatalf("envelope state_gen = %v, want 2", gen)
	}
}

// TestOnRunStateChunk_StateCapEnvelopeBeforeRefusal proves a state-cap
// refusal publishes its envelope before the REJECTED ack, and the ack holds
// when that publish fails.
func TestOnRunStateChunk_StateCapEnvelopeBeforeRefusal(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &acksBeforePublishPublisher{sender: sender}
	h.publisher = pub

	capped := oneChunk(runID, "jobs/team/builds/1/build.xml", 1, []byte("x"))
	capped.ObjectSize = perRunStateMaxBytes + 1
	h.OnRunStateChunk("cell1", "ns1", capped)

	if pub.published != 1 || len(pub.acksAt) != 1 || pub.acksAt[0] != 0 {
		t.Fatalf("published %d, acks before publish %v, want one envelope before any ack", pub.published, pub.acksAt)
	}
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonStateCap {
		t.Fatalf("ack = %+v, want REJECTED state-cap", ack)
	}

	sender2 := &recordingSender{}
	h2 := newRunStateTestHandler(t, owner, sender2)
	h2.publisher = &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats unavailable")}
	h2.OnRunStateChunk("cell1", "ns1", capped)
	if ack := lastRunStateAck(t, sender2); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonStateCap {
		t.Fatalf("ack after a failed publish = %+v, want REJECTED state-cap", ack)
	}
}

// TestOnRunStateChunk_UnresolvedTenantPastGraceAnswersForeignRun proves an
// unresolved tenant answers BUSY inside the grace and REJECTED foreign-run
// after it. The refusal publishes no envelope and records no push.
func TestOnRunStateChunk_UnresolvedTenantPastGraceAnswersForeignRun(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{}
	h.publisher = pub
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	const relpath = "jobs/team/builds/1/build.xml"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 1, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy || ack.Reason != stateReasonTenantUnresolved {
		t.Fatalf("first ack = %+v, want BUSY tenant-unresolved", ack)
	}

	h.stateMu.Lock()
	h.tenantGrace[tenantGraceKey{runID: runID}] = time.Now().Add(-(runStateTenantGrace + time.Second))
	h.stateMu.Unlock()

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 2, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonForeignRun {
		t.Fatalf("ack past the grace = %+v, want REJECTED foreign-run", ack)
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want none", got)
	}
	h.stateMu.Lock()
	recorded := h.stateRuns[runID].completed[relpath] != (storedPush{})
	h.stateMu.Unlock()
	if recorded {
		t.Fatal("the refused push was recorded")
	}
}

// TestOnRunStateChunk_UnresolvedTenantHealsInsideGrace proves a tenant that
// resolves on a retry inside the grace stores the file, and the grace clock
// clears.
func TestOnRunStateChunk_UnresolvedTenantHealsInsideGrace(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{}
	h.publisher = pub
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	const relpath = "jobs/team/builds/1/build.xml"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	h.OnRunStateChunk("cell1", "ns1", chunk)
	h.stateMu.Lock()
	_, started := h.tenantGrace[tenantGraceKey{runID: runID}]
	h.stateMu.Unlock()
	if !started {
		t.Fatal("the first unresolved observation did not start the grace clock")
	}

	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{"team/pipeline": {tenant: "tenant"}}})
	h.OnRunStateChunk("cell1", "ns1", chunk)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusStored {
		t.Fatalf("retry ack = %+v, want STORED", ack)
	}
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	h.stateMu.Lock()
	_, present := h.tenantGrace[tenantGraceKey{runID: runID}]
	cleared := !present
	h.stateMu.Unlock()
	if !cleared {
		t.Fatal("a resolved publish did not clear the grace clock")
	}
}

// TestOnRunStateChunk_PublishFailureNeverStartsTenantGrace proves a failed
// publish keeps BUSY with no grace clock. A transient cause never reaches
// the foreign-run refusal.
func TestOnRunStateChunk_PublishFailureNeverStartsTenantGrace(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats unavailable")}

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/team/builds/1/build.xml", 1, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy || ack.Reason != stateReasonBudgetFull {
		t.Fatalf("ack = %+v, want BUSY gateway-budget-full", ack)
	}
	h.stateMu.Lock()
	_, started := h.tenantGrace[tenantGraceKey{runID: runID}]
	h.stateMu.Unlock()
	if started {
		t.Fatal("a publish failure started the tenant grace clock")
	}
}

// TestBufferRunStateChunk_NewerEpochKeepsUnresolvedClock proves a newer
// pushing process keeps the run's tenant grace clock on the mesh path. A
// run whose tenant stayed unresolved past the grace is refused on the new
// process's first file.
func TestBufferRunStateChunk_NewerEpochKeepsUnresolvedClock(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	const relpath = "jobs/team/builds/1/build.xml"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 1, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("first ack = %+v, want BUSY", ack)
	}
	h.stateMu.Lock()
	since := h.tenantGrace[tenantGraceKey{runID: runID}]
	h.stateMu.Unlock()
	if since.IsZero() {
		t.Fatal("the first unresolved answer did not start the grace clock")
	}

	newer := oneChunk(runID, relpath, 1, []byte("hello"))
	newer.PusherEpoch = oneChunkEpoch + 1
	h.OnRunStateChunk("cell1", "ns1", newer)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("newer-epoch ack inside the grace = %+v, want BUSY", ack)
	}
	h.stateMu.Lock()
	epoch, kept := h.stateRuns[runID].newestEpoch, h.tenantGrace[tenantGraceKey{runID: runID}]
	h.tenantGrace[tenantGraceKey{runID: runID}] = time.Now().Add(-(runStateTenantGrace + time.Second))
	h.stateMu.Unlock()
	if epoch != oneChunkEpoch+1 {
		t.Fatalf("newest epoch = %d, want %d", epoch, oneChunkEpoch+1)
	}
	if !kept.Equal(since) {
		t.Fatalf("grace clock after a newer epoch = %v, want %v", kept, since)
	}

	newest := oneChunk(runID, relpath, 1, []byte("hello"))
	newest.PusherEpoch = oneChunkEpoch + 2
	h.OnRunStateChunk("cell1", "ns1", newest)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected {
		t.Fatalf("newer-epoch ack past the grace = %+v, want REJECTED", ack)
	}
}

// TestDropCellStateEntries_KeepsMeshTenantGraceClock proves a mite
// reconnect drops the run's entry but keeps its grace clock. A reconnect
// must not restart the grace.
func TestDropCellStateEntries_KeepsMeshTenantGraceClock(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{}
	h.publisher = pub
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	const relpath = "jobs/team/builds/1/build.xml"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 1, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("first ack = %+v, want BUSY", ack)
	}

	h.dropCellStateEntries("ns1", "cell1")
	h.stateMu.Lock()
	_, entryKept := h.stateRuns[runID]
	_, clockKept := h.tenantGrace[tenantGraceKey{runID: runID}]
	h.stateMu.Unlock()
	if entryKept {
		t.Fatal("the reconnect kept the run-state entry")
	}
	if !clockKept {
		t.Fatal("the reconnect dropped the mesh grace clock")
	}

	h.stateMu.Lock()
	h.tenantGrace[tenantGraceKey{runID: runID}] = time.Now().Add(-(runStateTenantGrace + time.Second))
	h.stateMu.Unlock()

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 2, []byte("hello")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected {
		t.Fatalf("ack after the reconnect past the grace = %+v, want REJECTED", ack)
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want none", got)
	}
}

// TestOnRunStateChunk_TenantResolvedAfterReconnectClearsGrace proves a
// tenant that resolves after a reconnect stores the file and clears the
// grace clock.
func TestOnRunStateChunk_TenantResolvedAfterReconnectClearsGrace(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	pub := &fakePublisher{}
	h.publisher = pub
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	const relpath = "jobs/team/builds/1/build.xml"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	h.OnRunStateChunk("cell1", "ns1", chunk)
	h.dropCellStateEntries("ns1", "cell1")

	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{"team/pipeline": {tenant: "tenant"}}})
	h.OnRunStateChunk("cell1", "ns1", chunk)
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusStored {
		t.Fatalf("ack after the tenant resolved = %+v, want STORED", ack)
	}
	h.stateMu.Lock()
	_, present := h.tenantGrace[tenantGraceKey{runID: runID}]
	h.stateMu.Unlock()
	if present {
		t.Fatal("a resolved publish after a reconnect kept the grace clock")
	}
}

// TestDropStateEntry_ClearsMeshTenantGraceClock proves a terminal drop
// clears the run's mesh clock and leaves the restart copy's clock.
func TestDropStateEntry_ClearsMeshTenantGraceClock(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/team/builds/1/build.xml", 1, []byte("hello")))
	restartKey := tenantGraceKey{runID: runID, restart: true}
	h.stateMu.Lock()
	h.tenantGrace[restartKey] = time.Now()
	h.stateMu.Unlock()

	h.dropStateEntry(runID)
	h.stateMu.Lock()
	_, meshKept := h.tenantGrace[tenantGraceKey{runID: runID}]
	_, restartKept := h.tenantGrace[restartKey]
	h.stateMu.Unlock()
	if meshKept {
		t.Fatal("the terminal drop kept the mesh grace clock")
	}
	if !restartKept {
		t.Fatal("the terminal drop cleared the restart copy's grace clock")
	}
}

// TestUnresolvedTenantGrace_SweepsOldClocks proves a clock older than the
// retention bound is swept on the next grace answer, and a fresh clock is
// kept.
func TestUnresolvedTenantGrace_SweepsOldClocks(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	newRun := newTestRunID()
	owner.setAnswer(newRun, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})

	now := time.Now()
	oldKey := tenantGraceKey{runID: "old"}
	freshKey := tenantGraceKey{runID: "fresh"}
	h.stateMu.Lock()
	h.tenantGrace = map[tenantGraceKey]time.Time{}
	h.tenantGrace[oldKey] = now.Add(-(tenantGraceRetain + time.Second))
	h.tenantGrace[freshKey] = now.Add(-time.Second)
	h.stateMu.Unlock()

	h.OnRunStateChunk("cell1", "ns1", oneChunk(newRun, "jobs/team/builds/1/build.xml", 1, []byte("hello")))

	h.stateMu.Lock()
	_, oldKept := h.tenantGrace[oldKey]
	_, freshKept := h.tenantGrace[freshKey]
	_, newKept := h.tenantGrace[tenantGraceKey{runID: newRun}]
	h.stateMu.Unlock()
	if oldKept {
		t.Fatal("the sweep kept a clock older than the retention bound")
	}
	if !freshKept {
		t.Fatal("the sweep dropped a fresh clock")
	}
	if !newKept {
		t.Fatal("the new run's clock was not recorded")
	}
}
