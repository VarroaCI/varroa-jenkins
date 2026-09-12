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
