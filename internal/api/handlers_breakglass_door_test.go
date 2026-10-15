package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// TestHandleBreakGlass_LocationURLIsDoor asserts that the break-glass
// location URL is the dashboard door when the operator has a dashboard
// origin, and the in-cluster Service URL when it has none.
func TestHandleBreakGlass_LocationURLIsDoor(t *testing.T) {
	t.Run("dashboard origin set", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		srv.deps.DashboardURL = "https://varroa.example.com"

		resp := breakGlassLocation(t, srv)
		if want := "https://varroa.example.com/headless/core/team-a/ci/"; resp.LocationURL != want {
			t.Errorf("locationUrl = %q, want %q", resp.LocationURL, want)
		}
	})

	t.Run("no dashboard origin", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		cr := seedHeadlessController(t, client)
		srv.deps.DashboardURL = ""

		resp := breakGlassLocation(t, srv)
		want := "http://" + strings.TrimSuffix(controller.PodName(cr, 0), "-0") + "-svc.team-a.svc.cluster.local:8080/"
		if resp.LocationURL != want {
			t.Errorf("locationUrl = %q, want %q", resp.LocationURL, want)
		}
	})
}

// breakGlassLocation posts a break-glass request and decodes the response.
func breakGlassLocation(t *testing.T, srv *Server) breakGlassResponse {
	t.Helper()
	w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident 4711","ttlSeconds":600}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp breakGlassResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}
