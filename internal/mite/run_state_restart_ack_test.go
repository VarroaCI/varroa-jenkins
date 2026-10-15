package mite

import (
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
