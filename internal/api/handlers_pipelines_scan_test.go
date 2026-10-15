package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// postScan posts a scan of the fixture job "svc/main" of tenant "ci".
func postScan(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := doWriteRequest("/pipelines/ci/jobs/svc/main/scan",
		map[string]string{"team": "ci", "job": "svc/main"}, testClaims(), body)
	rec := httptest.NewRecorder()
	srv.handleScanMultibranch(rec, req)
	return rec
}

// TestScanMultibranchPublishesCommand asserts a permitted scan answers 202
// with a fresh UUIDv7 scan identifier and the requested state, and publishes
// one SCAN_MULTIBRANCH command that names the job and the acting user.
func TestScanMultibranchPublishesCommand(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	rec := postScan(t, srv, scanMultibranchRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var out scanRequestedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.ScanID == "" || out.State != "requested" {
		t.Fatalf("response = %+v, want a scan identifier and state requested", out)
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeScanMultibranch || cmd.ScanMultibranch == nil {
		t.Fatalf("command = %+v, want one SCAN_MULTIBRANCH", cmd)
	}
	if cmd.ScanMultibranch.ScanId != out.ScanID || cmd.ScanMultibranch.JobPath != "svc/main" {
		t.Fatalf("payload = %+v, want scan %s on the job", cmd.ScanMultibranch, out.ScanID)
	}
	if cmd.ScanMultibranch.ActingUser == nil || cmd.ScanMultibranch.ActingUser.Name == "" {
		t.Fatalf("acting user = %+v, want the caller", cmd.ScanMultibranch.ActingUser)
	}
	if cmd.CommandId != "scan-multibranch-"+out.ScanID {
		t.Fatalf("command id = %q", cmd.CommandId)
	}
}

// TestScanMultibranchForbidden asserts a caller without the pipelines write
// verb answers 403 and publishes nothing.
func TestScanMultibranchForbidden(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServerWithVerbs(newFakeRecordReader(), mt, []string{"read"})

	rec := postScan(t, srv, scanMultibranchRequest{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestScanMultibranchHomeRef asserts the scan reaches the member that holds
// the job's home slot, and not the tenant's own coordinate.
func TestScanMultibranchHomeRef(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(reader, mt)

	rec := postScan(t, srv, scanMultibranchRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	if calls[0].ns != "cells-ns" || calls[0].name != "cell-b" {
		t.Fatalf("published to %s/%s, want the home member cells-ns/cell-b", calls[0].ns, calls[0].name)
	}
}
