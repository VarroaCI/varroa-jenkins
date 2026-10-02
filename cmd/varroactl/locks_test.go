package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLocksRelease_PostsTheBody asserts the command posts name, runId, and
// reason to /api/v1/locks/release and prints the released row.
func TestLocksRelease_PostsTheBody(t *testing.T) {
	testSetup(t)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/locks/release" {
			t.Errorf("request = %s %s, want POST /api/v1/locks/release", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"acme/deploy-prod","runId":"run-1","cluster":"core","tenant":"acme",` +
			`"jobPath":"acme/release","state":"released"}`))
	}))
	defer srv.Close()

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"locks", "release", "acme/deploy-prod", "--run", "run-1",
			"--reason", "the old pod is gone", "--server", srv.URL})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := map[string]string{"name": "acme/deploy-prod", "runId": "run-1", "reason": "the old pod is gone"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body %s = %q, want %q", k, got[k], v)
		}
	}
	if !strings.Contains(out, "released fleet lock acme/deploy-prod") {
		t.Errorf("output = %q, want the released row", out)
	}
}

// TestLocksRelease_RequiresEveryInput asserts a missing name, run, or
// reason is a usage error raised before any request.
func TestLocksRelease_RequiresEveryInput(t *testing.T) {
	cases := map[string][]string{
		"no name":      {"locks", "release", "--run", "run-1", "--reason", "x"},
		"no run":       {"locks", "release", "acme/deploy-prod", "--reason", "x"},
		"no reason":    {"locks", "release", "acme/deploy-prod", "--run", "run-1"},
		"blank reason": {"locks", "release", "acme/deploy-prod", "--run", "run-1", "--reason", " "},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			testSetup(t)
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("the server must not be called")
			}))
			defer srv.Close()
			if code := run(append(args, "--server", srv.URL)); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
		})
	}
}

// TestLocksRelease_ReportsARefusal asserts a 409 reply exits non-zero.
func TestLocksRelease_ReportsARefusal(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the lock is held; only a releasing lock can be force-released"}`))
	}))
	defer srv.Close()
	code := run([]string{"locks", "release", "acme/deploy-prod", "--run", "run-1", "--reason", "x", "--server", srv.URL})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// fleetLockServer answers want with the lock list body and fails the test
// on any other request.
func fleetLockServer(t *testing.T, want string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != want {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// lockListBody holds a held row and a waiting row of one lock name, and a
// releasing row of another.
func lockListBody() string {
	held := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	waiting := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"items":[`+
		`{"name":"acme/deploy","state":"held","runId":"run-1","tenant":"acme","cluster":"core",`+
		`"jobPath":"acme/release","acquiredAt":%q,"runLink":"/pipelines/acme/acme/release/run-1"},`+
		`{"name":"acme/deploy","state":"waiting","runId":"run-2","tenant":"acme","cluster":"member-a",`+
		`"jobPath":"acme/hotfix","waitingSince":%q},`+
		`{"name":"acme/smoke","state":"releasing","runId":"run-3","tenant":"acme","cluster":"core",`+
		`"jobPath":"acme/smoke"}]}`, held, waiting)
}

// TestLocksList_ReadsAllLocks asserts the command reads GET /locks and
// prints the table columns, with the age from each row's own state.
func TestLocksList_ReadsAllLocks(t *testing.T) {
	testSetup(t)
	srv := fleetLockServer(t, "/api/v1/locks", lockListBody())
	defer srv.Close()

	var code int
	out := captureStdout(t, func() { code = run([]string{"locks", "list", "--server", srv.URL}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("output has %d lines, want a header and 3 rows:\n%s", len(lines), out)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME STATE RUN TENANT CLUSTER JOB AGE" {
		t.Errorf("header = %v", got)
	}
	want := [][]string{
		{"acme/deploy", "held", "run-1", "acme", "core", "acme/release", "5m"},
		{"acme/deploy", "waiting", "run-2", "acme", "member-a", "acme/hotfix", "3h"},
		{"acme/smoke", "releasing", "run-3", "acme", "core", "acme/smoke", "-"},
	}
	for i, w := range want {
		if got := strings.Fields(lines[i+1]); strings.Join(got, " ") != strings.Join(w, " ") {
			t.Errorf("row %d = %v, want %v", i, got, w)
		}
	}
}

// TestLocksList_TeamReadsTeamPath asserts --team reads the team's own path.
func TestLocksList_TeamReadsTeamPath(t *testing.T) {
	testSetup(t)
	srv := fleetLockServer(t, "/api/v1/pipelines/acme/locks", lockListBody())
	defer srv.Close()

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"locks", "list", "--team", "acme", "--server", srv.URL})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "acme/smoke") {
		t.Errorf("output = %q, want the team's rows", out)
	}
}

// TestLocksList_OutputFormats asserts the name, wide, json, and yaml formats.
func TestLocksList_OutputFormats(t *testing.T) {
	cases := map[string]func(t *testing.T, out string){
		"name": func(t *testing.T, out string) {
			if got := strings.TrimSpace(out); got != "acme/deploy\nacme/deploy\nacme/smoke" {
				t.Errorf("name output = %q, want one lock name per row", got)
			}
		},
		"wide": func(t *testing.T, out string) {
			header := strings.Fields(strings.SplitN(out, "\n", 2)[0])
			if strings.Join(header, " ") != "NAME STATE RUN TENANT CLUSTER JOB AGE SINCE LINK" {
				t.Errorf("wide header = %v", header)
			}
			if !strings.Contains(out, "/pipelines/acme/acme/release/run-1") {
				t.Errorf("wide output = %q, want the run link", out)
			}
		},
		"json": func(t *testing.T, out string) {
			var rows []map[string]any
			if err := json.Unmarshal([]byte(out), &rows); err != nil {
				t.Fatalf("json output does not parse: %v\n%s", err, out)
			}
			if len(rows) != 3 || rows[1]["runId"] != "run-2" {
				t.Errorf("json rows = %v", rows)
			}
		},
		"yaml": func(t *testing.T, out string) {
			if !strings.Contains(out, "runId: run-3") {
				t.Errorf("yaml output = %q, want the run id", out)
			}
		},
	}
	for format, assert := range cases {
		t.Run(format, func(t *testing.T) {
			testSetup(t)
			srv := fleetLockServer(t, "/api/v1/locks", lockListBody())
			defer srv.Close()
			var code int
			out := captureStdout(t, func() {
				code = run([]string{"locks", "list", "-o", format, "--server", srv.URL})
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			assert(t, out)
		})
	}
}

// TestLocksList_EmptyJSONIsAList asserts an empty read prints [] in json.
func TestLocksList_EmptyJSONIsAList(t *testing.T) {
	testSetup(t)
	srv := fleetLockServer(t, "/api/v1/locks", `{"items":[]}`)
	defer srv.Close()
	out := captureStdout(t, func() { run([]string{"locks", "list", "-o", "json", "--server", srv.URL}) })
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("json output = %q, want []", out)
	}
}

// TestLocksList_ReportsRefusal asserts a 403 reply exits non-zero.
func TestLocksList_ReportsRefusal(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	if code := run([]string{"locks", "list", "--server", srv.URL}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestLocksList_RejectsArgs asserts a positional argument is a usage error.
func TestLocksList_RejectsArgs(t *testing.T) {
	testSetup(t)
	if code := run([]string{"locks", "list", "acme", "--server", "http://127.0.0.1:1"}); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
