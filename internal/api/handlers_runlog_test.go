// Round-1 review fixes for the log SSE endpoint (design D5;
// run-log-viewer, log-live-tail): fromLine renumbering on resume, the
// history-to-live subscribe-before-read ordering, and StatusDuplicate
// as a terminal status for the live tail.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// doStreamRunLogRequest builds a run-log request over ctx, whose Done
// promptly stops runLogStreamLoop after the synchronous history replay,
// so the test never waits on the real 10-second poll ticker.
func doStreamRunLogRequest(ctx context.Context, srv *Server, runID, rawQuery string) *httptest.ResponseRecorder {
	url := "/runs/" + runID + "/log"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetPathValue("run", runID)
	req = req.WithContext(auth.ContextWithClaims(ctx, testClaims()))
	rec := httptest.NewRecorder()
	srv.handleStreamRunLog(rec, req)
	return rec
}

// --- Finding 2: fromLine overrides the emitted numbering on resume ---

// TestHandleStreamRunLog_FromLineOverridesNumbering proves that when
// fromLine is present the handler numbers the resumed lines from it,
// not from the absolute count splitRunLogLines computed, per D5: "When
// it is present the handler SHALL number from it."
func TestHandleStreamRunLog_FromLineOverridesNumbering(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-fromline-1"

	var console strings.Builder
	var entries []runLogIndexEntry
	var line42Offset int64
	for i := 1; i <= 45; i++ {
		if i == 42 {
			line42Offset = int64(console.Len())
		}
		line := fmt.Sprintf("line%d\n", i)
		entries = append(entries, runLogIndexEntry{Seq: int64(i - 1), Offset: int64(console.Len()), Len: int64(len(line)), Final: i == 45})
		console.WriteString(line)
	}
	putRunLogConsole(t, store, runID, []byte(console.String()))
	putRunLogIndex(t, store, runID, entries)

	run := recordstore.Run{
		RunID: runID, Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusCompleted,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"chunk_count":45,"incomplete":false}}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)
	srv := newRunFixtureServer(reader, store)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	rawQuery := fmt.Sprintf("from=%d&fromLine=100", line42Offset)
	rec := doStreamRunLogRequest(ctx, srv, runID, rawQuery)

	lines := eventsNamed(parseSSEEvents(rec.Body.String()), "line")
	if len(lines) < 2 {
		t.Fatalf("expected at least two resumed line events, got %d: %s", len(lines), rec.Body.String())
	}
	var first, second runLogLineEvent
	if err := json.Unmarshal([]byte(lines[0].Data), &first); err != nil {
		t.Fatalf("decode first line event: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1].Data), &second); err != nil {
		t.Fatalf("decode second line event: %v", err)
	}
	if first.N != 100 || first.Text != "line42" {
		t.Fatalf("first resumed line = %+v, want n=100 text=line42", first)
	}
	if second.N != 101 || second.Text != "line43" {
		t.Fatalf("second resumed line = %+v, want n=101 text=line43", second)
	}
}

// TestHandleStreamRunLog_NoFromLineKeepsAbsoluteNumbering proves the
// absent case is unchanged: the handler still numbers from the object's
// own absolute count, per D5's other resume scenario.
func TestHandleStreamRunLog_NoFromLineKeepsAbsoluteNumbering(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-fromline-2"

	console := []byte("alpha\nbeta\ngamma\n")
	entries := []runLogIndexEntry{
		{Seq: 0, Offset: 0, Len: 6},
		{Seq: 1, Offset: 6, Len: 5},
		{Seq: 2, Offset: 11, Len: 6, Final: true},
	}
	putRunLogConsole(t, store, runID, console)
	putRunLogIndex(t, store, runID, entries)

	run := recordstore.Run{
		RunID: runID, Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusCompleted,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"chunk_count":3,"incomplete":false}}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)
	srv := newRunFixtureServer(reader, store)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	rec := doStreamRunLogRequest(ctx, srv, runID, "from=6")

	lines := eventsNamed(parseSSEEvents(rec.Body.String()), "line")
	if len(lines) != 2 {
		t.Fatalf("expected two resumed lines, got %d: %s", len(lines), rec.Body.String())
	}
	var first runLogLineEvent
	if err := json.Unmarshal([]byte(lines[0].Data), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.N != 2 || first.Text != "beta" {
		t.Fatalf("first resumed line = %+v, want the object's own absolute number 2", first)
	}
}

// --- Finding 3: the live subject is subscribed before history is read ---

// racingBlobStore triggers onConsoleRead exactly once, the instant the
// handler reads the run's console object. The test uses that hook to
// publish a live chunk at that exact moment, standing in for "a chunk
// durably written after the history read but before SubscribeData
// starts" — the fix moves the subscribe ahead of this read, so the
// chunk must already be captured on the (buffered) live channel by the
// time this fires.
type racingBlobStore struct {
	blob.Store
	runID         string
	onConsoleRead func()
	once          sync.Once
}

func (r *racingBlobStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == blob.LogKey(r.runID) {
		r.once.Do(r.onConsoleRead)
	}
	return r.Store.NewRangeReader(ctx, key, offset, length)
}

// TestHandleStreamRunLog_LiveTailCatchesChunkPublishedDuringHistoryRead
// proves the history-to-live race D5's dedup section presupposes is
// closed: a chunk published live at the exact instant of the handler's
// own history read is still emitted, because the fix subscribes before
// that read starts rather than after the whole history replay.
func TestHandleStreamRunLog_LiveTailCatchesChunkPublishedDuringHistoryRead(t *testing.T) {
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, cell = "core", "ns1", "cell1"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, cell)

	runID := "run-race-1"
	realStore := newRunLogTestStore(t)
	putRunLogConsole(t, realStore, runID, []byte("existing\n"))
	putRunLogIndex(t, realStore, runID, []runLogIndexEntry{{Seq: 0, Offset: 0, Len: 9}})

	published := make(chan struct{})
	store := &racingBlobStore{Store: realStore, runID: runID, onConsoleRead: func() {
		liveBytes := []byte("live-line\n")
		chunk := runLiveLogChunk{RunID: runID, Seq: 1, Offset: 9, Len: int64(len(liveBytes)), Data: liveBytes}
		body, marshalErr := json.Marshal(chunk)
		if marshalErr != nil {
			t.Errorf("marshal live chunk: %v", marshalErr)
			return
		}
		if pubErr := conn.Publish(bus.LogSubject(cluster, ns, cell, runID), body); pubErr != nil {
			t.Errorf("publish live chunk: %v", pubErr)
		}
		close(published)
	}}

	run := recordstore.Run{
		RunID: runID, Cluster: cluster, Namespace: ns, Cell: cell,
		Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusRunning,
		Payload: json.RawMessage(`{}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)

	srv := newRunFixtureServer(reader, store)
	srv.runLogTailReg = newRunTailRegistry(conn)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(400*time.Millisecond, cancel)
	rec := doStreamRunLogRequest(ctx, srv, runID, "")

	select {
	case <-published:
	default:
		t.Fatal("expected the history read to have triggered the publish")
	}

	var gotLive bool
	for _, ev := range eventsNamed(parseSSEEvents(rec.Body.String()), "line") {
		var le runLogLineEvent
		if err := json.Unmarshal([]byte(ev.Data), &le); err == nil && le.Text == "live-line" {
			gotLive = true
		}
	}
	if !gotLive {
		t.Fatalf("expected the chunk published during the history read to be emitted exactly once, got: %s", rec.Body.String())
	}
}

// --- Risk: a DUPLICATE run is terminal too; it must attach no tail ---

// TestHandleStreamRunLog_DuplicateRunAttachesNoLiveTail proves a
// StatusDuplicate run emits live.active=false and sends no tail-control
// start request, matching StatusCompleted's own treatment.
func TestHandleStreamRunLog_DuplicateRunAttachesNoLiveTail(t *testing.T) {
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, cell = "core", "ns2", "cell2"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, cell)

	runID := "run-dup-1"
	store := newRunLogTestStore(t)
	putRunLogConsole(t, store, runID, []byte("only line\n"))
	putRunLogIndex(t, store, runID, []runLogIndexEntry{{Seq: 0, Offset: 0, Len: 10, Final: true}})

	run := recordstore.Run{
		RunID: runID, Cluster: cluster, Namespace: ns, Cell: cell,
		Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusDuplicate,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"incomplete":false}}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)

	srv := newRunFixtureServer(reader, store)
	srv.runLogTailReg = newRunTailRegistry(conn)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	rec := doStreamRunLogRequest(ctx, srv, runID, "")

	liveEvents := eventsNamed(parseSSEEvents(rec.Body.String()), "live")
	if len(liveEvents) != 1 {
		t.Fatalf("expected exactly one live event, got %d: %s", len(liveEvents), rec.Body.String())
	}
	var le runLogLiveEvent
	if err := json.Unmarshal([]byte(liveEvents[0].Data), &le); err != nil {
		t.Fatalf("decode live event: %v", err)
	}
	if le.Active {
		t.Fatal("expected live.active=false for a terminal DUPLICATE run")
	}
	if got := atomic.LoadInt32(&gw.starts); got != 0 {
		t.Fatalf("expected no tail-control start request for a DUPLICATE run, got %d", got)
	}
}

// --- A live chunk after a byte gap ---

// growingConsoleStore answers the first read of the run's console object
// with first, and every later read from the wrapped store. It stands in
// for a blob that grows after the handler's history read, while the
// relay message of the chunk in between is lost.
type growingConsoleStore struct {
	blob.Store
	runID         string
	first         []byte
	onConsoleRead func()
	reads         atomic.Int32
}

func (g *growingConsoleStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == blob.LogKey(g.runID) && g.reads.Add(1) == 1 {
		if g.onConsoleRead != nil {
			g.onConsoleRead()
		}
		return io.NopCloser(strings.NewReader(string(g.first))), nil
	}
	return g.Store.NewRangeReader(ctx, key, offset, length)
}

// runLogGapFixture opens a stream on a running run whose history read
// sees "alpha\n" only. The blob store then holds full. During the
// history read the fixture publishes one live chunk, "live\n", at
// liveOffset, so the chunk that holds the bytes in between never reaches
// the tail. It returns the response and the time the handler took.
func runLogGapFixture(t *testing.T, runID, rawQuery string, full []byte, liveOffset int64) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	natsSrv := startRunLogTestNATS(t)
	conn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	const cluster, ns, cell = "core", "ns1", "cell1"
	gw := &fakeTailGateway{}
	gw.subscribe(t, conn, ns, cell)

	realStore := newRunLogTestStore(t)
	putRunLogConsole(t, realStore, runID, full)

	store := &growingConsoleStore{Store: realStore, runID: runID, first: []byte("alpha\n"), onConsoleRead: func() {
		liveBytes := []byte("live\n")
		chunk := runLiveLogChunk{RunID: runID, Seq: 2, Offset: liveOffset, Len: int64(len(liveBytes)), Data: liveBytes}
		body, marshalErr := json.Marshal(chunk)
		if marshalErr != nil {
			t.Errorf("marshal live chunk: %v", marshalErr)
			return
		}
		if pubErr := conn.Publish(bus.LogSubject(cluster, ns, cell, runID), body); pubErr != nil {
			t.Errorf("publish live chunk: %v", pubErr)
		}
	}}

	run := recordstore.Run{
		RunID: runID, Cluster: cluster, Namespace: ns, Cell: cell,
		Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusRunning,
		Payload: json.RawMessage(`{}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)

	srv := newRunFixtureServer(reader, store)
	srv.runLogTailReg = newRunTailRegistry(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	rec := doStreamRunLogRequest(ctx, srv, runID, rawQuery)
	return rec, time.Since(start)
}

// runLogLineEvents decodes every line event of body, in order.
func runLogLineEvents(t *testing.T, body string) []runLogLineEvent {
	t.Helper()
	events := eventsNamed(parseSSEEvents(body), "line")
	out := make([]runLogLineEvent, 0, len(events))
	for _, ev := range events {
		var le runLogLineEvent
		if err := json.Unmarshal([]byte(ev.Data), &le); err != nil {
			t.Fatalf("decode line event %q: %v", ev.Data, err)
		}
		out = append(out, le)
	}
	return out
}

func assertRunLogLines(t *testing.T, got []runLogLineEvent, want []runLogLineEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].N != want[i].N || got[i].Off != want[i].Off || got[i].Text != want[i].Text {
			t.Fatalf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestHandleStreamRunLog_LiveGapResyncsFromBlob proves that a live chunk
// after a byte gap makes the handler emit the lost lines from the blob
// store, with true numbers and offsets, and then the chunk's own line,
// each once.
func TestHandleStreamRunLog_LiveGapResyncsFromBlob(t *testing.T) {
	rec, _ := runLogGapFixture(t, "run-gap-1", "", []byte("alpha\nlost\nlive\n"), 11)
	assertRunLogLines(t, runLogLineEvents(t, rec.Body.String()), []runLogLineEvent{
		{N: 1, Off: 0, Text: "alpha"},
		{N: 2, Off: 6, Text: "lost"},
		{N: 3, Off: 11, Text: "live"},
	})
}

// TestHandleStreamRunLog_LiveGapResyncKeepsFromLineNumbering proves the
// resync continues a fromLine override rather than reverting to the
// absolute line count.
func TestHandleStreamRunLog_LiveGapResyncKeepsFromLineNumbering(t *testing.T) {
	rec, _ := runLogGapFixture(t, "run-gap-2", "from=0&fromLine=10", []byte("alpha\nlost\nlive\n"), 11)
	assertRunLogLines(t, runLogLineEvents(t, rec.Body.String()), []runLogLineEvent{
		{N: 10, Off: 0, Text: "alpha"},
		{N: 11, Off: 6, Text: "lost"},
		{N: 12, Off: 11, Text: "live"},
	})
}

// TestHandleStreamRunLog_LiveGapEndsStreamWhenBlobLags proves that a gap
// the blob store cannot fill ends the stream at once, with no end event
// and no line after the gap, so the client reconnects with its own from
// offset.
func TestHandleStreamRunLog_LiveGapEndsStreamWhenBlobLags(t *testing.T) {
	rec, took := runLogGapFixture(t, "run-gap-3", "", []byte("alpha\n"), 11)
	if took >= 2*time.Second {
		t.Fatalf("handler ran %v, want it to end the stream at the gap", took)
	}
	body := rec.Body.String()
	if n := len(eventsNamed(parseSSEEvents(body), "end")); n != 0 {
		t.Fatalf("end events = %d, want 0", n)
	}
	assertRunLogLines(t, runLogLineEvents(t, body), []runLogLineEvent{{N: 1, Off: 0, Text: "alpha"}})
}

// failingSSEWriter refuses every write, like a client that went away.
type failingSSEWriter struct{ writes atomic.Int32 }

func (f *failingSSEWriter) Write([]byte) (int, error) {
	f.writes.Add(1)
	return 0, io.ErrClosedPipe
}

func (f *failingSSEWriter) Flush() {}

// TestRunLogStreamLoop_LiveWriteErrorEndsStream proves a failed write of
// a live line ends the stream loop instead of waiting for the next tick.
func TestRunLogStreamLoop_LiveWriteErrorEndsStream(t *testing.T) {
	srv := runPollTestServer(newFakeRecordReader())
	live := make(chan []byte, 1)
	body, err := json.Marshal(runLiveLogChunk{RunID: "run-gap-4", Offset: 0, Len: 4, Data: []byte("one\n")})
	if err != nil {
		t.Fatalf("marshal live chunk: %v", err)
	}
	live <- body

	w := &failingSSEWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.runLogStreamLoop(ctx, w, w, recordstore.Run{RunID: "run-gap-4"}, map[string]string{}, nil, live, newRunLogLiveAssembler(0, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream loop kept running after a failed write")
	}
}

// TestHandleStreamRunLog_StepLinesJoinTheirStage proves a step line with
// no chunk stage joins the stage of its flow node, and the stage event
// starts at the labelled block line, not at the outer stage step.
func TestHandleStreamRunLog_StepLinesJoinTheirStage(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-stage-notes-1"

	noteLines := []runLogLine{
		testNewNodeNoteLine("4", "17", "17", "[Pipeline] stage"),
		testNewNodeNoteLine("17", "18", "18", "[Pipeline] { (boom)"),
		testNewNodeNoteLine("18", "19", "", "[Pipeline] error"),
		testNewNodeNoteLine("", "20", "18", "[Pipeline] }"),
		testNewNodeNoteLine("", "21", "17", "[Pipeline] // stage"),
	}
	var console []byte
	entries := make([]runLogIndexEntry, 0, len(noteLines))
	for i, ln := range noteLines {
		raw := append(testNoteRawBytes(ln), '\n')
		entries = append(entries, runLogIndexEntry{Seq: int64(i), Offset: int64(len(console)), Len: int64(len(raw)), Final: i == len(noteLines)-1})
		console = append(console, raw...)
	}
	putRunLogConsole(t, store, runID, console)
	putRunLogIndex(t, store, runID, entries)

	run := recordstore.Run{
		RunID: runID, Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: recordstore.StatusCompleted,
		Payload: json.RawMessage(`{"log_summary":{"finalized":true,"chunk_count":5,"incomplete":false}}`),
	}
	reader := newFakeRecordReader()
	reader.addRun(run)
	reader.stages[runID] = []recordstore.Stage{{RunID: runID, StageID: "18", Name: "boom", Status: "FAILURE"}}
	srv := newRunFixtureServer(reader, store)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	rec := doStreamRunLogRequest(ctx, srv, runID, "")

	lines := runLogLineEvents(t, rec.Body.String())
	wantStage := []string{"", "18", "18", "18", ""}
	if len(lines) != len(wantStage) {
		t.Fatalf("lines = %d, want %d: %s", len(lines), len(wantStage), rec.Body.String())
	}
	for i, ln := range lines {
		if ln.StageID != wantStage[i] {
			t.Errorf("line %d (%q) stageId = %q, want %q", ln.N, ln.Text, ln.StageID, wantStage[i])
		}
	}

	stages := eventsNamed(parseSSEEvents(rec.Body.String()), "stage")
	if len(stages) != 1 {
		t.Fatalf("stage events = %d, want 1: %s", len(stages), rec.Body.String())
	}
	var ev runLogStageEvent
	if err := json.Unmarshal([]byte(stages[0].Data), &ev); err != nil {
		t.Fatalf("decode stage event: %v", err)
	}
	if ev.StageID != "18" || ev.StartLine != 2 || ev.StartOffset != int64(len(testNoteRawBytes(noteLines[0]))+1) {
		t.Fatalf("stage event = %+v, want stage 18 starting at line 2", ev)
	}
}
