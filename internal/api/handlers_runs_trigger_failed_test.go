package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestHandleGetRun_CarriesTheTriggerFailure asserts the run detail carries the
// two reason fields a refused build trigger stored.
func TestHandleGetRun_CarriesTheTriggerFailure(t *testing.T) {
	reader := newFakeRecordReader()
	result := recordstore.ResultTriggerFailed
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result:  &result,
		Payload: json.RawMessage(`{"trigger_error":"no such job","trigger_reason_code":"JOB_NOT_FOUND","trigger_http_status":404}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.TriggerReasonCode != "JOB_NOT_FOUND" {
		t.Fatalf("triggerReasonCode = %q, want JOB_NOT_FOUND", detail.TriggerReasonCode)
	}
	if detail.TriggerError != "no such job" {
		t.Fatalf("triggerError = %q", detail.TriggerError)
	}

	// The stored http status reaches no response field.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["triggerHttpStatus"]; ok {
		t.Fatal("the response carries the stored http status, which it must not")
	}
	// A refused trigger carries no digest: it never reached Jenkins.
	if _, ok := raw["digest"]; ok {
		t.Fatal("the response carries a digest for a refused trigger")
	}
}

// TestHandleGetRun_OmitsTheTriggerFailure asserts a run with no such value
// carries neither field.
func TestHandleGetRun_OmitsTheTriggerFailure(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["triggerError"]; ok {
		t.Fatal("the response carries triggerError for a run that holds none")
	}
	if _, ok := raw["triggerReasonCode"]; ok {
		t.Fatal("the response carries triggerReasonCode for a run that holds none")
	}
}

// TestHandleRerunRun_OnATriggerFailedRun asserts a rerun of a refused trigger
// publishes one trigger with a fresh identity and no parameter, and leaves the
// failed run as it is.
func TestHandleRerunRun_OnATriggerFailedRun(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	result := recordstore.ResultTriggerFailed
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "COMPLETED",
		Result:  &result,
		Payload: json.RawMessage(`{"trigger_reason_code":"JOB_NOT_FOUND"}`),
	}
	srv := writeTestServer(reader, mt)

	req := doWriteRequest("/runs/r1/rerun", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleRerunRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID == "r1" {
		t.Fatal("the rerun must carry a fresh run identifier")
	}
	if _, err := uuid.Parse(resp.RunID); err != nil {
		t.Fatalf("runId %q is not a UUID: %v", resp.RunID, err)
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeTriggerBuild {
		t.Fatalf("command type = %v, want TRIGGER_BUILD", cmd.Type)
	}
	if len(cmd.TriggerBuild.Parameters) != 0 {
		t.Fatalf("parameters = %v, want none: the record stores none", cmd.TriggerBuild.Parameters)
	}

	stored := reader.runs["r1"]
	if stored.Result == nil || *stored.Result != recordstore.ResultTriggerFailed {
		t.Fatalf("the failed run's result = %v, want it unchanged", stored.Result)
	}
}
