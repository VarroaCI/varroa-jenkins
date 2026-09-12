package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// syncSSEWriter is a concurrency-safe io.Writer + Flush(), standing in
// for the real ResponseWriter.
type syncSSEWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncSSEWriter) Flush() {}

func (w *syncSSEWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// --- 5.7: the terminal poll ---

// sseEvent is one parsed "event: X\ndata: Y" pair from a recorded SSE
// body.
type sseEvent struct {
	Event string
	Data  string
}

func parseSSEEvents(body string) []sseEvent {
	var events []sseEvent
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if ev, ok := strings.CutPrefix(lines[i], "event: "); ok {
			if i+1 < len(lines) {
				if data, ok := strings.CutPrefix(lines[i+1], "data: "); ok {
					events = append(events, sseEvent{Event: ev, Data: data})
				}
			}
		}
	}
	return events
}

func eventsNamed(events []sseEvent, name string) []sseEvent {
	var out []sseEvent
	for _, e := range events {
		if e.Event == name {
			out = append(out, e)
		}
	}
	return out
}

// runPollTestServer builds a bare *Server wired only for the poll loop:
// a fakeRecordReader and a logger.
func runPollTestServer(reader *fakeRecordReader) *Server {
	return &Server{deps: &Dependencies{RecordStore: reader, Logger: slog.Default()}}
}

// TestPollRunLogOnce_NoEndUntilFinalizedMarker proves a completed record
// with no finalized marker emits no end, and that the marker then emits
// end within one poll. Calling pollRunLogOnce directly — rather than
// waiting on a real ticker — is this reader's fake clock: each call is
// exactly one poll, with no dependency on wall time.
func TestPollRunLogOnce_NoEndUntilFinalizedMarker(t *testing.T) {
	reader := newFakeRecordReader()
	run := recordstore.Run{RunID: "run-poll-1", Status: recordstore.StatusCompleted, Payload: json.RawMessage(`{}`)}
	reader.addRun(run)
	srv := runPollTestServer(reader)
	rec := &syncSSEWriter{}
	known := map[string]string{}

	if done := srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil); done {
		t.Fatal("expected no end before the marker is set")
	}
	if got := len(eventsNamed(parseSSEEvents(rec.String()), "end")); got != 0 {
		t.Fatalf("expected no end event written, got %d", got)
	}

	run.Payload = json.RawMessage(`{"log_summary":{"finalized":true,"incomplete":false}}`)
	reader.addRun(run)

	if done := srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil); !done {
		t.Fatal("expected end within one poll once the marker is set")
	}
	ends := eventsNamed(parseSSEEvents(rec.String()), "end")
	if len(ends) != 1 {
		t.Fatalf("expected exactly one end event, got %d: %s", len(ends), rec.String())
	}
}

// TestPollRunLogOnce_FlushesBufferedPartialBeforeEnd proves a fragment
// still buffered in the assembler when the run finalizes is emitted as
// its own final line, before the end event, instead of being dropped.
func TestPollRunLogOnce_FlushesBufferedPartialBeforeEnd(t *testing.T) {
	reader := newFakeRecordReader()
	run := recordstore.Run{
		RunID: "run-poll-partial", Status: recordstore.StatusCompleted,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"incomplete":false}}`),
	}
	reader.addRun(run)
	srv := runPollTestServer(reader)
	rec := &syncSSEWriter{}
	known := map[string]string{}

	assembler := newRunLogLiveAssembler(0, 1)
	assembler.seedPartial([]byte("unterminated tail"), 0)

	if done := srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, assembler); !done {
		t.Fatal("expected end within one poll once the marker is set")
	}

	events := parseSSEEvents(rec.String())
	lines := eventsNamed(events, "line")
	if len(lines) != 1 {
		t.Fatalf("expected the buffered fragment to be flushed as one line event, got %d: %s", len(lines), rec.String())
	}
	var ln runLogLineEvent
	if err := json.Unmarshal([]byte(lines[0].Data), &ln); err != nil {
		t.Fatalf("decode line event: %v", err)
	}
	if ln.Text != "unterminated tail" {
		t.Fatalf("line event text = %q, want %q", ln.Text, "unterminated tail")
	}

	ends := eventsNamed(events, "end")
	if len(ends) != 1 {
		t.Fatalf("expected exactly one end event, got %d", len(ends))
	}
	if events[len(events)-1].Event != "end" {
		t.Fatalf("expected the flushed line to precede end, got order: %+v", events)
	}
}

// TestPollRunLogOnce_IncompleteSummaryReported proves an incomplete
// finalized summary sets logComplete false and carries the reason.
func TestPollRunLogOnce_IncompleteSummaryReported(t *testing.T) {
	reader := newFakeRecordReader()
	run := recordstore.Run{
		RunID: "run-poll-2", Status: recordstore.StatusCompleted,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"incomplete":true,"incomplete_reason":"gateway-gap"}}`),
	}
	reader.addRun(run)
	srv := runPollTestServer(reader)
	rec := &syncSSEWriter{}
	known := map[string]string{}

	if done := srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil); !done {
		t.Fatal("expected end on the first poll: the marker is already set")
	}

	ends := eventsNamed(parseSSEEvents(rec.String()), "end")
	if len(ends) != 1 {
		t.Fatalf("expected exactly one end event, got %d", len(ends))
	}
	var e runLogEndEvent
	if err := json.Unmarshal([]byte(ends[0].Data), &e); err != nil {
		t.Fatalf("decode end event: %v", err)
	}
	if e.LogComplete || e.IncompleteReason != "gateway-gap" {
		t.Fatalf("end event = %+v, want logComplete=false reason=gateway-gap", e)
	}
}

// TestPollRunLogOnce_NewStageEmitsOneEvent proves a stage the poll
// discovers as new produces exactly one stage event, and that an
// unchanged stage on the next poll produces none.
func TestPollRunLogOnce_NewStageEmitsOneEvent(t *testing.T) {
	reader := newFakeRecordReader()
	run := recordstore.Run{RunID: "run-poll-3", Status: recordstore.StatusRunning, Payload: json.RawMessage(`{}`)}
	reader.addRun(run)
	reader.stages[run.RunID] = []recordstore.Stage{{RunID: run.RunID, StageID: "s1", Name: "Build", Status: "IN_PROGRESS"}}
	srv := runPollTestServer(reader)
	rec := &syncSSEWriter{}
	known := map[string]string{}

	if done := srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil); done {
		t.Fatal("expected no end: the run has no finalized marker")
	}
	stages := eventsNamed(parseSSEEvents(rec.String()), "stage")
	if len(stages) != 1 {
		t.Fatalf("expected exactly one stage event for the newly discovered stage, got %d", len(stages))
	}

	// A second poll with the same stage status must not repeat the event.
	srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil)
	if got := len(eventsNamed(parseSSEEvents(rec.String()), "stage")); got != 1 {
		t.Fatalf("expected the stage event to fire once, got %d", got)
	}

	// A status change on the same stage fires a second event.
	reader.stages[run.RunID] = []recordstore.Stage{{RunID: run.RunID, StageID: "s1", Name: "Build", Status: "SUCCESS"}}
	srv.pollRunLogOnce(context.Background(), rec, rec, run, &known, nil)
	if got := len(eventsNamed(parseSSEEvents(rec.String()), "stage")); got != 2 {
		t.Fatalf("expected a second stage event on status change, got %d", got)
	}
}

// TestRunLogStreamLoop_AlreadyStoppedRunStillObservesMarker opens a
// stream on a run that is already stopped, with no live tail attached
// (live is nil), and asserts the stream still emits end once the ticks
// channel delivers a poll that finds the marker set.
func TestRunLogStreamLoop_AlreadyStoppedRunStillObservesMarker(t *testing.T) {
	reader := newFakeRecordReader()
	result := "SUCCESS"
	run := recordstore.Run{
		RunID: "run-poll-4", Status: recordstore.StatusCompleted, Result: &result,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"incomplete":false}}`),
	}
	reader.addRun(run)
	srv := runPollTestServer(reader)

	ticks := make(chan time.Time, 1)
	rec := &syncSSEWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// live is nil: no tail was attached, matching a stream that
		// opens after the run already stopped.
		srv.runLogStreamLoop(ctx, rec, rec, run, map[string]string{}, ticks, nil, newRunLogLiveAssembler(0, 1))
		close(done)
	}()

	ticks <- time.Now()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stream to end")
	}
	ends := eventsNamed(parseSSEEvents(rec.String()), "end")
	if len(ends) != 1 {
		t.Fatalf("expected exactly one end event, got %d", len(ends))
	}
	var e runLogEndEvent
	if err := json.Unmarshal([]byte(ends[0].Data), &e); err != nil {
		t.Fatalf("decode end event: %v", err)
	}
	if e.Result != "SUCCESS" || !e.LogComplete {
		t.Fatalf("end event = %+v, want result=SUCCESS logComplete=true", e)
	}
}
