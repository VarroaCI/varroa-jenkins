package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// rejectedPayload is a run payload whose completion names a refused signature.
const rejectedPayload = `{"run.completed":{"payload":{"result":"FAILURE","rejected_signature":"method java.lang.System exit int"}}}`

// TestGetRun_RejectedSignatureOnFailure proves a failed run answers the
// refused signature on both the HTTP path and the service path.
func TestGetRun_RejectedSignatureOnFailure(t *testing.T) {
	failure := "FAILURE"
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: &failure, Payload: json.RawMessage(rejectedPayload),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail["rejectedSignature"] != "method java.lang.System exit int" {
		t.Fatalf("rejectedSignature = %v, want the refused signature", detail["rejectedSignature"])
	}

	out, serr := srv.GetRun(context.Background(), testClaims(), "r1")
	if serr != nil {
		t.Fatalf("GetRun: %v", serr)
	}
	if out.RejectedSignature != "method java.lang.System exit int" {
		t.Fatalf("GetRun rejectedSignature = %q", out.RejectedSignature)
	}
}

// TestGetRun_RejectedSignatureHiddenOnSuccess proves a run that did not fail
// on a signature answers no refused signature, even when the payload holds one.
func TestGetRun_RejectedSignatureHiddenOnSuccess(t *testing.T) {
	success := "SUCCESS"
	reader := newFakeRecordReader()
	reader.runs["r2"] = recordstore.Run{
		RunID: "r2", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: &success, Payload: json.RawMessage(rejectedPayload),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r2", map[string]string{"run": "r2"}, nil)
	if strings.Contains(rec.Body.String(), "rejectedSignature") {
		t.Fatalf("body = %s, want no rejectedSignature on a successful run", rec.Body.String())
	}
}
