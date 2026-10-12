package jenkins

import (
	"context"
	"net/http"
	"testing"
)

// TestScanMultibranchPostsTheRequestedBody asserts the scan method posts the
// scan identifier, the job path, and the acting user to the plugin's own
// route, and reports the upstream status beside the error.
func TestScanMultibranchPostsTheRequestedBody(t *testing.T) {
	srv, rec := newInputRouteServer(t, http.StatusAccepted)
	c := NewClient(srv.URL, "user", "token")
	status, err := c.ScanMultibranch(context.Background(), ScanMultibranchRequest{
		ScanID:     "0199d0b7-0000-7000-8000-000000000009",
		JobPath:    "team/multi",
		ActingUser: &ActingUser{Name: "ana"},
	})
	if err != nil {
		t.Fatalf("ScanMultibranch: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if rec.path != "/varroa-run/scan" {
		t.Fatalf("path = %q, want /varroa-run/scan", rec.path)
	}
	if rec.body["scanId"] != "0199d0b7-0000-7000-8000-000000000009" || rec.body["jobPath"] != "team/multi" {
		t.Fatalf("body = %+v", rec.body)
	}
	acting, ok := rec.body["actingUser"].(map[string]interface{})
	if !ok || acting["name"] != "ana" {
		t.Fatalf("actingUser = %+v", rec.body["actingUser"])
	}

	refusedSrv, _ := newInputRouteServer(t, http.StatusForbidden)
	refused := NewClient(refusedSrv.URL, "user", "token")
	refusedStatus, refusedErr := refused.ScanMultibranch(context.Background(), ScanMultibranchRequest{JobPath: "team/multi"})
	if refusedErr == nil || refusedStatus != http.StatusForbidden {
		t.Fatalf("status = %d, err = %v; want 403 and an error", refusedStatus, refusedErr)
	}
}
