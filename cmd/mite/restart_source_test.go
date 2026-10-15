package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestRestartSourceBuildPrefix(t *testing.T) {
	got, err := restartSourceBuildPrefix("team-a/app", 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := "jobs/team-a/jobs/app/builds/7/"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	if _, err := restartSourceBuildPrefix("team-a/app", 0); err == nil {
		t.Fatal("a build number of 0 must be refused")
	}
	if _, err := restartSourceBuildPrefix("", 7); err == nil {
		t.Fatal("an empty job path must be refused")
	}
}

func TestCheckRestartSourceRelpath(t *testing.T) {
	const prefix = "jobs/team-a/jobs/app/builds/7/"
	tests := []struct {
		name    string
		relpath string
		ok      bool
	}{
		{name: "build.xml", relpath: prefix + "build.xml", ok: true},
		{name: "workflow-completed file", relpath: prefix + "workflow-completed/1.xml", ok: true},
		{name: "libs file", relpath: prefix + "libs/lib/src/Lib.groovy", ok: true},
		{name: "another build number", relpath: "jobs/team-a/jobs/app/builds/8/build.xml"},
		{name: "node config", relpath: "nodes/x/config.xml"},
		{name: "program.dat", relpath: prefix + "program.dat"},
		{name: "bare workflow-completed dir", relpath: prefix + "workflow-completed/"},
		{name: "dot segment", relpath: prefix + "libs/../../config.xml"},
		{name: "absolute", relpath: "/" + prefix + "build.xml"},
		{name: "backslash", relpath: prefix + `libs\x`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRestartSourceRelpath(tc.relpath, prefix)
			if (err == nil) != tc.ok {
				t.Fatalf("checkRestartSourceRelpath(%q) = %v, want ok=%v", tc.relpath, err, tc.ok)
			}
		})
	}
}

// restartTestAgent builds an agent whose Jenkins client points at a test
// server and whose home is a fresh directory.
func restartTestAgent(t *testing.T, jenkinsURL, home string) *Agent {
	t.Helper()
	agent := mustNewAgent(t, Config{JenkinsURL: jenkinsURL, CellInstance: "pod-live"})
	agent.jenkinsHomeDir = home
	return agent
}

// TestRestoreRestartSourceRefusesExistingBuild proves a restore never
// overwrites a build directory that already exists.
func TestRestoreRestartSourceRefusesExistingBuild(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "jobs", "team-a", "jobs", "app", "builds", "7", "workflow-completed"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := restartTestAgent(t, "http://127.0.0.1:1", home)
	payload := &mitev1.RestartBuildPayload{
		RunId: "new", JobPath: "team-a/app", SourceRunId: "src", SourceBuildNumber: 7,
		SourceFiles: []*mitev1.RestartSourceFile{{Relpath: "jobs/team-a/jobs/app/builds/7/build.xml", Bytes: 10}},
	}
	err := agent.restoreRestartSource(context.Background(), payload)
	if err == nil || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "completed flow graph") {
		t.Fatalf("restore over a completed build = %v, want a refusal that names the completed flow graph", err)
	}
}

// TestRestoreRestartSourceRefusesPathOutsideBuild proves a copy file that
// names another build is refused before any fetch or write.
func TestRestoreRestartSourceRefusesPathOutsideBuild(t *testing.T) {
	home := t.TempDir()
	agent := restartTestAgent(t, "http://127.0.0.1:1", home)
	payload := &mitev1.RestartBuildPayload{
		RunId: "new", JobPath: "team-a/app", SourceRunId: "src", SourceBuildNumber: 7,
		SourceFiles: []*mitev1.RestartSourceFile{{Relpath: "jobs/team-a/jobs/app/builds/8/build.xml", Bytes: 10}},
	}
	if err := agent.restoreRestartSource(context.Background(), payload); err == nil {
		t.Fatal("a file of another build must refuse the restore")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused restore left %d entries under the home", len(entries))
	}
}

// TestRestartWithRestoreRetriesOnlyOnSourceMissing proves the mite restores
// and retries only when the plugin names a missing source build. Every other
// refusal, and a copy-less command, passes through after one attempt.
func TestRestartWithRestoreRetriesOnlyOnSourceMissing(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		files       []*mitev1.RestartSourceFile
		wantAttempt int
		wantStatus  int
	}{
		{name: "source missing with a refused restore does not retry", header: "source-missing",
			files:       []*mitev1.RestartSourceFile{{Relpath: "jobs/team-a/jobs/app/builds/7/build.xml", Bytes: 10}},
			wantAttempt: 1, wantStatus: http.StatusConflict},
		{name: "source missing without a copy passes through", header: "source-missing", wantAttempt: 1, wantStatus: http.StatusConflict},
		{name: "other refusal does not restore", header: "", files: []*mitev1.RestartSourceFile{
			{Relpath: "jobs/team-a/jobs/app/builds/7/build.xml", Bytes: 10}}, wantAttempt: 1, wantStatus: http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/crumbIssuer/api/json" {
					_, _ = w.Write([]byte(`{"crumbRequestField":"Jenkins-Crumb","crumb":"test"}`))
					return
				}
				attempts++
				if tc.header != "" {
					w.Header().Set("X-Varroa-Restart-Refusal", tc.header)
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte("Conflict: refused"))
			}))
			defer srv.Close()

			// The build directory holds a completed flow graph, so a restore refuses before any fetch.
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "jobs", "team-a", "jobs", "app", "builds", "7", "workflow-completed"), 0o755); err != nil {
				t.Fatal(err)
			}
			agent := restartTestAgent(t, srv.URL, home)
			payload := &mitev1.RestartBuildPayload{RunId: "new", JobPath: "team-a/app", SourceRunId: "src",
				SourceBuildNumber: 7, SourceFiles: tc.files}
			status, err := agent.restartWithRestore(context.Background(), payload, jenkins.RestartBuildRequest{
				RunID: "new", JobPath: "team-a/app", SourceRunID: "src", SourceBuildNumber: 7, Stage: "deploy",
			})
			if err == nil || status != tc.wantStatus {
				t.Fatalf("status = %d, err = %v, want %d and an error", status, err, tc.wantStatus)
			}
			if attempts != tc.wantAttempt {
				t.Fatalf("restart attempts = %d, want %d", attempts, tc.wantAttempt)
			}
		})
	}
}

// TestRestoreRestartSourceRepeatsReloadWhenCopyPresent proves a restore that
// finds its copy already in place skips the write and goes on to the reload.
// The reload answers 409 on the test server, so the error names the reload.
func TestRestoreRestartSourceRepeatsReloadWhenCopyPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			_, _ = w.Write([]byte(`{"crumbRequestField":"Jenkins-Crumb","crumb":"test"}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	home := t.TempDir()
	buildDir := filepath.Join(home, "jobs", "team-a", "jobs", "app", "builds", "7")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "build.xml"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := restartTestAgent(t, srv.URL, home)
	payload := &mitev1.RestartBuildPayload{
		RunId: "new", JobPath: "team-a/app", SourceRunId: "src", SourceBuildNumber: 7,
		SourceFiles: []*mitev1.RestartSourceFile{{Relpath: "jobs/team-a/jobs/app/builds/7/build.xml", Bytes: 10}},
	}
	err := agent.restoreRestartSource(context.Background(), payload)
	if err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("restore with the copy in place = %v, want a failure at the reload", err)
	}
}

// TestRestoreRestartSourceReplacesRehydratedStub proves a restore replaces a
// rehydration stub, a build directory with no workflow-completed/, with the
// copy. The reload answers 409 on the test server, so the restore stops there
// after the swap. The stub must not remain under a moved-aside name.
func TestRestoreRestartSourceReplacesRehydratedStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			_, _ = w.Write([]byte(`{"crumbRequestField":"Jenkins-Crumb","crumb":"test"}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	home := t.TempDir()
	buildDir := filepath.Join(home, "jobs", "team-a", "jobs", "app", "builds", "7")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "build.xml"), []byte(strings.Repeat("s", 632)), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := restartTestAgent(t, srv.URL, home)
	agent.Logger = slog.Default()
	setTestStream(agent, runStateStreamResolvingFetch(t, agent, []byte("0123456789")))
	payload := &mitev1.RestartBuildPayload{
		RunId: "new", JobPath: "team-a/app", SourceRunId: "src", SourceBuildNumber: 7,
		SourceFiles: []*mitev1.RestartSourceFile{{Relpath: "jobs/team-a/jobs/app/builds/7/build.xml", Bytes: 10}},
	}
	err := agent.restoreRestartSource(context.Background(), payload)
	if err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("restore over a stub = %v, want a failure at the reload after the swap", err)
	}
	got, err := os.ReadFile(filepath.Join(buildDir, "build.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789" {
		t.Fatalf("build.xml = %q, want the copy", got)
	}
	entries, err := os.ReadDir(filepath.Join(home, "jobs", "team-a", "jobs", "app", "builds"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".varroa-replaced-") {
			t.Fatalf("a moved-aside stub remains: %s", e.Name())
		}
	}
}

// TestFlowGraphCompletedReturnsStatErrors proves a stat failure other than a
// missing entry reaches the caller. A regular file in place of the build
// directory makes the stat fail with ENOTDIR.
func TestFlowGraphCompletedReturnsStatErrors(t *testing.T) {
	dir := t.TempDir()
	if done, err := flowGraphCompleted(filepath.Join(dir, "absent")); done || err != nil {
		t.Fatalf("a missing build directory = (%v, %v), want (false, nil)", done, err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := flowGraphCompleted(file); err == nil {
		t.Fatal("a stat failure must reach the caller")
	}
	marker := filepath.Join(dir, "build")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, "workflow-completed"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := flowGraphCompleted(marker); err == nil {
		t.Fatal("a workflow-completed entry that is not a directory must be refused")
	}
}
