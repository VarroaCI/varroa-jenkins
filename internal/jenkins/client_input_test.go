package jenkins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// inputRouteRecorder captures one route call's path and decoded body.
type inputRouteRecorder struct {
	path string
	body map[string]interface{}
}

// newInputRouteServer answers every route with the given status, and it
// records the path and the JSON body of the first call it serves. The
// crumb probe answers 404, so the client posts without a crumb.
func newInputRouteServer(t *testing.T, status int) (*httptest.Server, *inputRouteRecorder) {
	t.Helper()
	rec := &inputRouteRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rec.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &rec.body)
		if status >= http.StatusBadRequest {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("refused"))
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestProceedInputPostsTheRequestedBody asserts the proceed method posts
// the acting user and the parameters to the plugin's own route.
func TestProceedInputPostsTheRequestedBody(t *testing.T) {
	srv, rec := newInputRouteServer(t, http.StatusAccepted)
	c := NewClient(srv.URL, "user", "token")
	status, err := c.ProceedInput(context.Background(), ProceedInputRequest{
		RunID:       "0199d0b7-0000-7000-8000-000000000001",
		JobPath:     "team/job",
		BuildNumber: 7,
		InputID:     "input-1",
		Parameters:  map[string]string{"ENV": "prod"},
		ActingUser: ActingUser{
			Name:   "ana",
			Email:  "ana@example.invalid",
			Groups: []string{"varroa-payments"},
		},
	})
	if err != nil {
		t.Fatalf("ProceedInput: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if rec.path != "/varroa-run/proceed-input" {
		t.Fatalf("path = %q, want /varroa-run/proceed-input", rec.path)
	}
	if rec.body["runId"] != "0199d0b7-0000-7000-8000-000000000001" || rec.body["inputId"] != "input-1" {
		t.Fatalf("body = %+v", rec.body)
	}
	if rec.body["buildNumber"] != float64(7) || rec.body["jobPath"] != "team/job" {
		t.Fatalf("body = %+v", rec.body)
	}
	params, ok := rec.body["parameters"].(map[string]interface{})
	if !ok || params["ENV"] != "prod" {
		t.Fatalf("parameters = %+v", rec.body["parameters"])
	}
	acting, ok := rec.body["actingUser"].(map[string]interface{})
	if !ok || acting["name"] != "ana" || acting["email"] != "ana@example.invalid" {
		t.Fatalf("actingUser = %+v", rec.body["actingUser"])
	}
	groups, ok := acting["groups"].([]interface{})
	if !ok || len(groups) != 1 || groups[0] != "varroa-payments" {
		t.Fatalf("groups = %+v", acting["groups"])
	}
}

// TestAbortInputPostsTheRequestedBody asserts the abort method posts the
// acting user to the plugin's own route, and carries no parameters.
func TestAbortInputPostsTheRequestedBody(t *testing.T) {
	srv, rec := newInputRouteServer(t, http.StatusAccepted)
	c := NewClient(srv.URL, "user", "token")
	status, err := c.AbortInput(context.Background(), AbortInputRequest{
		RunID:       "0199d0b7-0000-7000-8000-000000000002",
		JobPath:     "team/job",
		BuildNumber: 8,
		InputID:     "input-2",
		ActingUser:  ActingUser{Name: "bo"},
	})
	if err != nil {
		t.Fatalf("AbortInput: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if rec.path != "/varroa-run/abort-input" {
		t.Fatalf("path = %q, want /varroa-run/abort-input", rec.path)
	}
	if rec.body["inputId"] != "input-2" || rec.body["buildNumber"] != float64(8) {
		t.Fatalf("body = %+v", rec.body)
	}
	if _, ok := rec.body["parameters"]; ok {
		t.Fatalf("body carries a parameters key: %+v", rec.body)
	}
	acting, ok := rec.body["actingUser"].(map[string]interface{})
	if !ok || acting["name"] != "bo" {
		t.Fatalf("actingUser = %+v", rec.body["actingUser"])
	}
}

// TestInputRoutesReportTheUpstreamStatus asserts both methods report the
// upstream status beside the error, so the gateway can log it on the
// command result.
func TestInputRoutesReportTheUpstreamStatus(t *testing.T) {
	srv, _ := newInputRouteServer(t, http.StatusForbidden)
	c := NewClient(srv.URL, "user", "token")

	proceedStatus, proceedErr := c.ProceedInput(context.Background(), ProceedInputRequest{
		RunID:       "0199d0b7-0000-7000-8000-000000000003",
		JobPath:     "team/job",
		BuildNumber: 9,
		InputID:     "input-3",
		ActingUser:  ActingUser{Name: "ana"},
	})
	if proceedErr == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if proceedStatus != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", proceedStatus, http.StatusForbidden)
	}

	abortStatus, abortErr := c.AbortInput(context.Background(), AbortInputRequest{
		RunID:       "0199d0b7-0000-7000-8000-000000000004",
		JobPath:     "team/job",
		BuildNumber: 10,
		InputID:     "input-4",
		ActingUser:  ActingUser{Name: "ana"},
	})
	if abortErr == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if abortStatus != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", abortStatus, http.StatusForbidden)
	}
}

// TestRestartBuildPostsTheRequestedBody asserts the restart method posts the
// source run, the source build number, the stage, and the acting user to the
// plugin's restart route, and reports the upstream status beside the error.
func TestRestartBuildPostsTheRequestedBody(t *testing.T) {
	srv, rec := newInputRouteServer(t, http.StatusAccepted)
	c := NewClient(srv.URL, "user", "token")
	status, err := c.RestartBuild(context.Background(), RestartBuildRequest{
		RunID:             "0199d0b7-0000-7000-8000-000000000005",
		IntentKey:         "restart:0199d0b7-0000-7000-8000-000000000001:0199d0b7-0000-7000-8000-000000000005",
		JobPath:           "team/job",
		SourceRunID:       "0199d0b7-0000-7000-8000-000000000001",
		SourceBuildNumber: 7,
		Stage:             "deploy",
		ActingUser:        &ActingUser{Name: "ana"},
	})
	if err != nil {
		t.Fatalf("RestartBuild: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if rec.path != "/varroa-run/restart" {
		t.Fatalf("path = %q, want /varroa-run/restart", rec.path)
	}
	if rec.body["stage"] != "deploy" || rec.body["sourceRunId"] != "0199d0b7-0000-7000-8000-000000000001" ||
		rec.body["sourceBuildNumber"] != float64(7) {
		t.Fatalf("body = %+v", rec.body)
	}
}

// TestReplayBuildPostsTheRequestedBody asserts the replay method posts the
// script, the source run, the parameters, and the acting user to the
// plugin's own route, and reports the upstream status beside the error.
func TestReplayBuildPostsTheRequestedBody(t *testing.T) {
	srv, rec := newInputRouteServer(t, http.StatusAccepted)
	c := NewClient(srv.URL, "user", "token")
	status, err := c.ReplayBuild(context.Background(), ReplayBuildRequest{
		RunID:       "0199d0b7-0000-7000-8000-000000000005",
		IntentKey:   "replay:0199d0b7-0000-7000-8000-000000000001:0199d0b7-0000-7000-8000-000000000005",
		JobPath:     "team/job",
		SourceRunID: "0199d0b7-0000-7000-8000-000000000001",
		Script:      "echo 'edited'",
		Parameters:  map[string]string{"ENV": "prod"},
		ActingUser:  &ActingUser{Name: "ana", Groups: []string{"varroa-payments"}},
	})
	if err != nil {
		t.Fatalf("ReplayBuild: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
	}
	if rec.path != "/varroa-run/replay" {
		t.Fatalf("path = %q, want /varroa-run/replay", rec.path)
	}
	if rec.body["script"] != "echo 'edited'" || rec.body["sourceRunId"] != "0199d0b7-0000-7000-8000-000000000001" {
		t.Fatalf("body = %+v", rec.body)
	}
	params, ok := rec.body["parameters"].(map[string]interface{})
	if !ok || params["ENV"] != "prod" {
		t.Fatalf("parameters = %+v", rec.body["parameters"])
	}
	acting, ok := rec.body["actingUser"].(map[string]interface{})
	if !ok || acting["name"] != "ana" {
		t.Fatalf("actingUser = %+v", rec.body["actingUser"])
	}

	refusedSrv, _ := newInputRouteServer(t, http.StatusConflict)
	refused := NewClient(refusedSrv.URL, "user", "token")
	refusedStatus, refusedErr := refused.ReplayBuild(context.Background(), ReplayBuildRequest{JobPath: "team/job"})
	if refusedErr == nil || refusedStatus != http.StatusConflict {
		t.Fatalf("status = %d, err = %v; want 409 and an error", refusedStatus, refusedErr)
	}
}

// TestRunCommandRoutesReportTheTitleOfAnHTMLRefusal asserts every run
// command route reports the title of a Jenkins HTML error page, not the
// page itself.
func TestRunCommandRoutesReportTheTitleOfAnHTMLRefusal(t *testing.T) {
	const message = "Not Found: no build or queue item carries run id r1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(jenkinsErrorPage(message + " - Jenkins")))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "user", "token")
	ctx := context.Background()

	calls := map[string]func() (int, error){
		"trigger": func() (int, error) { return c.TriggerBuild(ctx, TriggerBuildRequest{JobPath: "team/job"}) },
		"cancel":  func() (int, error) { return c.CancelBuild(ctx, CancelBuildRequest{JobPath: "team/job"}) },
		"proceed": func() (int, error) { return c.ProceedInput(ctx, ProceedInputRequest{JobPath: "team/job"}) },
		"abort":   func() (int, error) { return c.AbortInput(ctx, AbortInputRequest{JobPath: "team/job"}) },
		"replay":  func() (int, error) { return c.ReplayBuild(ctx, ReplayBuildRequest{JobPath: "team/job"}) },
	}
	for name, call := range calls {
		status, err := call()
		if err == nil || status != http.StatusNotFound {
			t.Fatalf("%s: status = %d, err = %v; want 404 and an error", name, status, err)
		}
		msg := err.Error()
		if !strings.HasSuffix(msg, ": HTTP 404: "+message) {
			t.Fatalf("%s: error = %q, want the page title only", name, msg)
		}
		if strings.Contains(msg, "<") {
			t.Fatalf("%s: error carries HTML: %q", name, msg)
		}
	}
}

// TestRunCommandRoutesReportAPlainTextRefusal asserts every run command
// route reports the whole plain-text body the plugin refuses with.
func TestRunCommandRoutesReportAPlainTextRefusal(t *testing.T) {
	const message = "Conflict: the input is already settled"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(message))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "user", "token")
	ctx := context.Background()

	calls := map[string]func() (int, error){
		"trigger": func() (int, error) { return c.TriggerBuild(ctx, TriggerBuildRequest{JobPath: "team/job"}) },
		"cancel":  func() (int, error) { return c.CancelBuild(ctx, CancelBuildRequest{JobPath: "team/job"}) },
		"proceed": func() (int, error) { return c.ProceedInput(ctx, ProceedInputRequest{JobPath: "team/job"}) },
		"abort":   func() (int, error) { return c.AbortInput(ctx, AbortInputRequest{JobPath: "team/job"}) },
		"replay":  func() (int, error) { return c.ReplayBuild(ctx, ReplayBuildRequest{JobPath: "team/job"}) },
	}
	for name, call := range calls {
		status, err := call()
		if err == nil || status != http.StatusConflict {
			t.Fatalf("%s: status = %d, err = %v; want 409 and an error", name, status, err)
		}
		if msg := err.Error(); !strings.HasSuffix(msg, ": HTTP 409: "+message) {
			t.Fatalf("%s: error = %q, want the plain-text body", name, msg)
		}
	}
}
