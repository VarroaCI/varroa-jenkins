package mite

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// acksBeforePublishPublisher records how many RunStateAck messages the
// sender had seen when the run.state envelope was published.
type acksBeforePublishPublisher struct {
	sender    *recordingSender
	published int
	acksAt    []int
}

func (p *acksBeforePublishPublisher) PublishJetStream(string, []byte) (*nats.PubAck, error) {
	p.published++
	p.acksAt = append(p.acksAt, len(runStateAcksFrom(p.sender)))
	return &nats.PubAck{}, nil
}

func (p *acksBeforePublishPublisher) PublishJetStreamAsync(string, []byte) (nats.PubAckFuture, error) {
	return nil, nil
}

// twoChunkRestartChunk builds chunk seq of a two-chunk restart-copy file.
func twoChunkRestartChunk(t *testing.T, runID, relpath string, seq int64, part []byte) *mitev1.RunStateChunk {
	t.Helper()
	gz, err := gzipBytes(part)
	if err != nil {
		t.Fatalf("gzip chunk: %v", err)
	}
	return &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 7, Relpath: relpath, StateGen: 1,
		Seq: seq, Total: 2, GzipContent: gz, RawSize: int64(len(part)), ObjectSize: 12,
		PusherEpoch: 100, RestartSource: true, CopyFiles: 1, CopyBytes: 12,
	}
}

func TestRestartSourceAckCarriesSeq(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "workflow-completed/2.xml"

	h.OnRunStateChunk("cell1", "ns1", twoChunkRestartChunk(t, runID, relpath, 0, []byte("first-")))
	h.OnRunStateChunk("cell1", "ns1", twoChunkRestartChunk(t, runID, relpath, 1, []byte("second")))

	acks := runStateAcksFrom(sender)
	if len(acks) != 2 {
		t.Fatalf("got %d acks, want one per chunk", len(acks))
	}
	for i, ack := range acks {
		if ack.Seq != int64(i) {
			t.Fatalf("ack %d carries seq %d, want %d", i, ack.Seq, i)
		}
		if !ack.RestartSource {
			t.Fatalf("ack %d lost the restart-copy flag", i)
		}
	}
}

func TestRestartSourceEnvelopePublishedBeforeStoredAck(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	pub := &acksBeforePublishPublisher{sender: sender}
	h.publisher = pub
	relpath := restartTestBuildPath + "workflow-completed/3.xml"

	h.OnRunStateChunk("cell1", "ns1", twoChunkRestartChunk(t, runID, relpath, 0, []byte("first-")))
	h.OnRunStateChunk("cell1", "ns1", twoChunkRestartChunk(t, runID, relpath, 1, []byte("second")))

	if pub.published != 1 {
		t.Fatalf("published %d envelopes, want 1 for the stored file", pub.published)
	}
	// Only the first chunk's ack (not yet STORED) may precede the publish.
	// The STORED ack for the final chunk must come after it.
	if pub.acksAt[0] != 1 {
		t.Fatalf("envelope published after %d acks, want 1: the STORED ack went out first", pub.acksAt[0])
	}
}

// storedRelpathsOf reports the relpaths one restart copy holds as stored.
func storedRelpathsOf(t *testing.T, h *BusHandler, runID string) (map[string]int64, bool) {
	t.Helper()
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	e, ok := h.restartRuns[runID]
	if !ok {
		return nil, false
	}
	return e.stored, true
}

func TestRestartSourcePublishFailureAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats down")}
	h.publisher = pub
	relpath := restartTestBuildPath + "workflow-completed/2.xml"
	data := []byte("<flow/>")

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, int64(len(data)), data))

	ack := lastRunStateAck(t, sender)
	if ack.Status != stateAckStatusBusy {
		t.Fatalf("ack status %v, want BUSY", ack.Status)
	}
	if ack.Reason != stateReasonBudgetFull {
		t.Fatalf("ack reason %q, want %q", ack.Reason, stateReasonBudgetFull)
	}
	if ack.PauseMs <= 0 {
		t.Fatalf("ack pause %d, want a positive pause", ack.PauseMs)
	}
	stored, ok := storedRelpathsOf(t, h, runID)
	if !ok {
		t.Fatal("copy entry was dropped after a failed publish")
	}
	if _, has := stored[relpath]; has {
		t.Fatal("failed file entered the stored set")
	}
	if len(pub.published) != 0 {
		t.Fatalf("published %d envelopes after a failed publish, want 0", len(pub.published))
	}

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, int64(len(data)), data))

	if got := lastRunStateAck(t, sender); got.Status != stateAckStatusStored {
		t.Fatalf("retry ack status %v, want STORED", got.Status)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d envelopes after retry, want 1", len(pub.published))
	}
	stored, _ = storedRelpathsOf(t, h, runID)
	if _, has := stored[relpath]; !has {
		t.Fatal("retried file is not in the stored set")
	}
}

func TestRestartSourcePublishFailureOnLastFileKeepsEntry(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats down")}
	h.publisher = pub
	relpath := restartTestBuildPath + "workflow-completed/2.xml"
	data := []byte("<flow/>")

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, int64(len(data)), data))

	if got := lastRunStateAck(t, sender); got.Status != stateAckStatusBusy {
		t.Fatalf("ack status %v, want BUSY", got.Status)
	}
	if _, ok := storedRelpathsOf(t, h, runID); !ok {
		t.Fatal("copy entry was deleted after a failed publish of its last file")
	}

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, int64(len(data)), data))

	if got := lastRunStateAck(t, sender); got.Status != stateAckStatusStored {
		t.Fatalf("retry ack status %v, want STORED", got.Status)
	}
	if _, ok := storedRelpathsOf(t, h, runID); ok {
		t.Fatal("copy entry survived its last stored file")
	}
	if len(pub.published) != 1 {
		t.Fatalf("published %d envelopes, want 1", len(pub.published))
	}
}

func TestRestartSourceUnresolvedTenantAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	pub := &fakePublisher{}
	h.publisher = pub
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}}
	relpath := restartTestBuildPath + "workflow-completed/2.xml"
	data := []byte("<flow/>")

	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 2, int64(len(data)), data))

	if got := lastRunStateAck(t, sender); got.Status != stateAckStatusBusy || got.Reason != stateReasonTenantUnresolved {
		t.Fatalf("ack = %+v, want BUSY tenant-unresolved", got)
	}
	if len(pub.published) != 0 {
		t.Fatalf("published %d envelopes for an unresolved tenant, want 0", len(pub.published))
	}
	stored, _ := storedRelpathsOf(t, h, runID)
	if _, has := stored[relpath]; has {
		t.Fatal("file with an unresolved tenant entered the stored set")
	}
}
