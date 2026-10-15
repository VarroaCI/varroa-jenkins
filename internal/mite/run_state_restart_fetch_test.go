package mite

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// restartFetchAnswer collects the inflated body of one fetch and the error
// text of the answer, if any.
func restartFetchAnswer(t *testing.T, h *BusHandler, req *mitev1.RunStateRequest) ([]byte, string) {
	t.Helper()
	var body []byte
	var errText string
	h.OnRunStateRequest("cell1", "ns1", req, func(m *mitev1.OperatorMessage) error {
		resp := m.GetRunStateResponse()
		if resp == nil {
			t.Fatalf("answer is not a run-state response: %+v", m)
		}
		if resp.Error != "" {
			errText = resp.Error
			return nil
		}
		zr, err := gzip.NewReader(bytes.NewReader(resp.GzipContent))
		if err != nil {
			t.Fatalf("open answer: %v", err)
		}
		part, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("inflate answer: %v", err)
		}
		body = append(body, part...)
		return nil
	})
	return body, errText
}

// TestOnRunStateRequest_RestartSourceServesCopyWithoutEpoch proves a fetch
// flagged as a restart copy reads the copy's own key. The request names no
// pusher epoch and no push generation, because the copy's key carries neither.
func TestOnRunStateRequest_RestartSourceServesCopyWithoutEpoch(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)
	relpath := restartTestBuildPath + "build.xml"
	data := []byte("<flow-build><result>SUCCESS</result></flow-build>")
	h.OnRunStateChunk("cell1", "ns1", restartOneChunk(runID, relpath, 100, 1, int64(len(data)), data))

	requestID := uint64(9)
	got, errText := restartFetchAnswer(t, h, &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: relpath, RestartSource: true,
	})
	if errText != "" {
		t.Fatalf("fetch answered an error: %s", errText)
	}
	if string(got) != string(data) {
		t.Fatalf("fetched body = %q, want %q", got, data)
	}
}

// TestOnRunStateRequest_RestartSourceMissingCopyIsAnError proves a restart
// fetch for a run with no copy answers an error, never an empty file.
func TestOnRunStateRequest_RestartSourceMissingCopyIsAnError(t *testing.T) {
	sender := &recordingSender{}
	runID := newTestRunID()
	h := newRunStateTestHandler(t, newRestartTestOwner(runID), sender)

	requestID := uint64(10)
	got, errText := restartFetchAnswer(t, h, &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: restartTestBuildPath + "build.xml", RestartSource: true,
	})
	if errText == "" {
		t.Fatalf("fetch of a missing copy answered a body %q, want an error", got)
	}
}
