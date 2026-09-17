package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// --- 9.2: checkResumePathBinding ---

// TestCheckResumePathBinding_MismatchFails proves a shape-valid path
// still fails the command when it names another job or another build.
// A node path carries no job, so it takes no binding check.
func TestCheckResumePathBinding_MismatchFails(t *testing.T) {
	payload := &mitev1.ResumeBuildPayload{JobPath: "team/pipeline", BuildNumber: 7}
	cases := []struct {
		name    string
		parsed  parsedRehydrationPath
		wantErr bool
	}{
		{
			name:   "matching job and build passes",
			parsed: parsedRehydrationPath{kind: rehydrationPathBuildXML, jobPath: "team/pipeline", buildNumber: 7},
		},
		{
			name:    "mismatched job path fails",
			parsed:  parsedRehydrationPath{kind: rehydrationPathBuildXML, jobPath: "team/other", buildNumber: 7},
			wantErr: true,
		},
		{
			name:    "mismatched build number fails",
			parsed:  parsedRehydrationPath{kind: rehydrationPathProgramDat, jobPath: "team/pipeline", buildNumber: 8},
			wantErr: true,
		},
		{
			name:   "node config carries no binding",
			parsed: parsedRehydrationPath{kind: rehydrationPathNodeConfig, nodeName: "agent-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResumePathBinding(tc.parsed, payload)
			if tc.wantErr && err == nil {
				t.Error("checkResumePathBinding: expected an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkResumePathBinding: unexpected error: %v", err)
			}
		})
	}
}

// runStateStreamResolvingFetch builds a stream whose Send answers every
// run_state_request with one chunk holding data, correlated by the
// request's own request_id. It matches the shape fetchRunStateFile
// expects: one response whose Total equals 1.
func runStateStreamResolvingFetch(t *testing.T, a *Agent, data []byte) *concurrentSendStream {
	t.Helper()
	gz := gzipBytes(t, data)
	return &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		req := m.GetRunStateRequest()
		if req == nil {
			return nil
		}
		a.runStateWaiters.resolve(&mitev1.RunStateResponse{
			RequestId: req.RequestId, Relpath: req.Relpath,
			Seq: 0, Total: 1, GzipContent: gz, RawSize: int64(len(data)),
		})
		return nil
	}}
}

// runStateStreamFailingFetch builds a stream whose Send answers every
// run_state_request with an error response, so fetchRunStateFile always
// fails.
func runStateStreamFailingFetch(a *Agent, reason string) *concurrentSendStream {
	return &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		req := m.GetRunStateRequest()
		if req == nil {
			return nil
		}
		a.runStateWaiters.resolve(&mitev1.RunStateResponse{
			RequestId: req.RequestId, Relpath: req.Relpath, Error: reason,
		})
		return nil
	}}
}

// startAdoptAndReloadStub starts a stub Jenkins on localhost:8080 (the
// adopt-node route is hard-coded there, matching every other loopback
// route's own test in this package) serving the crumb issuer as
// disabled, the adopt-node route, and one job's reload route. It skips
// the test when the port is unavailable.
func startAdoptAndReloadStub(t *testing.T) *resumeStub {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skip("skipping: localhost:8080 not available:", err)
	}
	stub := &resumeStub{loadStatus: http.StatusOK, loadBuilding: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/crumbIssuer/api/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc(varroaAdoptNodePath, func(w http.ResponseWriter, r *http.Request) {
		var body adoptNodeRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		stub.adoptedName.Store(body.Name)
		stub.note("adopt")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/job/"+strings.ReplaceAll(stubJobPath, "/", "/job/")+"/reload", func(w http.ResponseWriter, r *http.Request) {
		stub.reloadHit.Store(true)
		stub.note("reload")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(varroaLoadBuildPath, func(w http.ResponseWriter, r *http.Request) {
		var body loadBuildRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Probe {
			stub.note("probe")
			if stub.takeProbeFailure() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			running, finished := stub.probeAnswer()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(loadBuildAnswer{
				Building: running, Registered: running, Finished: finished,
			})
			return
		}
		stub.loadBody.Store(body)
		stub.note("load")
		status, building := stub.loadAnswer()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(loadBuildAnswer{Building: building, Registered: building})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	time.Sleep(10 * time.Millisecond)
	return stub
}

// stubJobPath is the one job every resume test drives, and the job the
// stub serves a reload for.
const stubJobPath = "team/pipeline"

// resumeStub records what the resume called on the Jenkins side, and
// scripts the load route's answer.
type resumeStub struct {
	adoptedName atomic.Value
	reloadHit   atomic.Bool
	loadBody    atomic.Value

	mu            sync.Mutex
	calls         []string
	loadStatus    int
	loadBuilding  bool
	probeBuilding bool
	probeFinished bool
	probeFailures int
}

// note records one call, so a test can assert the order of the steps.
func (s *resumeStub) note(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

// callOrder answers the calls the resume made, in order.
func (s *resumeStub) callOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// answerLoad scripts what the load route answers.
func (s *resumeStub) answerLoad(status int, building bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadStatus = status
	s.loadBuilding = building
}

func (s *resumeStub) loadAnswer() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadStatus, s.loadBuilding
}

// answerProbe scripts what the probe reports. A resume of a build that
// already runs, or of one that already ended, must touch nothing.
func (s *resumeStub) answerProbe(running, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeBuilding = running
	s.probeFinished = finished
}

// failProbes makes the next count probes answer a server error.
func (s *resumeStub) failProbes(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeFailures = count
}

func (s *resumeStub) takeProbeFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probeFailures <= 0 {
		return false
	}
	s.probeFailures--
	return true
}

func (s *resumeStub) probeAnswer() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probeBuilding, s.probeFinished
}

// TestApplyResumeBuild_ExistingNodeConfigNotOverwrittenButAdopted proves
// the node path's one rule: an existing nodes/<name>/config.xml is never
// overwritten, and the mite still adopts the node the disk already
// holds.
func TestApplyResumeBuild_ExistingNodeConfigNotOverwrittenButAdopted(t *testing.T) {
	const jobPath = stubJobPath
	stub := startAdoptAndReloadStub(t)

	home := t.TempDir()
	nodeDir := filepath.Join(home, "nodes", "agent-1")
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	nodeConfigPath := filepath.Join(nodeDir, "config.xml")
	if err := os.WriteFile(nodeConfigPath, []byte("<original-node/>"), 0644); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = home
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.stream = runStateStreamResolvingFetch(t, agent, []byte("<from-gateway-node/>"))

	payload := &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: jobPath, BuildNumber: 7,
		Files: []string{"nodes/agent-1/config.xml"}, Instance: agent.cfg.CellInstance,
	}
	if err := agent.applyResumeBuild(context.Background(), payload); err != nil {
		t.Fatalf("applyResumeBuild: %v", err)
	}

	got, err := os.ReadFile(nodeConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<original-node/>" {
		t.Errorf("existing node config.xml was overwritten: got %q", got)
	}
	if name, _ := stub.adoptedName.Load().(string); name != "agent-1" {
		t.Errorf("adopt-node was called with name %q, want agent-1", name)
	}
	if !stub.reloadHit.Load() {
		t.Error("expected the job to be reloaded after the resume")
	}
}

// TestApplyResumeBuild_FetchErrorFailsBeforeReload proves a fetch
// failure fails the whole command, so a half-written build is never
// reloaded.
func TestApplyResumeBuild_FetchErrorFailsBeforeReload(t *testing.T) {
	const jobPath = stubJobPath
	stub := startAdoptAndReloadStub(t)

	agent := NewAgent(Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = t.TempDir()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.stream = runStateStreamFailingFetch(agent, "the gateway cannot read the stored file")

	payload := &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: jobPath, BuildNumber: 7,
		Files: []string{"jobs/team/jobs/pipeline/builds/7/program.dat"}, Instance: agent.cfg.CellInstance,
	}
	err := agent.applyResumeBuild(context.Background(), payload)
	if err == nil {
		t.Fatal("applyResumeBuild: expected an error from the fetch failure")
	}
	if !strings.Contains(err.Error(), "fetch") {
		t.Errorf("applyResumeBuild error = %v, want it to name the fetch failure", err)
	}
	if strings.Contains(err.Error(), "reload") {
		t.Errorf("applyResumeBuild error mentions reload, want a fetch-stage failure: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if stub.reloadHit.Load() {
		t.Error("applyResumeBuild reloaded the job after a fetch failure, want no reload")
	}
}

// --- Pre-flight round 1: the resume fence ---

// TestApplyResumeBuild_ForeignInstanceIsRefused proves the mite refuses
// a command the recorder assigned to another pod, before it writes any
// file. A pod that rolls after the recorder chose it holds the same
// controller name and a different instance.
func TestApplyResumeBuild_ForeignInstanceIsRefused(t *testing.T) {
	home := t.TempDir()
	agent := &Agent{jenkinsHomeDir: home, Logger: slog.Default()}
	agent.cfg.CellInstance = "pod-live"

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId:       "run-1",
		JobPath:     "team/pipeline",
		BuildNumber: 7,
		Files:       []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance:    "pod-dead",
	})
	if err == nil {
		t.Fatal("want a refusal for a command naming another instance")
	}
	if !strings.Contains(err.Error(), resumeInstanceMismatch) {
		t.Fatalf("error = %v, want one naming %s", err, resumeInstanceMismatch)
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("the refusal wrote under the Jenkins home: %v, %v", entries, readErr)
	}
}

// TestApplyResumeBuild_OwnInstanceIsAccepted proves the fence admits a
// command naming this mite's own instance. The command still fails on
// the fetch, because this test opens no stream, and that failure proves
// the fence let it through.
func TestApplyResumeBuild_OwnInstanceIsAccepted(t *testing.T) {
	agent := &Agent{
		jenkinsHomeDir:  t.TempDir(),
		Logger:          slog.Default(),
		runStateWaiters: newRunStateWaiterMap(),
	}
	agent.cfg.CellInstance = "pod-live"

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId:       "run-1",
		JobPath:     "team/pipeline",
		BuildNumber: 7,
		Files:       []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance:    "pod-live",
	})
	if err == nil {
		t.Fatal("want the fetch to fail with no stream open")
	}
	if strings.Contains(err.Error(), resumeInstanceMismatch) {
		t.Fatalf("the fence refused its own instance: %v", err)
	}
}

// TestApplyResumeBuild_MissingInstanceIsRefused proves an absent
// instance fails the command. The field is required, and a command that
// names no instance fences nothing: it would restore files on whichever
// pod happens to own the controller.
func TestApplyResumeBuild_MissingInstanceIsRefused(t *testing.T) {
	home := t.TempDir()
	agent := &Agent{jenkinsHomeDir: home, Logger: slog.Default()}
	agent.cfg.CellInstance = "pod-live"

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId:       "run-1",
		JobPath:     "team/pipeline",
		BuildNumber: 7,
		Files:       []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
	})
	if err == nil {
		t.Fatal("want a refusal for a command that names no instance")
	}
	if !strings.Contains(err.Error(), resumeInstanceMismatch) {
		t.Fatalf("error = %v, want one naming %s", err, resumeInstanceMismatch)
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("the refusal wrote under the Jenkins home: %v, %v", entries, readErr)
	}
}

// --- Pre-flight round 6: the absolute expiry ---

// TestApplyResumeBuild_AnExpiredDeadlineIsRefused proves a command that
// reaches the mite after the recorder gave up restores nothing. A
// command can wait in the queue, and its execution bound cannot catch
// that wait: that window opens at pickup.
func TestApplyResumeBuild_AnExpiredDeadlineIsRefused(t *testing.T) {
	home := t.TempDir()
	agent := &Agent{jenkinsHomeDir: home, Logger: slog.Default()}
	agent.cfg.CellInstance = "pod-live"

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId:        "run-1",
		JobPath:      "team/pipeline",
		BuildNumber:  7,
		Files:        []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance:     "pod-live",
		DeadlineUnix: time.Now().Add(-time.Minute).Unix(),
	})
	if err == nil {
		t.Fatal("want a refusal for a command whose deadline passed")
	}
	if !strings.Contains(err.Error(), resumeExpired) {
		t.Fatalf("error = %v, want one naming %s", err, resumeExpired)
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("the refusal wrote under the Jenkins home: %v, %v", entries, readErr)
	}
}

// TestApplyResumeBuild_AFutureDeadlineIsAccepted proves the expiry test
// admits a command that is still useful. The command then fails on the
// fetch, because this test opens no stream, and that failure proves the
// test let it through.
func TestApplyResumeBuild_AFutureDeadlineIsAccepted(t *testing.T) {
	agent := &Agent{
		jenkinsHomeDir:  t.TempDir(),
		Logger:          slog.Default(),
		runStateWaiters: newRunStateWaiterMap(),
	}
	agent.cfg.CellInstance = "pod-live"

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId:        "run-1",
		JobPath:      "team/pipeline",
		BuildNumber:  7,
		Files:        []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance:     "pod-live",
		DeadlineUnix: time.Now().Add(5 * time.Minute).Unix(),
	})
	if err == nil {
		t.Fatal("want the fetch to fail with no stream open")
	}
	if strings.Contains(err.Error(), resumeExpired) {
		t.Fatalf("the expiry test refused a live command: %v", err)
	}
}

// --- Live finding L3: the restored build must load ---

// newResumeAgent builds an agent wired for one resume of jobPath, whose
// fetch answers data for every file.
func newResumeAgent(t *testing.T, home string, data []byte) *Agent {
	t.Helper()
	agent := NewAgent(Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = home
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.stream = runStateStreamResolvingFetch(t, agent, data)
	return agent
}

// TestApplyResumeBuild_LoadsTheBuildAfterTheReload proves the resume
// loads the restored build itself. Jenkins loads a build on demand, so
// a build directory written after the job was loaded stays invisible,
// and the resume machinery inside that load never runs.
func TestApplyResumeBuild_LoadsTheBuildAfterTheReload(t *testing.T) {
	const jobPath = stubJobPath
	stub := startAdoptAndReloadStub(t)

	agent := newResumeAgent(t, t.TempDir(), []byte("<build/>"))
	payload := &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: jobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	}
	if err := agent.applyResumeBuild(context.Background(), payload); err != nil {
		t.Fatalf("applyResumeBuild: %v", err)
	}

	order := stub.callOrder()
	if len(order) != 3 || order[0] != "probe" || order[1] != "reload" || order[2] != "load" {
		t.Fatalf("call order = %v, want the probe, the reload, then the load", order)
	}
	body, _ := stub.loadBody.Load().(loadBuildRequest)
	if body.JobPath != jobPath || body.BuildNumber != 7 {
		t.Fatalf("load-build body = %+v, want the command's own job and build", body)
	}
}

// TestApplyResumeBuild_ABuildThatDoesNotRunFailsTheCommand proves a
// build that loads but does not run fails the command. Such a resume
// restored files and started nothing, and reporting success would hide
// that.
func TestApplyResumeBuild_ABuildThatDoesNotRunFailsTheCommand(t *testing.T) {
	const jobPath = stubJobPath
	stub := startAdoptAndReloadStub(t)
	stub.answerLoad(http.StatusOK, false)

	agent := newResumeAgent(t, t.TempDir(), []byte("<build/>"))
	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: jobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	})
	if err == nil {
		t.Fatal("want a failure for a build that does not run")
	}
	if !strings.Contains(err.Error(), resumeNotBuilding) {
		t.Fatalf("error = %v, want one naming %s", err, resumeNotBuilding)
	}
}

// TestApplyResumeBuild_ALoadFailureFailsTheCommand proves a load the
// plugin refuses fails the command, naming the load.
func TestApplyResumeBuild_ALoadFailureFailsTheCommand(t *testing.T) {
	const jobPath = stubJobPath
	stub := startAdoptAndReloadStub(t)
	stub.answerLoad(http.StatusNotFound, false)

	agent := newResumeAgent(t, t.TempDir(), []byte("<build/>"))
	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: jobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	})
	if err == nil {
		t.Fatal("want a failure for a build that does not load")
	}
	if !strings.Contains(err.Error(), resumeLoadFailed) {
		t.Fatalf("error = %v, want one naming %s", err, resumeLoadFailed)
	}
}

// --- Pre-flight round 7: redelivery and the absolute deadline ---

// TestApplyResumeBuild_ARedeliveredCommandTouchesNothing proves a
// second delivery of the same resume writes no file and reloads no job.
// The transport redelivers a command whose result was lost, and
// rewriting the files of a build that already runs would corrupt it.
func TestApplyResumeBuild_ARedeliveredCommandTouchesNothing(t *testing.T) {
	stub := startAdoptAndReloadStub(t)
	stub.answerProbe(true, false)

	home := t.TempDir()
	agent := NewAgent(Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = home
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.stream = runStateStreamFailingFetch(agent, "the gateway must not be asked")

	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: stubJobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	})
	if err != nil {
		t.Fatalf("a redelivered resume must answer success: %v", err)
	}
	if order := stub.callOrder(); len(order) != 1 || order[0] != "probe" {
		t.Fatalf("call order = %v, want the probe alone", order)
	}
	if stub.reloadHit.Load() {
		t.Error("a redelivered resume reloaded the job")
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("a redelivered resume wrote under the Jenkins home: %v, %v", entries, readErr)
	}
}

// TestApplyResumeBuild_TheAbsoluteDeadlineBoundsEveryStep proves the
// deadline bounds the work, not only the first test. A command picked up
// just before that moment would otherwise fetch, reload, and load past
// the recorder's own mark.
func TestApplyResumeBuild_TheAbsoluteDeadlineBoundsEveryStep(t *testing.T) {
	stub := startAdoptAndReloadStub(t)

	agent := NewAgent(Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = t.TempDir()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	// The gateway never answers, so only the deadline ends the fetch.
	agent.stream = &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error { return nil }}

	started := time.Now()
	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: stubJobPath, BuildNumber: 7,
		Files:        []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance:     agent.cfg.CellInstance,
		DeadlineUnix: time.Now().Add(time.Second).Unix(),
	})
	if err == nil {
		t.Fatal("want a failure when the deadline passes mid-way")
	}
	if !strings.Contains(err.Error(), resumeExpired) {
		t.Fatalf("error = %v, want one naming %s", err, resumeExpired)
	}
	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("the resume ran %v, want the deadline to end it", took)
	}
	if stub.reloadHit.Load() {
		t.Error("an expired resume reloaded the job")
	}
}

// --- Pre-flight review round 8: the probe fails closed ---

// TestApplyResumeBuild_AProbeThatNeverAnswersFailsClosed proves the
// command refuses rather than write when it cannot learn whether the
// build already runs. A write over a running build costs more than a
// resume that does not happen, and the arm window bounds that loss.
func TestApplyResumeBuild_AProbeThatNeverAnswersFailsClosed(t *testing.T) {
	stub := startAdoptAndReloadStub(t)
	stub.failProbes(resumeProbeAttempts)

	home := t.TempDir()
	agent := newResumeAgent(t, home, []byte("<build/>"))
	err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: stubJobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	})
	if err == nil {
		t.Fatal("want a refusal when the probe cannot answer")
	}
	if !strings.Contains(err.Error(), resumeProbeFailed) {
		t.Fatalf("error = %v, want one naming %s", err, resumeProbeFailed)
	}
	if stub.reloadHit.Load() {
		t.Error("a refused resume reloaded the job")
	}
	entries, readErr := os.ReadDir(home)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("a refused resume wrote under the Jenkins home: %v, %v", entries, readErr)
	}
}

// TestApplyResumeBuild_ATransientProbeFailureStillResumes proves one
// refused probe does not cost the run its rescue. The probe retries
// inside the command's own deadline.
func TestApplyResumeBuild_ATransientProbeFailureStillResumes(t *testing.T) {
	stub := startAdoptAndReloadStub(t)
	stub.failProbes(1)

	agent := newResumeAgent(t, t.TempDir(), []byte("<build/>"))
	if err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
		RunId: "run-1", JobPath: stubJobPath, BuildNumber: 7,
		Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
		Instance: agent.cfg.CellInstance,
	}); err != nil {
		t.Fatalf("applyResumeBuild: %v", err)
	}
	if !stub.reloadHit.Load() {
		t.Error("the resume stopped at a probe it could retry")
	}
}

// TestApplyResumeBuild_AFinishedBuildIsNotRestored proves a build that
// ended between two deliveries of the same command is left alone.
// Restoring its files would start the same build again under the same
// number.
func TestApplyResumeBuild_AFinishedBuildIsNotRestored(t *testing.T) {
	cases := []struct {
		name     string
		finished string
		probe    bool
	}{
		{name: "the flow storage is marked complete", finished: "workflow-completed"},
		{name: "the build record carries a result", finished: "build.xml"},
		{name: "Jenkins holds the finished build", probe: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := startAdoptAndReloadStub(t)
			stub.answerProbe(false, tc.probe)

			home := t.TempDir()
			buildDir := filepath.Join(home, "jobs", "team", "jobs", "pipeline", "builds", "7")
			if err := os.MkdirAll(buildDir, 0755); err != nil {
				t.Fatal(err)
			}
			const ownRecord = "<flow-build><result>SUCCESS</result></flow-build>"
			switch tc.finished {
			case "workflow-completed":
				if err := os.MkdirAll(filepath.Join(buildDir, "workflow-completed"), 0755); err != nil {
					t.Fatal(err)
				}
			case "build.xml":
				if err := os.WriteFile(filepath.Join(buildDir, "build.xml"), []byte(ownRecord), 0644); err != nil {
					t.Fatal(err)
				}
			}

			agent := newResumeAgent(t, home, []byte("<from-the-gateway/>"))
			if err := agent.applyResumeBuild(context.Background(), &mitev1.ResumeBuildPayload{
				RunId: "run-1", JobPath: stubJobPath, BuildNumber: 7,
				Files:    []string{"jobs/team/jobs/pipeline/builds/7/build.xml"},
				Instance: agent.cfg.CellInstance,
			}); err != nil {
				t.Fatalf("a finished build must answer success: %v", err)
			}
			if stub.reloadHit.Load() {
				t.Error("a finished build was reloaded")
			}
			if order := stub.callOrder(); len(order) != 1 || order[0] != "probe" {
				t.Fatalf("call order = %v, want the probe alone", order)
			}
			if tc.finished == "build.xml" {
				got, err := os.ReadFile(filepath.Join(buildDir, "build.xml"))
				if err != nil || string(got) != ownRecord {
					t.Fatalf("the target's own build record was overwritten: %q, %v", got, err)
				}
			}
		})
	}
}
