package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// ---------------------------------------------------------------------------
// splitTenantJob / parseParamFlags / looksLikeRunID unit tests
// ---------------------------------------------------------------------------

func TestSplitTenantJob(t *testing.T) {
	tenant, job, err := splitTenantJob("payments/checkout-api/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "payments" || job != "checkout-api/main" {
		t.Errorf("expected tenant=payments job=checkout-api/main, got tenant=%s job=%s", tenant, job)
	}
}

func TestSplitTenantJob_NoSlash(t *testing.T) {
	if _, _, err := splitTenantJob("payments"); err == nil {
		t.Fatal("expected error for argument with no slash")
	}
}

func TestLooksLikeRunID(t *testing.T) {
	v7 := uuid.Must(uuid.NewV7()).String()
	if !looksLikeRunID(v7) {
		t.Errorf("expected %q to look like a run id", v7)
	}
	if looksLikeRunID("team-pay/ci") {
		t.Error("expected a controller argument not to look like a run id")
	}
	if looksLikeRunID("ci") {
		t.Error("expected a bare controller name not to look like a run id")
	}
}

// ---------------------------------------------------------------------------
// run <tenant>/<job>
// ---------------------------------------------------------------------------

func TestPipelinesRun_PrintsRunID(t *testing.T) {
	testSetup(t)
	var gotBody map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.EscapedPath(), "/api/v1/pipelines/payments/jobs/checkout-api%2Fmain/runs") {
			t.Errorf("expected job path percent-encoded, got %s", r.URL.EscapedPath())
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("failed to decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runId":     "0192f0a1-1c2d-7abc-9def-0123456789ab",
			"intentKey": "manual:0192f0a1-1c2d-7abc-9def-0123456789ab",
			"state":     "requested",
		})
	})
	defer srv.Close()

	code := run([]string{"run", "payments/checkout-api/main", "--param", "ENVIRONMENT=staging", "--server", srv.URL})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	tokenStr, _ := gotBody["clientToken"].(string)
	token, err := uuid.Parse(tokenStr)
	if err != nil {
		t.Fatalf("expected clientToken to be a UUID, got %q: %v", tokenStr, err)
	}
	if token.Version() != 7 {
		t.Errorf("expected clientToken to be a UUIDv7, got version %d", token.Version())
	}

	params, _ := gotBody["parameters"].(map[string]any)
	if params["ENVIRONMENT"] != "staging" {
		t.Errorf("expected parameters.ENVIRONMENT=staging, got %v", params)
	}
}

func TestPipelinesRun_ParamRepeatedKeyExitsUsage(t *testing.T) {
	testSetup(t)
	called := false
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})
	defer srv.Close()

	code := run([]string{"run", "payments/checkout-api", "--param", "A=1", "--param", "A=2", "--server", srv.URL})
	if code != 2 {
		t.Errorf("expected exit code 2 for a repeated --param key, got %d", code)
	}
	if called {
		t.Error("expected no request for a usage error caught before the client call")
	}
}

func TestPipelinesRun_AccessDenied(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "caller lacks pipelines:write on this tenant"})
	})
	defer srv.Close()

	code := run([]string{"run", "payments/checkout-api", "--server", srv.URL})
	if code == 0 {
		t.Fatal("expected non-zero exit for a 403 response")
	}
}

// ---------------------------------------------------------------------------
// builds <tenant>/<job>
// ---------------------------------------------------------------------------

func TestPipelinesBuilds_JobPathEncodedAndTable(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.EscapedPath(), "/api/v1/pipelines/payments/jobs/checkout-api%2Fmain/runs") {
			t.Errorf("expected the job path percent-encoded in the request path, got %s", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"runId":       "0192f0a1-1c2d-7abc-9def-0123456789ab",
					"buildNumber": 42,
					"status":      "COMPLETED",
					"result":      "SUCCESS",
					"branch":      "main",
					"durationMs":  65000,
					"queuedAt":    time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
				},
			},
		})
	})
	defer srv.Close()

	code := run([]string{"builds", "payments/checkout-api/main", "--server", srv.URL})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestBuildSummaryRow(t *testing.T) {
	build := int64(42)
	result := "SUCCESS"
	branch := "main"
	durationMs := int64(65000)
	queuedAt := time.Now().Add(-90 * time.Second)

	row := buildSummaryRow(client.ComponentsSchemasRunSummary{
		Branch:      &branch,
		BuildNumber: &build,
		DurationMs:  &durationMs,
		QueuedAt:    &queuedAt,
		Result:      &result,
		RunId:       "0192f0a1-1c2d-7abc-9def-0123456789ab",
		Status:      "COMPLETED",
	})

	if row[0] != "#42" {
		t.Errorf("expected BUILD column #42, got %q", row[0])
	}
	if row[1] != "SUCCESS" {
		t.Errorf("expected RESULT column SUCCESS, got %q", row[1])
	}
	if row[2] != "main" {
		t.Errorf("expected BRANCH column main, got %q", row[2])
	}
	if row[3] != "1m5s" {
		t.Errorf("expected DURATION column 1m5s, got %q", row[3])
	}
	if row[4] != "1m" {
		t.Errorf("expected AGE column 1m, got %q", row[4])
	}
}

// ---------------------------------------------------------------------------
// logs: run vs controller dispatch
// ---------------------------------------------------------------------------

func TestLogsCommand_DispatchesRunVsController(t *testing.T) {
	testSetup(t)
	runID := uuid.Must(uuid.NewV7()).String()

	var gotPaths []string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "/runs/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprint(w, "log line\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		}
	})
	defer srv.Close()

	if code := run([]string{"logs", runID, "--server", srv.URL}); code != 0 {
		t.Fatalf("expected exit 0 for the run form, got %d", code)
	}
	if code := run([]string{"logs", "team-pay/ci", "--server", srv.URL}); code != 0 {
		t.Fatalf("expected exit 0 for the controller form, got %d", code)
	}

	if len(gotPaths) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(gotPaths), gotPaths)
	}
	if !strings.Contains(gotPaths[0], "/api/v1/runs/"+runID+"/log") {
		t.Errorf("expected the run path to hit /runs/%s/log, got %s", runID, gotPaths[0])
	}
	if !strings.Contains(gotPaths[1], "/api/v1/clusters/core/controllers/team-pay/ci/logs") {
		t.Errorf("expected the controller path to hit the controller logs route, got %s", gotPaths[1])
	}
}

func TestLogsCommand_ExactlyOneInTree(t *testing.T) {
	root := newRootCmd()
	count := 0
	for _, c := range root.Commands() {
		if c.Name() == "logs" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one 'logs' command, found %d", count)
	}
}

// ---------------------------------------------------------------------------
// --follow / -f: exit code on run result
// ---------------------------------------------------------------------------

func sseRunLogHandler(result string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: line\ndata: {\"text\":\"building\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: end\ndata: {\"result\":%q,\"logComplete\":true}\n\n", result)
		flusher.Flush()
		<-r.Context().Done()
	}
}

func TestLogsFollowRun_ExitNonZeroOnFailure(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	runID := uuid.Must(uuid.NewV7()).String()
	srv := testServer(t, sseRunLogHandler("FAILURE"))
	defer srv.Close()

	code := run([]string{"logs", runID, "-f", "--server", srv.URL})
	if code == 0 {
		t.Fatal("expected non-zero exit for a run that finished with FAILURE")
	}
}

func TestLogsFollowRun_ExitZeroOnSuccess(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	runID := uuid.Must(uuid.NewV7()).String()
	srv := testServer(t, sseRunLogHandler("SUCCESS"))
	defer srv.Close()

	code := run([]string{"logs", runID, "-f", "--server", srv.URL})
	if code != 0 {
		t.Fatalf("expected exit 0 for a run that finished with SUCCESS, got %d", code)
	}
}

// TestPipelinesRunFollow_EndToEnd drives `run --follow` through create, the
// run-appears poll, and the log stream, asserting the exit code follows the
// run's result (varroa-cli: "Following exits on the build result").
func TestPipelinesRunFollow_EndToEnd(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)

	origInterval, origTimeout := waitForRunPollInterval, waitForRunTimeout
	waitForRunPollInterval = 5 * time.Millisecond
	waitForRunTimeout = time.Second
	t.Cleanup(func() {
		waitForRunPollInterval, waitForRunTimeout = origInterval, origTimeout
	})

	runID := "0192f0a1-1c2d-7abc-9def-0123456789ab"

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runId": runID, "intentKey": "manual:" + runID, "state": "requested",
			})
		case strings.HasSuffix(r.URL.Path, "/api/v1/runs/"+runID):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"runId": runID, "jobPath": "checkout-api", "status": "COMPLETED"})
		case strings.Contains(r.URL.Path, "/log"):
			sseRunLogHandler("FAILURE")(w, r)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	code := run([]string{"run", "payments/checkout-api", "--follow", "--server", srv.URL})
	if code == 0 {
		t.Fatal("expected non-zero exit: the followed run finished with FAILURE")
	}
}

// TestLogsFollowRun_ReconnectResumesWithoutRepeat drops the stream after
// two lines. The reconnect must carry the resume cursor, and a line the
// server sends again must not print twice.
func TestLogsFollowRun_ReconnectResumesWithoutRepeat(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	runID := uuid.Must(uuid.NewV7()).String()

	var requests atomic.Int32
	queries := make(chan string, 4)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if requests.Add(1) == 1 {
			_, _ = fmt.Fprintf(w, "event: line\ndata: {\"n\":1,\"off\":0,\"text\":\"alpha\"}\n\n")
			_, _ = fmt.Fprintf(w, "event: line\ndata: {\"n\":2,\"off\":6,\"text\":\"beta\"}\n\n")
			flusher.Flush()
			return
		}
		_, _ = fmt.Fprintf(w, "event: line\ndata: {\"n\":2,\"off\":6,\"text\":\"beta\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: gap\ndata: {\"seq\":3,\"off\":0}\n\n")
		_, _ = fmt.Fprintf(w, "event: line\ndata: {\"n\":3,\"off\":11,\"text\":\"gamma\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: end\ndata: {\"result\":\"SUCCESS\",\"logComplete\":true}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	code := run([]string{"logs", runID, "-f", "--server", srv.URL})
	_ = pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if got, want := string(out), "alpha\nbeta\ngamma\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	close(queries)
	var got []string
	for q := range queries {
		got = append(got, q)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(got), got)
	}
	if got[0] != "" {
		t.Errorf("first request must carry no cursor, got %q", got[0])
	}
	if got[1] != "from=7&fromLine=3" {
		t.Errorf("reconnect query = %q, want from=7&fromLine=3", got[1])
	}
}

func TestRunLogWatermark_SkipsReplayedLineAtOrBelowWatermark(t *testing.T) {
	var mark runLogWatermark
	if got := mark.resumeURL("http://bff/api/v1/runs/r/log"); got != "" {
		t.Errorf("resumeURL before any line = %q, want empty", got)
	}
	if !mark.accept(runLogLineEvent{N: 1, Off: 0}) {
		t.Fatal("first line at offset 0 must print")
	}
	if !mark.accept(runLogLineEvent{N: 2, Off: 6}) {
		t.Fatal("a line past the watermark must print")
	}
	if mark.accept(runLogLineEvent{N: 2, Off: 6}) || mark.accept(runLogLineEvent{N: 1, Off: 0}) {
		t.Error("a line at or below the watermark must not print")
	}
	if got, want := mark.resumeURL("http://bff/api/v1/runs/r/log"), "http://bff/api/v1/runs/r/log?from=7&fromLine=3"; got != want {
		t.Errorf("resumeURL = %q, want %q", got, want)
	}
}
