package mite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// owedMarkerCount returns the number of owed finalized markers.
func owedMarkerCount(h *BusHandler) int {
	h.logMarkerMu.Lock()
	defer h.logMarkerMu.Unlock()
	return len(h.logMarkersOwed)
}

// downPublisher returns a publisher whose every JetStream publish fails,
// as during a bus outage.
func downPublisher() *fakePublisher {
	return &fakePublisher{syncFailLeft: math.MaxInt32, syncErr: errors.New("nats unavailable")}
}

// bringPublisherUp ends the outage of pub.
func bringPublisherUp(pub *fakePublisher) {
	pub.mu.Lock()
	pub.syncFailLeft = 0
	pub.mu.Unlock()
}

// TestFinalizedMarkerOutagePastTheWaitIsSentAgain proves that a bus
// outage longer than the marker wait does not hold the final chunk. The
// final STORED ack goes out with no marker on the bus, and the marker is
// owed. A resend pass during the outage keeps it. The first pass after
// the bus returns publishes it once, and a later pass publishes nothing.
// A chunk that is not final waits for no marker: its ack follows no
// publish attempt. Both the drain and restart recovery owe the marker.
func TestFinalizedMarkerOutagePastTheWaitIsSentAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "drain", restart: false},
		{name: "restart recovery", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID, otherRun := newTestRunID(), newTestRunID()
			owner.setAnswer(runID, ownerAnswer())
			owner.setAnswer(otherRun, ownerAnswer())
			store := newTestLogStore(t)
			if tc.restart {
				storeLogWithNoMarker(t, owner, store, runID, true)
			}

			sender := &recordingSender{}
			h, _, _ := newIdleSweepTestHandler(t, owner, sender, store)
			pub := downPublisher()
			h.publisher = pub
			h.logMarkerWait = time.Millisecond

			if tc.restart {
				sendInst1Chunk(h, runID, 1, "b", true)
			} else {
				sendInst1Chunk(h, runID, 0, "a", false)
				if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 0 {
					t.Fatalf("expected chunk 0 STORED at ack_seq 0, got %+v", got)
				}
				if calls := syncPublishCalls(pub); calls != 0 {
					t.Fatalf("the ack of a chunk that is not final followed %d publish attempts, want 0", calls)
				}
				sendInst1Chunk(h, runID, 1, "b", true)
			}
			acks := waitForLogAcks(t, sender, map[bool]int{true: 1, false: 2}[tc.restart])
			if got := acks[len(acks)-1]; got.Status != logAckStatusStored || got.AckSeq != 1 {
				t.Fatalf("expected the final chunk STORED at ack_seq 1, got %+v", got)
			}
			if syncPublishCalls(pub) == 0 {
				t.Fatal("expected a marker publish attempt before the final ack")
			}
			if got := finalizedPublishes(pub, runID); got != 0 {
				t.Fatalf("published %d finalized envelopes during the outage, want 0", got)
			}
			waitForEviction(t, h, runID)
			if n := owedMarkerCount(h); n != 1 {
				t.Fatalf("owed markers = %d, want 1", n)
			}

			callsBefore := syncPublishCalls(pub)
			sendInst1Chunk(h, otherRun, 0, "x", false)
			if got := sender.logAcks(); len(got) == 0 || got[len(got)-1].RunId != otherRun {
				waitFor(t, func() bool {
					a := sender.logAcks()
					return len(a) > 0 && a[len(a)-1].RunId == otherRun
				}, 5*time.Second, "the chunk of another run got no ack")
			}
			if calls := syncPublishCalls(pub); calls != callsBefore {
				t.Fatalf("the ack of another run's chunk followed %d publish attempts, want 0", calls-callsBefore)
			}

			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 0 {
				t.Fatalf("a resend during the outage published %d finalized envelopes, want 0", got)
			}
			if n := owedMarkerCount(h); n != 1 {
				t.Fatalf("owed markers after a failed resend = %d, want 1", n)
			}

			bringPublisherUp(pub)
			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 1 {
				t.Fatalf("the resend after the outage published %d finalized envelopes, want 1", got)
			}
			env := pub.envelopeAt(pub.publishedCount() - 1)
			if env.RunID != runID || len(env.Payload) != 1 || env.JobPath != "tenant-a/build" {
				t.Fatalf("resent envelope run=%s payload=%v job_path=%q, want only finalized true and the job path",
					env.RunID, env.Payload, env.JobPath)
			}
			if n := owedMarkerCount(h); n != 0 {
				t.Fatalf("owed markers after the resend = %d, want 0", n)
			}

			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 1 {
				t.Fatalf("a second pass published %d finalized envelopes in all, want 1", got)
			}
		})
	}
}

// TestOwedFinalizedMarkerAgeBound proves that a marker owed for
// logMarkerResendAge is dropped with no publish. The recorder's sweep
// finalizes such a run from the stored index, and a later marker changes
// nothing.
func TestOwedFinalizedMarkerAgeBound(t *testing.T) {
	runID := newTestRunID()
	h, clock, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	pub := downPublisher()
	h.publisher = pub

	h.oweFinalizedMarker("ns1", "cell1", runID, "tenant-a/build")
	clock.advance(logMarkerResendAge - time.Second)
	h.resendOwedMarkers(context.Background())
	if n := owedMarkerCount(h); n != 1 {
		t.Fatalf("owed markers inside the age bound = %d, want 1", n)
	}

	clock.advance(time.Second)
	bringPublisherUp(pub)
	h.resendOwedMarkers(context.Background())
	if got := finalizedPublishes(pub, runID); got != 0 {
		t.Fatalf("published %d finalized envelopes for a marker past its age bound, want 0", got)
	}
	if n := owedMarkerCount(h); n != 0 {
		t.Fatalf("owed markers past the age bound = %d, want 0", n)
	}
}

// TestOwedFinalizedMarkerCountBound proves that the gateway keeps at most
// logMarkersOwedMax owed markers. A marker over the bound is not kept,
// and a repeat of a kept marker adds nothing.
func TestOwedFinalizedMarkerCountBound(t *testing.T) {
	h, _, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	h.logMarkerMu.Lock()
	h.logMarkersOwed = make(map[string]*owedFinalizedMarker, logMarkersOwedMax)
	for i := 0; i < logMarkersOwedMax; i++ {
		h.logMarkersOwed[fmt.Sprintf("run-%d", i)] = &owedFinalizedMarker{ns: "ns1", name: "cell1"}
	}
	h.logMarkerMu.Unlock()

	runID := newTestRunID()
	h.oweFinalizedMarker("ns1", "cell1", runID, "tenant-a/build")
	h.oweFinalizedMarker("ns1", "cell1", "run-0", "tenant-a/build")
	if n := owedMarkerCount(h); n != logMarkersOwedMax {
		t.Fatalf("owed markers = %d, want the bound %d", n, logMarkersOwedMax)
	}
	h.logMarkerMu.Lock()
	_, kept := h.logMarkersOwed[runID]
	h.logMarkerMu.Unlock()
	if kept {
		t.Fatal("expected the marker over the bound not kept")
	}
}

// TestCloseEndsTheFinalizedPublishWait proves that Close ends a drain that
// waits for a finalized publish. The stopping gateway sends no ack for
// the final chunk and owes no marker: the mite keeps the chunk, and the
// restart recovery of its next send publishes the marker.
func TestCloseEndsTheFinalizedPublishWait(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	sender := &recordingSender{}
	h, _, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	pub := downPublisher()
	h.publisher = pub
	h.logMarkerWait = time.Hour
	h.logStop = make(chan struct{})
	h.contentDone = make(chan struct{})

	sendInst1Chunk(h, runID, 0, "a", false)
	waitForLogAcks(t, sender, 1)
	sendInst1Chunk(h, runID, 1, "b", true)
	waitFor(t, func() bool { return syncPublishCalls(pub) >= 1 }, 10*time.Second, "the drain did not try the marker")

	h.Close()
	waitForEviction(t, h, runID)
	if acks := sender.logAcks(); len(acks) != 1 {
		t.Fatalf("got acks %+v, want only chunk 0: the mite must keep the final chunk", acks)
	}
	if n := owedMarkerCount(h); n != 0 {
		t.Fatalf("owed markers = %d, want 0 for a stopping gateway", n)
	}
	if got := finalizedPublishes(pub, runID); got != 0 {
		t.Fatalf("published %d finalized envelopes, want 0", got)
	}
}

// slowPublisher is a fakePublisher whose each JetStream publish takes
// cost on clock, as a slow bus that still takes each publish.
type slowPublisher struct {
	*fakePublisher
	clock *stepClock
	cost  time.Duration
}

func (p *slowPublisher) PublishJetStream(subject string, data []byte) (*nats.PubAck, error) {
	p.clock.advance(p.cost)
	return p.fakePublisher.PublishJetStream(subject, data)
}

// oweMarkers owes the marker of each run in ids on h, in order, one step
// apart on clock.
func oweMarkers(h *BusHandler, clock *stepClock, ids []string, step time.Duration) {
	for _, id := range ids {
		h.oweFinalizedMarker("ns1", "cell1", id, "tenant-a/build")
		clock.advance(step)
	}
}

// markerRunIDs returns n run ids with prefix that sort in index order.
func markerRunIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%05d", prefix, i)
	}
	return ids
}

// publishedRunIDs returns the run id of each envelope that pub took, in
// publish order.
func publishedRunIDs(pub *fakePublisher) []string {
	ids := make([]string, pub.publishedCount())
	for i := range ids {
		ids[i] = pub.envelopeAt(i).RunID
	}
	return ids
}

// bufferLogger returns a logger that writes Info and above to buf.
func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// logLines returns the lines in buf.
func logLines(buf *bytes.Buffer) []string {
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestOwedMarkerResendPassBound proves that one resend pass sends at
// most logMarkerResendPassMax owed markers, the marker tried longest ago
// first, and keeps the rest for the next pass. Each publish takes 100 ms
// on the step clock, as on a slow bus. The idle sweep round runs after
// the pass on the same goroutine, so the pass time is the time that the
// round waits.
func TestOwedMarkerResendPassBound(t *testing.T) {
	h, clock, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	var logs bytes.Buffer
	h.Logger = bufferLogger(&logs)
	const cost = 100 * time.Millisecond
	pub := &slowPublisher{fakePublisher: &fakePublisher{}, clock: clock, cost: cost}
	h.publisher = pub

	ids := markerRunIDs("run", logMarkersOwedMax)
	oweMarkers(h, clock, ids, time.Millisecond)

	start := clock.Now()
	h.resendOwedMarkers(context.Background())
	held := clock.Now().Sub(start)
	sent := publishedRunIDs(pub.fakePublisher)
	if len(sent) != logMarkerResendPassMax || held != logMarkerResendPassMax*cost {
		t.Fatalf("a pass over %d owed markers made %d publishes and held the idle sweep for %v, want %d publishes and %v",
			logMarkersOwedMax, len(sent), held, logMarkerResendPassMax, logMarkerResendPassMax*cost)
	}
	if held >= logIdleSweepInterval {
		t.Fatalf("a full pass held the idle sweep for %v, want less than the round interval %v", held, logIdleSweepInterval)
	}
	for i, id := range sent {
		if id != ids[i] {
			t.Fatalf("publish %d sent %s, want %s: the pass sends the marker tried longest ago first", i, id, ids[i])
		}
	}
	if n := owedMarkerCount(h); n != logMarkersOwedMax-logMarkerResendPassMax {
		t.Fatalf("owed markers after the pass = %d, want %d", n, logMarkersOwedMax-logMarkerResendPassMax)
	}

	h.resendOwedMarkers(context.Background())
	sent = publishedRunIDs(pub.fakePublisher)
	if len(sent) != 2*logMarkerResendPassMax || sent[logMarkerResendPassMax] != ids[logMarkerResendPassMax] {
		t.Fatalf("two passes made %d publishes, want %d, and the second pass starts at %s",
			len(sent), 2*logMarkerResendPassMax, ids[logMarkerResendPassMax])
	}
}

// TestOwedMarkerResendKeepsATriedMarkerAheadOfNewerOnes proves that a
// marker owed after a failed pass never goes ahead of the marker that the
// pass tried. A pass at the bound therefore cannot starve a marker that
// failed once, and every marker reaches the front of a later pass.
func TestOwedMarkerResendKeepsATriedMarkerAheadOfNewerOnes(t *testing.T) {
	h, clock, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	var logs bytes.Buffer
	h.Logger = bufferLogger(&logs)
	pub := downPublisher()
	h.publisher = pub

	const tried = "run-z"
	oweMarkers(h, clock, []string{tried}, time.Minute)
	h.resendOwedMarkers(context.Background())
	if got := syncPublishCalls(pub); got != 1 {
		t.Fatalf("the pass during the outage made %d publish attempts, want 1", got)
	}

	clock.advance(time.Second)
	newer := markerRunIDs("run-a", logMarkerResendPassMax)
	oweMarkers(h, clock, newer, time.Millisecond)
	bringPublisherUp(pub)
	h.resendOwedMarkers(context.Background())
	sent := publishedRunIDs(pub)
	if len(sent) == 0 || sent[0] != tried {
		first := ""
		if len(sent) > 0 {
			first = sent[0]
		}
		t.Fatalf("the pass after the outage sent %d markers, %q first, want %s first", len(sent), first, tried)
	}
	if len(sent) != logMarkerResendPassMax {
		t.Fatalf("the pass after the outage sent %d markers, want the bound %d", len(sent), logMarkerResendPassMax)
	}
	newest := newer[len(newer)-1]
	if n := owedMarkerCount(h); n != 1 || finalizedPublishes(pub, newest) != 0 {
		t.Fatalf("owed markers = %d, want only the newest marker %s left", n, newest)
	}

	h.resendOwedMarkers(context.Background())
	if got := finalizedPublishes(pub, newest); got != 1 || owedMarkerCount(h) != 0 {
		t.Fatalf("the next pass published %d markers of %s and left %d owed, want 1 and 0", got, newest, owedMarkerCount(h))
	}
}

// TestOwedMarkerBoundsSendAFullSetBeforeTheAgeBound proves that the
// count, pass, and age bounds fit together. A full owed set on a bus that
// takes each publish goes out in passes one idle sweep interval apart,
// and no marker reaches its age bound first.
func TestOwedMarkerBoundsSendAFullSetBeforeTheAgeBound(t *testing.T) {
	h, clock, pub := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	var logs bytes.Buffer
	h.Logger = bufferLogger(&logs)

	oweMarkers(h, clock, markerRunIDs("run", logMarkersOwedMax), 0)
	passes := 0
	for owedMarkerCount(h) > 0 && passes < logMarkersOwedMax {
		clock.advance(logIdleSweepInterval)
		h.resendOwedMarkers(context.Background())
		passes++
	}
	if got := pub.publishedCount(); got != logMarkersOwedMax {
		t.Fatalf("published %d markers of a full owed set, want each of the %d once", got, logMarkersOwedMax)
	}
	if took := time.Duration(passes) * logIdleSweepInterval; took >= logMarkerResendAge {
		t.Fatalf("a full owed set took %d passes (%v), want less than the age bound %v", passes, took, logMarkerResendAge)
	}
}

// TestOwedMarkerResendPassWritesOneSummaryLine proves that a pass writes
// one log line that counts the markers it sent, the failed publish, and
// the markers it dropped by age. A pass with no owed marker writes none.
func TestOwedMarkerResendPassWritesOneSummaryLine(t *testing.T) {
	h, clock, pub := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	var logs bytes.Buffer
	h.Logger = bufferLogger(&logs)

	oweMarkers(h, clock, markerRunIDs("old", 2), 0)
	clock.advance(logMarkerResendAge - time.Minute)
	oweMarkers(h, clock, markerRunIDs("new", 3), 0)
	clock.advance(time.Minute)
	logs.Reset()
	h.resendOwedMarkers(context.Background())
	lines := logLines(&logs)
	if len(lines) != 1 || !strings.Contains(lines[0], "sent=3 failed=0 dropped_by_age=2 owed=0") ||
		!strings.Contains(lines[0], "level=WARN") {
		t.Fatalf("a pass that sent 3 markers and dropped 2 wrote %d lines, want one warning:\n%s", len(lines), logs.String())
	}
	if got := pub.publishedCount(); got != 3 {
		t.Fatalf("published %d markers, want 3", got)
	}

	logs.Reset()
	h.resendOwedMarkers(context.Background())
	if lines := logLines(&logs); len(lines) != 0 {
		t.Fatalf("a pass with no owed marker wrote:\n%s", logs.String())
	}

	oweMarkers(h, clock, markerRunIDs("sent", 2), 0)
	logs.Reset()
	h.resendOwedMarkers(context.Background())
	if lines := logLines(&logs); len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") ||
		!strings.Contains(lines[0], "sent=2 failed=0 dropped_by_age=0 owed=0") {
		t.Fatalf("a pass that sent 2 markers wrote %d lines, want one info line:\n%s", len(lines), logs.String())
	}

	h.publisher = downPublisher()
	oweMarkers(h, clock, markerRunIDs("late", 2), 0)
	logs.Reset()
	h.resendOwedMarkers(context.Background())
	lines = logLines(&logs)
	var summaries []string
	for _, line := range lines {
		if strings.Contains(line, "sent=") {
			summaries = append(summaries, line)
		}
	}
	if len(lines) != 2 || len(summaries) != 1 || !strings.Contains(summaries[0], "level=WARN") ||
		!strings.Contains(summaries[0], "sent=0 failed=1 dropped_by_age=0 owed=2") {
		t.Fatalf("a failed pass wrote %d lines, want the publish failure and one warning summary:\n%s", len(lines), logs.String())
	}
}
