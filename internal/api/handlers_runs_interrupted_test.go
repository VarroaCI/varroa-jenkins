package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

	// A run that never started carries no digest.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["digest"]; ok {
		t.Fatal("the response carries a digest for a run that never started")
	}
}

// rollCausePayload is the payload of a cell-rolled mark that found the
// cell roll.
const rollCausePayload = `{"interrupted_reason":"cell-rolled","interrupted_detail":"MiteUnreachable"}`

// TestGetRun_CarriesTheInterruptedDetail asserts that a cell-rolled run
// answers the stored roll cause on both the HTTP path and the service path
// that get_run calls.
func TestGetRun_CarriesTheInterruptedDetail(t *testing.T) {
	reader := newFakeRecordReader()
	result := "INTERRUPTED"
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: &result, Payload: json.RawMessage(rollCausePayload),
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
	if detail.InterruptedReason != "cell-rolled" || detail.InterruptedDetail != "MiteUnreachable" {
		t.Fatalf("interruptedReason = %q, interruptedDetail = %q, want cell-rolled and MiteUnreachable",
			detail.InterruptedReason, detail.InterruptedDetail)
	}

	out, serr := srv.GetRun(context.Background(), testClaims(), "r1")
	if serr != nil {
		t.Fatalf("GetRun: %v", serr)
	}
	if out.InterruptedDetail != "MiteUnreachable" {
		t.Fatalf("GetRun interruptedDetail = %q, want MiteUnreachable", out.InterruptedDetail)
	}
}

// TestGetRun_OmitsTheInterruptedDetailAfterSupersede asserts that a run that
// a real terminal event superseded answers no interruptedDetail, even though
// the stored payload still holds it.
func TestGetRun_OmitsTheInterruptedDetailAfterSupersede(t *testing.T) {
	reader := newFakeRecordReader()
	result := "SUCCESS"
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: &result, Payload: json.RawMessage(rollCausePayload),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "interruptedDetail") {
		t.Fatalf("the response carries interruptedDetail for a superseded run: %s", rec.Body.String())
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
