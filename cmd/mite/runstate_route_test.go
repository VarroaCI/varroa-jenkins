package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// The bearer and the job path every request in this file carries.
const (
	runStateTestToken   = "tok"
	runStateTestJobPath = "team/pipeline"
	runStateTestEpoch   = 1700000000000
)

// newRunStateRequest builds a POST /v1/run-state request carrying the
// nine X-Varroa-* headers the route names, plus the bearer and body.
// Every header is a parameter, so a test can drive any one of them.
//
//nolint:unparam // the parameter set mirrors the route's own header set
func newRunStateRequest(runID, relpath string, buildNumber, stateGen, seq, total, rawSize, objectSize int64, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/run-state", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+runStateTestToken)
	if runID != "" {
		req.Header.Set(hdrStateRunID, runID)
	}
	req.Header.Set(hdrStateJobPath, url.QueryEscape(runStateTestJobPath))
	req.Header.Set(hdrStateBuildNumber, strconv.FormatInt(buildNumber, 10))
	req.Header.Set(hdrStateRelpath, url.QueryEscape(relpath))
	req.Header.Set(hdrStateGen, strconv.FormatInt(stateGen, 10))
	req.Header.Set(hdrStateSeq, strconv.FormatInt(seq, 10))
	req.Header.Set(hdrStateTotal, strconv.FormatInt(total, 10))
	req.Header.Set(hdrStateRawSize, strconv.FormatInt(rawSize, 10))
	req.Header.Set(hdrStateObjectSize, strconv.FormatInt(objectSize, 10))
	req.Header.Set(hdrStatePusherEpoch, strconv.FormatInt(runStateTestEpoch, 10))
	return req
}

// newRunStateTestAgent builds an Agent ready to serve handleRunState,
// with a stream that accepts every forwarded message.
func newRunStateTestAgent(t *testing.T) *Agent {
	agent := mustNewAgent(t, Config{CellInstance: "pod-uid-7"})
	agent.Logger = slog.Default()
	agent.loopbackToken = runStateTestToken
	setTestStream(agent, &concurrentSendStream{sendFn: storeRunStateChunkSend(agent)})
	return agent
}

// storeRunStateChunkSend answers every forwarded run-state chunk STORED, as
// the gateway does. An acked chunk waits for its own ack, so a stream that
// never answers would time the route out.
func storeRunStateChunkSend(agent *Agent) func(*mitev1.MiteMessage) error {
	return func(m *mitev1.MiteMessage) error {
		if c := m.GetRunStateChunk(); c != nil {
			agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
				RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
				Status: ackStatusStored, RestartSource: c.RestartSource,
			})
		}
		return nil
	}
}

// --- 9.2a: the /v1/run-state route ---

func TestHandleRunState_MissingHeaderIs400(t *testing.T) {
	agent := newRunStateTestAgent(t)

	// hdrStateRunID left unset makes parseRunStateHeaders fail.
	req := newRunStateRequest("", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "x")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing run id header, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "malformed run-state headers" {
		t.Errorf("body = %q, want %q", got, "malformed run-state headers")
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}

func TestHandleRunState_UnreadableBodyIs400(t *testing.T) {
	agent := newRunStateTestAgent(t)

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "")
	req.Body = io.NopCloser(iotest.ErrReader(errors.New("read failed")))
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unreadable body, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "unreadable run-state body" {
		t.Errorf("body = %q, want %q", got, "unreadable run-state body")
	}
}

func TestHandleRunState_Oversized413CarriesMaxChunkHeader(t *testing.T) {
	agent := newRunStateTestAgent(t)

	big := strings.Repeat("x", runStateChunkMaxBytes+1)
	req := newRunStateRequest("run-1", "jobs/team/builds/1/program.dat",
		1, 1, 0, 1, int64(len(big)), int64(len(big)), big)
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrLogMaxChunkBytes); got != strconv.Itoa(runStateChunkMaxBytes) {
		t.Errorf("%s = %q, want %d", hdrLogMaxChunkBytes, got, runStateChunkMaxBytes)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "run-state chunk over the cap" {
		t.Errorf("body = %q, want %q", got, "run-state chunk over the cap")
	}
}

func TestHandleRunState_ReasonHeaderOn503(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAcks.record(&mitev1.RunStateAck{RunId: "run-1", Status: ackStatusRejected, Reason: stateReasonStateCap})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 1, 5, 5, "hello")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonStateCap {
		t.Errorf("%s = %q, want %q", hdrStateReason, got, stateReasonStateCap)
	}
}

func TestHandleRunState_BusyPauseHeaderReportsTimeLeft(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAcks.record(&mitev1.RunStateAck{
		RunId: "run-1", Status: ackStatusBusy, Reason: stateReasonBudgetFull, PauseMs: 3000,
	})
	// Move the window end back, so the time left differs from pause_ms.
	agent.runStateAcks.mu.Lock()
	r := agent.runStateAcks.refusals["run-1"]
	r.until = time.Now().Add(1200 * time.Millisecond)
	agent.runStateAcks.refusals["run-1"] = r
	agent.runStateAcks.mu.Unlock()

	rec := httptest.NewRecorder()
	agent.handleRunState(rec, newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 1, 5, 5, "hello"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 inside the busy window, got %d", rec.Code)
	}
	left, err := strconv.Atoi(rec.Header().Get(hdrPauseMs))
	if err != nil {
		t.Fatalf("%s = %q: %v", hdrPauseMs, rec.Header().Get(hdrPauseMs), err)
	}
	if left < 1100 || left > 1200 {
		t.Errorf("%s = %d, want the time left in the window, not pause_ms", hdrPauseMs, left)
	}
}

func TestHandleRunState_TenantUnresolvedPauseHeaderReportsTimeLeft(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAcks.record(&mitev1.RunStateAck{
		RunId: "run-1", Status: ackStatusBusy, Reason: stateReasonTenantUnresolved, PauseMs: 3000,
	})
	// Move the window end back, so the time left differs from pause_ms.
	agent.runStateAcks.mu.Lock()
	r := agent.runStateAcks.refusals["run-1"]
	r.until = time.Now().Add(1200 * time.Millisecond)
	agent.runStateAcks.refusals["run-1"] = r
	agent.runStateAcks.mu.Unlock()

	rec := httptest.NewRecorder()
	agent.handleRunState(rec, newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 1, 5, 5, "hello"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 inside the busy window, got %d", rec.Code)
	}
	left, err := strconv.Atoi(rec.Header().Get(hdrPauseMs))
	if err != nil {
		t.Fatalf("%s = %q: %v", hdrPauseMs, rec.Header().Get(hdrPauseMs), err)
	}
	if left < 1100 || left > 1200 {
		t.Errorf("%s = %d, want the time left in the window, not pause_ms", hdrPauseMs, left)
	}
}

// TestHandleRunState_ForwardsMiteOwnCellInstance proves the route stamps
// the mite's own cell instance onto the forwarded chunk, regardless of
// any value a caller might have sent. The route reads no cell-instance
// header at all (design.md D5), so a caller cannot claim another
// identity.
func TestHandleRunState_ForwardsMiteOwnCellInstance(t *testing.T) {
	agent := newRunStateTestAgent(t)
	var sent *mitev1.RunStateChunk
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = m.GetRunStateChunk()
		return storeRunStateChunkSend(agent)(m)
	}})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 1, 5, 5, "hello")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if sent == nil {
		t.Fatal("expected a RunStateChunk forwarded on the stream")
	}
	if sent.CellInstance != "pod-uid-7" {
		t.Errorf("CellInstance = %q, want the mite's own %q", sent.CellInstance, "pod-uid-7")
	}
}

// setRestartSourceHeaders marks a request as one chunk of a restart copy.
// An empty files or bytes value leaves that header unset.
func setRestartSourceHeaders(req *http.Request, files, bytes string) {
	req.Header.Set(hdrStateRestartSource, "1")
	if files != "" {
		req.Header.Set(hdrStateCopyFiles, files)
	}
	if bytes != "" {
		req.Header.Set(hdrStateCopyBytes, bytes)
	}
}

func TestHandleRunState_RestartSourceForwardsFlagAndCounts(t *testing.T) {
	agent := newRunStateTestAgent(t)
	var sent *mitev1.RunStateChunk
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = m.GetRunStateChunk()
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: sent.RunId, Relpath: sent.Relpath, StateGen: sent.StateGen, Seq: sent.Seq,
			Status: ackStatusStored, RestartSource: true,
		})
		return nil
	}})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "3", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if sent == nil || !sent.RestartSource || sent.CopyFiles != 3 || sent.CopyBytes != 4096 {
		t.Fatalf("forwarded chunk = %+v, want the restart flag and the copy counts", sent)
	}
}

func TestHandleRunState_RestartSourceMissingCountIs400(t *testing.T) {
	agent := newRunStateTestAgent(t)
	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing file count, got %d", rec.Code)
	}
}

func TestHandleRunState_RestartSourceCountsOutOfRangeIs400(t *testing.T) {
	for _, tc := range []struct{ files, bytes string }{
		{"0", "4096"},
		{"513", "4096"},
		{"3", "0"},
		{"3", strconv.Itoa(restartSourceMaxBytes + 1)},
	} {
		agent := newRunStateTestAgent(t)
		req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
		setRestartSourceHeaders(req, tc.files, tc.bytes)
		rec := httptest.NewRecorder()
		agent.handleRunState(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("files %s bytes %s: expected 400, got %d", tc.files, tc.bytes, rec.Code)
		}
	}
}

func TestHandleRunState_RestartSourceBadFlagIs400(t *testing.T) {
	agent := newRunStateTestAgent(t)
	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	req.Header.Set(hdrStateRestartSource, "yes")
	req.Header.Set(hdrStateCopyFiles, "3")
	req.Header.Set(hdrStateCopyBytes, "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a flag other than 1, got %d", rec.Code)
	}
}

func TestRunStateAckStore_MeshRefusalDoesNotBlockRestartSource(t *testing.T) {
	agent := newRunStateTestAgent(t)
	ackingStream(agent, ackStatusStored, "")
	agent.runStateAcks.record(&mitev1.RunStateAck{RunId: "run-1", Status: ackStatusRejected, Reason: stateReasonStateCap})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "3", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a mesh refusal blocked a restart chunk: got %d", rec.Code)
	}
}

func TestRunStateAckStore_RestartRefusalDoesNotBlockMesh(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAcks.record(&mitev1.RunStateAck{RunId: "run-1", Status: ackStatusRejected, Reason: stateReasonStateCap, RestartSource: true})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 1, 5, 5, "hello")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a restart refusal blocked a mesh chunk: got %d", rec.Code)
	}
}
