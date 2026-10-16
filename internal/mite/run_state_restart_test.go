package mite

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// restartTestBuildPath is the relpath prefix of build 7 of the job the
// run-state fixtures name, team/pipeline.
const restartTestBuildPath = "jobs/team/jobs/pipeline/builds/7/"

// newRestartTestOwner answers a terminal owner for runID, which is what a
// finalized run's copy meets.
func newRestartTestOwner(runID string) *fakeOwnerRequester {
	owner := newFakeOwnerRequester()
	owner.setAnswer(runID, ownerLookupResponse{
		Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1", Status: "COMPLETED",
	})
	return owner
}

// restartOneChunk builds a single-chunk restart-copy chunk of build 7.
func restartOneChunk(runID, relpath string, epoch, copyFiles, copyBytes int64, data []byte) *mitev1.RunStateChunk {
	gz, err := gzipBytes(data)
	if err != nil {
		panic(err)
	}
	return &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: relpath,
		StateGen: 1, Seq: 0, Total: 1, GzipContent: gz, RawSize: int64(len(data)),
		ObjectSize: int64(len(data)), PusherEpoch: epoch,
		RestartSource: true, CopyFiles: copyFiles, CopyBytes: copyBytes,
	}
}

func lastRunStateAck(t *testing.T, sender *recordingSender) *mitev1.RunStateAck {
	t.Helper()
	acks := runStateAcksFrom(sender)
	if len(acks) == 0 {
		t.Fatal("expected an ack, got none")
	}
	return acks[len(acks)-1]
}

func restartObjectExists(t *testing.T, h *BusHandler, runID, relpath string) bool {
	t.Helper()
	_, err := h.logStore.NewRangeReader(context.Background(), blob.RestartSourceKey(runID, relpath), 0, -1)
	if errors.Is(err, blob.ErrObjectNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read restart copy object: %v", err)
	}
	return true
}

func readRestartObject(t *testing.T, h *BusHandler, runID, relpath string) []byte {
	t.Helper()
	r, err := h.logStore.NewRangeReader(context.Background(), blob.RestartSourceKey(runID, relpath), 0, -1)
	if err != nil {
		t.Fatalf("read restart copy object: %v", err)
	}
	defer func() { _ = r.Close() }()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read restart copy body: %v", err)
	}
	return body
}

func TestOnRunStateChunk_RestartSourceStoresUnderRestartKey(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "build.xml"
	data := []byte("<flow-build><result>SUCCESS</result></flow-build>")

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, int64(len(data)), data))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusStored || !ack.RestartSource {
		t.Fatalf("expected a STORED restart-copy ack, got %+v", ack)
	}
	if got := readRestartObject(t, h, runID, relpath); string(got) != string(data) {
		t.Fatalf("stored body = %q, want %q", got, data)
	}
	if want := "restartsource/" + runID + "/jobs/team/jobs/pipeline/builds/7/build.xml"; blob.RestartSourceKey(runID, relpath) != want {
		t.Fatalf("restart key = %q, want %q", blob.RestartSourceKey(runID, relpath), want)
	}
}

func TestOnRunStateChunk_RestartSourceMultiChunkFile(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "workflow-completed/1.xml"
	gz0, _ := gzipBytes([]byte("first-"))
	gz1, _ := gzipBytes([]byte("second"))

	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: relpath, StateGen: 1,
		Seq: 0, Total: 2, GzipContent: gz0, RawSize: 6, ObjectSize: 12, PusherEpoch: 100,
		RestartSource: true, CopyFiles: 1, CopyBytes: 12,
	})
	if restartObjectExists(t, h, runID, relpath) {
		t.Fatal("a file stored before its last chunk arrived")
	}
	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: relpath, StateGen: 1,
		Seq: 1, Total: 2, GzipContent: gz1, RawSize: 6, ObjectSize: 12, PusherEpoch: 100,
		RestartSource: true, CopyFiles: 1, CopyBytes: 12,
	})
	if got := readRestartObject(t, h, runID, relpath); string(got) != "first-second" {
		t.Fatalf("stored body = %q, want %q", got, "first-second")
	}
}

func TestOnRunStateChunk_RestartSourceRelpathOutsideBuildIsMalformed(t *testing.T) {
	for _, relpath := range []string{
		"jobs/team/jobs/pipeline/builds/8/build.xml",
		"nodes/x/config.xml",
		restartTestBuildPath + "program.dat",
		restartTestBuildPath + "workflow-completed/",
		restartTestBuildPath + "libs/",
	} {
		sender := &recordingSender{}
		runID := newTestRunID()
		h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
		data := []byte("x")
		h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, 1, data))
		ack := lastRunStateAck(t, sender)
		if ack.Status != stateAckStatusRejected || ack.Reason != stateReasonMalformed {
			t.Errorf("relpath %q: expected REJECTED malformed, got %+v", relpath, ack)
		}
		if restartObjectExists(t, h, runID, relpath) {
			t.Errorf("relpath %q: a malformed copy stored an object", relpath)
		}
	}
}

func TestOnRunStateChunk_RestartSourceForeignRunRefused(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	owner := newFakeOwnerRequester()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "other", Instance: "inst-other", Status: "COMPLETED"})
	h := newRunStateTestHandler(t, owner, sender)
	relpath := restartTestBuildPath + "build.xml"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, 1, []byte("x")))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusRejected || ack.Reason != stateReasonForeignRun || !ack.RestartSource {
		t.Fatalf("expected a foreign-run restart-copy refusal, got %+v", ack)
	}
	if restartObjectExists(t, h, runID, relpath) {
		t.Fatal("a foreign copy stored an object")
	}
}

func TestOnRunStateChunk_RestartSourceOverFileCapRefused(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	stored := restartTestBuildPath + "build.xml"
	partial := restartTestBuildPath + "libs/a.jar"
	extra := restartTestBuildPath + "libs/b.jar"

	// The copy declares two files. One is stored, one is still arriving,
	// so a third file is past the declared count.
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, stored, 100, 2, 8, []byte("aaaa")))
	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: partial, StateGen: 1,
		Seq: 0, Total: 2, GzipContent: mustGzip(t, []byte("bb")), RawSize: 2, ObjectSize: 4, PusherEpoch: 100,
		RestartSource: true, CopyFiles: 2, CopyBytes: 8,
	})
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, extra, 100, 2, 8, []byte("cccc")))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusRejected || ack.Reason != stateReasonStateCap {
		t.Fatalf("expected a state-cap refusal for a file past the declared count, got %+v", ack)
	}
	if restartObjectExists(t, h, runID, extra) {
		t.Fatal("a file past the declared count was stored")
	}
}

func mustGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	gz, err := gzipBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

func TestOnRunStateChunk_RestartSourceOverByteCapRefused(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "build.xml"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, 5, []byte("abcdef")))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusRejected || ack.Reason != stateReasonStateCap {
		t.Fatalf("expected a state-cap refusal past the declared bytes, got %+v", ack)
	}
	if restartObjectExists(t, h, runID, relpath) {
		t.Fatal("a file past the declared bytes was stored")
	}
}

func TestOnRunStateChunk_RestartSourceRedeliveryAnsweredStored(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "build.xml"
	chunk := restartOneChunk(runID, relpath, 100, 2, 4, []byte("abcd"))

	h.OnRunStateChunk("cell1", "ns1", chunk)
	h.OnRunStateChunk("cell1", "ns1", chunk)

	acks := runStateAcksFrom(sender)
	if len(acks) != 2 || acks[1].Status != stateAckStatusStored {
		t.Fatalf("expected a redelivery answered STORED, got %+v", acks)
	}
}

func TestOnRunStateChunk_RestartSourceNewerEpochResets(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	first := restartTestBuildPath + "build.xml"
	second := restartTestBuildPath + "libs/a.jar"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, first, 100, 2, 8, []byte("aaaa")))
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, second, 200, 2, 8, []byte("bbbb")))

	h.stateMu.Lock()
	e := h.restartRuns[runID]
	h.stateMu.Unlock()
	if e == nil {
		t.Fatal("the reset copy has no entry")
	}
	if e.epoch != 200 || len(e.stored) != 1 || e.storedTotal != 4 {
		t.Fatalf("entry after reset = epoch %d stored %d total %d, want epoch 200, one file, 4 bytes",
			e.epoch, len(e.stored), e.storedTotal)
	}
	if _, kept := e.stored[first]; kept {
		t.Fatal("the older copy's file survived the reset")
	}
}

func TestOnRunStateChunk_RestartSourceOlderEpochDropped(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	newer := restartTestBuildPath + "build.xml"
	older := restartTestBuildPath + "libs/a.jar"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, newer, 200, 2, 8, []byte("aaaa")))
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, older, 100, 2, 8, []byte("bbbb")))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusStored {
		t.Fatalf("expected a retired chunk answered STORED, got %+v", ack)
	}
	if restartObjectExists(t, h, runID, older) {
		t.Fatal("a retired epoch's chunk was stored")
	}
}

func TestOnRunStateChunk_RestartSourceEntryDroppedWhenComplete(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, restartTestBuildPath+"build.xml", 100, 1, 4, []byte("abcd")))

	h.stateMu.Lock()
	_, present := h.restartRuns[runID]
	h.stateMu.Unlock()
	if present {
		t.Fatal("a complete copy kept its entry")
	}
}

func TestDropCellStateEntries_DropsRestartSourceBuffers(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	gz, _ := gzipBytes([]byte("half"))

	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: restartTestBuildPath + "build.xml",
		StateGen: 1, Seq: 0, Total: 2, GzipContent: gz, RawSize: 4, ObjectSize: 8, PusherEpoch: 100,
		RestartSource: true, CopyFiles: 1, CopyBytes: 8,
	})
	h.stateMu.Lock()
	pending := h.stateTotalPendingBytes
	h.stateMu.Unlock()
	if pending == 0 {
		t.Fatal("the partial copy charged no pending bytes")
	}

	h.dropCellStateEntries("ns1", "cell1")

	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if _, ok := h.restartRuns[runID]; ok {
		t.Error("the cell disconnect kept the restart copy entry")
	}
	if h.stateTotalPendingBytes != 0 {
		t.Errorf("pending bytes after the disconnect = %d, want 0", h.stateTotalPendingBytes)
	}
}

// TestOnRunStateChunk_MeshTerminalDropUnchanged guards the mesh path: a
// terminal run's mesh chunk is still acknowledged and dropped, and no
// object is written.
func TestOnRunStateChunk_MeshTerminalDropUnchanged(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := "jobs/team/jobs/pipeline/builds/7/program.dat"

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 1, []byte("mesh")))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusStored || ack.RestartSource {
		t.Fatalf("expected a mesh STORED ack for a terminal run, got %+v", ack)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, oneChunkEpoch, 1, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("a terminal mesh chunk stored an object: %v", err)
	}
}

// TestOnRunStateChunk_RestartSourceRetriedSequenceRefundsItsBytes proves a
// retried sequence charges the bound once. Without the refund the retry of
// the same chunk would count twice against the gateway-wide budget.
func TestOnRunStateChunk_RestartSourceRetriedSequenceRefundsItsBytes(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "workflow-completed/big.bin"
	gz0 := mustGzip(t, []byte("first-"))
	chunk := &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: relpath, StateGen: 1,
		Seq: 0, Total: 2, GzipContent: gz0, RawSize: 6, ObjectSize: 12, PusherEpoch: 100,
		RestartSource: true, CopyFiles: 1, CopyBytes: 12,
	}

	h.OnRunStateChunk("cell1", "ns1", chunk)
	h.OnRunStateChunk("cell1", "ns1", chunk)

	h.stateMu.Lock()
	pending := h.stateTotalPendingBytes
	h.stateMu.Unlock()
	if pending != int64(len(gz0)) {
		t.Fatalf("pending bytes after a retry = %d, want one copy of %d", pending, len(gz0))
	}
}

// TestOnRunStateChunk_RestartSourceUnresolvedTenantBusyThenForeignRun proves
// an unresolved tenant answers the restart copy BUSY inside the grace and
// REJECTED foreign-run after it. The file is not stored.
func TestOnRunStateChunk_RestartSourceUnresolvedTenantBusyThenForeignRun(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})
	relpath := restartTestBuildPath + "build.xml"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, 1, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy || ack.Reason != stateReasonTenantUnresolved || !ack.RestartSource {
		t.Fatalf("first ack = %+v, want a BUSY tenant-unresolved restart-copy ack", ack)
	}

	if restartObjectExists(t, h, runID, relpath) {
		t.Fatal("a withheld copy wrote an object before the tenant resolved")
	}

	h.stateMu.Lock()
	h.restartRuns[runID].tenantUnresolvedSince = time.Now().Add(-(runStateTenantGrace + time.Second))
	h.stateMu.Unlock()

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, 1, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonForeignRun || !ack.RestartSource {
		t.Fatalf("ack past the grace = %+v, want a REJECTED foreign-run restart-copy ack", ack)
	}
	h.stateMu.Lock()
	_, stored := h.restartRuns[runID].stored[relpath]
	h.stateMu.Unlock()
	if stored {
		t.Fatal("a refused copy entered the stored set")
	}
}

// TestOnRunStateChunk_RestartSourceTenantGraceClearsOnPublish proves a
// resolved publish of a restart-copy file clears its grace clock.
func TestOnRunStateChunk_RestartSourceTenantGraceClearsOnPublish(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})
	relpath := restartTestBuildPath + "build.xml"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, 2, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("first ack = %+v, want BUSY", ack)
	}

	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{"team/pipeline": {tenant: "team"}}})
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, 2, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusStored {
		t.Fatalf("retry ack = %+v, want STORED", ack)
	}
	h.stateMu.Lock()
	cleared := h.restartRuns[runID].tenantUnresolvedSince.IsZero()
	h.stateMu.Unlock()
	if !cleared {
		t.Fatal("a resolved publish did not clear the restart grace clock")
	}
}

// TestBufferRestartSourceChunk_NewerEpochKeepsUnresolvedClock proves a
// newer process's re-post of a restart copy keeps the copy's tenant grace
// clock. A copy whose tenant stayed unresolved past the grace is refused
// on the re-post's first file.
func TestBufferRestartSourceChunk_NewerEpochKeepsUnresolvedClock(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	h.publisher = &fakePublisher{}
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})
	relpath := restartTestBuildPath + "build.xml"

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, 1, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("first ack = %+v, want BUSY", ack)
	}
	h.stateMu.Lock()
	since := h.restartRuns[runID].tenantUnresolvedSince
	h.stateMu.Unlock()
	if since.IsZero() {
		t.Fatal("the first unresolved answer did not start the grace clock")
	}

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 200, 1, 1, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("re-post ack inside the grace = %+v, want BUSY", ack)
	}
	h.stateMu.Lock()
	e := h.restartRuns[runID]
	epoch, kept := e.epoch, e.tenantUnresolvedSince
	e.tenantUnresolvedSince = time.Now().Add(-(runStateTenantGrace + time.Second))
	h.stateMu.Unlock()
	if epoch != 200 {
		t.Fatalf("copy epoch = %d, want 200", epoch)
	}
	if !kept.Equal(since) {
		t.Fatalf("grace clock after a newer epoch = %v, want %v", kept, since)
	}

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 300, 1, 1, []byte("x")))
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusRejected {
		t.Fatalf("re-post ack past the grace = %+v, want REJECTED", ack)
	}
}
