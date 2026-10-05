package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

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
func newRunStateTestAgent() *Agent {
	agent := NewAgent(Config{CellInstance: "pod-uid-7"})
	agent.Logger = slog.Default()
	agent.loopbackToken = runStateTestToken
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})
	return agent
}

// --- 9.2a: the /v1/run-state route ---

func TestHandleRunState_MissingHeaderIs400(t *testing.T) {
	agent := newRunStateTestAgent()

	// hdrStateRunID left unset makes parseRunStateHeaders fail.
	req := newRunStateRequest("", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "x")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing run id header, got %d", rec.Code)
	}
}

func TestHandleRunState_Oversized413CarriesMaxChunkHeader(t *testing.T) {
	agent := newRunStateTestAgent()

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
}

func TestHandleRunState_ReasonHeaderOn503(t *testing.T) {
	agent := newRunStateTestAgent()
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

// TestHandleRunState_ForwardsMiteOwnCellInstance proves the route stamps
// the mite's own cell instance onto the forwarded chunk, regardless of
// any value a caller might have sent. The route reads no cell-instance
// header at all (design.md D5), so a caller cannot claim another
// identity.
func TestHandleRunState_ForwardsMiteOwnCellInstance(t *testing.T) {
	agent := newRunStateTestAgent()
	var sent *mitev1.RunStateChunk
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent = m.GetRunStateChunk()
		return nil
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
