package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestHandleGetRun_CarriesTheInterruptedReason asserts the run detail
// carries the reason the interruption sweep stored in the run's payload.
func TestHandleGetRun_CarriesTheInterruptedReason(t *testing.T) {
	reader := newFakeRecordReader()
	result := "INTERRUPTED"
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result:  &result,
		Payload: json.RawMessage(`{"interrupted_reason":"no-heartbeat"}`),
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
	if detail.InterruptedReason != "no-heartbeat" {
		t.Fatalf("interruptedReason = %q, want no-heartbeat", detail.InterruptedReason)
	}
}

// TestHandleGetRun_OmitsTheInterruptedReason asserts a run with no such
// value carries no interruptedReason field.
func TestHandleGetRun_OmitsTheInterruptedReason(t *testing.T) {
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
	if _, ok := raw["interruptedReason"]; ok {
		t.Fatal("the response carries interruptedReason for a run that holds none")
	}
}

// TestHandleGetRun_OmitsTheInterruptedReasonAfterSupersede asserts that a
// run superseded by a real terminal event carries no interruptedReason,
// even though the stored payload still holds the reason.
func TestHandleGetRun_OmitsTheInterruptedReasonAfterSupersede(t *testing.T) {
	reader := newFakeRecordReader()
	result := "SUCCESS"
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result:  &result,
		Payload: json.RawMessage(`{"interrupted_reason":"no-heartbeat"}`),
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
	if _, ok := raw["interruptedReason"]; ok {
		t.Fatal("the response carries interruptedReason for a superseded run")
	}
}
