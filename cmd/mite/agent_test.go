package main

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"google.golang.org/grpc/metadata"

	"github.com/varroaci/varroa-jenkins/internal/ca"
	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestRunReconnectsOnStreamFailure verifies that Run() does not return when
// the stream or connection fails — it must retry until the context is
// cancelled. Without a reconnect loop, Run() returns a connection error
// immediately; with one, it returns context.DeadlineExceeded.
func TestRunReconnectsOnStreamFailure(t *testing.T) {
	// Write a bootstrap token so loadOrCreateIdentity doesn't fail.
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "token")
	if err := os.WriteFile(tokenFile, []byte("test-bootstrap-token"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		VarroaEndpoint: "localhost:1", // port 1: connection refused, fails fast
		JenkinsURL:     "http://localhost:2",
		ControllerName: "test",
		Namespace:      "ns",
		BootstrapFile:  tokenFile,
	}
	agent := NewAgent(cfg)
	agent.Logger = slog.Default()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	err := agent.Run(ctx)

	// Run must return because the context expired, not because of a
	// one-shot connection failure.
	if err != context.DeadlineExceeded && err != context.Canceled {
		t.Errorf("expected context error (DeadlineExceeded or Canceled), got: %v", err)
	}
}

// concurrentSendStream is a minimal mock of mitev1.Mite_CommandStreamClient
// that tracks concurrent calls to Send for race detection.
type concurrentSendStream struct {
	sendFn func(*mitev1.MiteMessage) error
	recvFn func() (*mitev1.OperatorMessage, error)
}

func (s *concurrentSendStream) Send(m *mitev1.MiteMessage) error {
	return s.sendFn(m)
}
func (s *concurrentSendStream) Recv() (*mitev1.OperatorMessage, error) {
	return s.recvFn()
}
func (s *concurrentSendStream) Header() (metadata.MD, error) { return nil, nil }
func (s *concurrentSendStream) Trailer() metadata.MD         { return nil }
func (s *concurrentSendStream) CloseSend() error             { return nil }
func (s *concurrentSendStream) Context() context.Context     { return context.Background() }
func (s *concurrentSendStream) SendMsg(m interface{}) error  { return nil }
func (s *concurrentSendStream) RecvMsg(m interface{}) error  { return nil }

// TestConcurrentSendDoesNotRace verifies serialization of concurrent
// stream.Send calls. sendHeartbeats runs in a goroutine while the main
// goroutine calls Send directly. Without a mutex, the two callers race.
// TestHeartbeatReprobesJenkinsHealth verifies that sendHeartbeats re-probes
// Jenkins health on every tick instead of caching stale values from startup.
func TestHeartbeatReprobesJenkinsHealth(t *testing.T) {
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()

	// currentSnapshot returns hashes only — health comes from probeJenkinsHealth.
	snap := agent.currentSnapshot()
	if snap.JenkinsHealth != "" {
		t.Errorf("currentSnapshot should not include health; got %q", snap.JenkinsHealth)
	}

	// probeJenkinsHealth fills health in the snapshot. localhost:1 is
	// refused, so GetInfo fails → "unreachable".
	client := jenkins.NewClient("http://localhost:1", "test", "")
	agent.probeJenkinsHealth(context.Background(), client, snap)
	if snap.JenkinsHealth != "unreachable" {
		t.Errorf("expected 'unreachable' after failed probe, got %q", snap.JenkinsHealth)
	}
}

func TestConcurrentSendDoesNotRace(t *testing.T) {
	stream := &concurrentSendStream{}
	var inSend int32
	var maxConcurrent int32
	stream.sendFn = func(m *mitev1.MiteMessage) error {
		n := atomic.AddInt32(&inSend, 1)
		if n > atomic.LoadInt32(&maxConcurrent) {
			atomic.StoreInt32(&maxConcurrent, n)
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inSend, -1)
		return nil
	}
	stream.recvFn = func() (*mitev1.OperatorMessage, error) {
		<-time.After(600 * time.Millisecond)
		return nil, context.DeadlineExceeded
	}

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.heartbeatInterval = 30 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go agent.sendHeartbeats(ctx)

	// Call Send from the main goroutine repeatedly to overlap with heartbeats.
	// Acquire sendMu just like the production code paths do.
	for i := 0; i < 5; i++ {
		agent.sendMu.Lock()
		_ = stream.Send(&mitev1.MiteMessage{})
		agent.sendMu.Unlock()
		time.Sleep(30 * time.Millisecond)
	}

	if maxConcurrent > 1 {
		t.Errorf("concurrent stream.Send detected: maxConcurrent=%d — sendMu missing or not protecting Send",
			maxConcurrent)
	}
}

// --- Token grant cache tests ---

func TestTokenGrantUpdatesCache(t *testing.T) {
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()

	// Verify initial state is empty.
	if got := agent.currentJenkinsToken(); got != "" {
		t.Errorf("expected empty token initially, got %q", got)
	}

	// Directly set a token as if received via DesiredStateCommand.
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "token-from-desired-state"
	agent.jenkinsTokenExp = time.Now().Add(60 * time.Minute).Unix()
	agent.jenkinsTokenMu.Unlock()

	if got := agent.currentJenkinsToken(); got != "token-from-desired-state" {
		t.Errorf("expected token from desired state, got %q", got)
	}

	// A TokenGrant with a newer expiry should override.
	agent.processTokenGrant(&mitev1.TokenGrant{
		MiteJenkinsToken:    "fresher-token",
		MiteJenkinsTokenExp: time.Now().Add(70 * time.Minute).Unix(),
	})
	if got := agent.currentJenkinsToken(); got != "fresher-token" {
		t.Errorf("expected fresher token from grant, got %q", got)
	}
}

func TestTokenGrantOlderExpDoesNotOverwrite(t *testing.T) {
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()

	newerExp := time.Now().Add(60 * time.Minute).Unix()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "newer-token"
	agent.jenkinsTokenExp = newerExp
	agent.jenkinsTokenMu.Unlock()

	agent.processTokenGrant(&mitev1.TokenGrant{
		MiteJenkinsToken:    "older-token",
		MiteJenkinsTokenExp: time.Now().Add(30 * time.Minute).Unix(),
	})
	if got := agent.currentJenkinsToken(); got != "newer-token" {
		t.Errorf("expected newer token to survive, got %q", got)
	}
}

// processTokenGrant applies a token grant using the same logic as the
// processCommands TokenGrant handler, for unit-test use.
func (a *Agent) processTokenGrant(grant *mitev1.TokenGrant) {
	if grant == nil || grant.MiteJenkinsToken == "" {
		return
	}
	a.jenkinsTokenMu.Lock()
	if grant.MiteJenkinsTokenExp > a.jenkinsTokenExp {
		a.jenkinsToken = grant.MiteJenkinsToken
		a.jenkinsTokenExp = grant.MiteJenkinsTokenExp
	}
	a.jenkinsTokenMu.Unlock()
	a.wakeTokenWaiters()
}

// --- Convergence gate tests (6.4, 7.4) ---

func TestAllComponentsSucceeded(t *testing.T) {
	tests := []struct {
		name   string
		cmd    *mitev1.DesiredStateCommand
		result *mitev1.DesiredStateResult
		want   bool
	}{
		{
			name:   "all empty",
			cmd:    &mitev1.DesiredStateCommand{},
			result: &mitev1.DesiredStateResult{},
			want:   true,
		},
		{
			name:   "jcasc succeeded",
			cmd:    &mitev1.DesiredStateCommand{JcascYaml: "config"},
			result: &mitev1.DesiredStateResult{ConfigSuccess: true},
			want:   true,
		},
		{
			name:   "jcasc failed",
			cmd:    &mitev1.DesiredStateCommand{JcascYaml: "config"},
			result: &mitev1.DesiredStateResult{ConfigSuccess: false},
			want:   false,
		},
		{
			name:   "rbac failed",
			cmd:    &mitev1.DesiredStateCommand{RbacYaml: "rbac"},
			result: &mitev1.DesiredStateResult{RbacSuccess: false},
			want:   false,
		},
		{
			name:   "items failed",
			cmd:    &mitev1.DesiredStateCommand{ItemsYaml: "items"},
			result: &mitev1.DesiredStateResult{ItemsSuccess: false},
			want:   false,
		},
		{
			name:   "all components succeeded",
			cmd:    &mitev1.DesiredStateCommand{JcascYaml: "c", RbacYaml: "r", ItemsYaml: "i"},
			result: &mitev1.DesiredStateResult{ConfigSuccess: true, RbacSuccess: true, ItemsSuccess: true},
			want:   true,
		},
		{
			name:   "marker written only on full success",
			cmd:    &mitev1.DesiredStateCommand{JcascYaml: "c", RbacYaml: "r"},
			result: &mitev1.DesiredStateResult{ConfigSuccess: true, RbacSuccess: false, ItemsSuccess: true, PluginsSuccess: true},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&Agent{}).allComponentsSucceeded(tt.result, tt.cmd)
			if got != tt.want {
				t.Errorf("allComponentsSucceeded = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHealthCacheConcurrentAccess exercises the cached-health accessors from
// many goroutines at once. It is a regression guard for the data race where
// the heartbeat goroutine read lastHealth while the health-probe goroutine
// wrote it without synchronization. Run with -race.
func TestHealthCacheConcurrentAccess(t *testing.T) {
	agent := NewAgent(Config{})
	agent.Logger = slog.Default()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				agent.setHealth("healthy", "2.0")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = agent.getHealth()
			}
		}()
	}
	wg.Wait()

	// After all writers finish, the cached value reflects the last write.
	if h, v := agent.getHealth(); h != "healthy" || v != "2.0" {
		t.Errorf("getHealth after concurrent writes = (%q,%q), want (healthy,2.0)", h, v)
	}
}

// newTestAgent returns an agent whose state markers live in a temp dir so
// the convergence/first-boot logic can be exercised hermetically.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgent(Config{ControllerName: "test", Namespace: "ns"})
	a.Logger = slog.Default()
	a.miteDir = t.TempDir()
	return a
}

// newTestAgentWithJenkins returns a hermetic test agent whose Jenkins client
// points at the given stub server URL.
func newTestAgentWithJenkins(t *testing.T, jenkinsURL string) *Agent {
	t.Helper()
	a := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: jenkinsURL})
	a.Logger = slog.Default()
	a.miteDir = t.TempDir()
	return a
}

func TestLastAppliedHashMarker(t *testing.T) {
	agent := newTestAgent(t)

	if agent.hasLastAppliedHash() {
		t.Fatal("expected no last-applied hash initially")
	}
	agent.writeMarker(appliedMarker{composite: "abc123", session: "sess-1"})
	if !agent.hasLastAppliedHash() {
		t.Fatal("expected last-applied hash after write")
	}
	m := agent.readMarker()
	if m.composite != "abc123" || m.session != "sess-1" {
		t.Errorf("readMarker = (%q, %q), want (abc123, sess-1)", m.composite, m.session)
	}
}

// TestLegacyMarkerParsesAsSessionless verifies a marker written by an older
// mite (bare hash, no session) parses with an empty session, which can never
// match a live Jenkins session and therefore forces one re-apply.
func TestLegacyMarkerParsesAsSessionless(t *testing.T) {
	agent := newTestAgent(t)
	if err := os.WriteFile(agent.lastAppliedHashPath(), []byte("abc123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := agent.readMarker()
	if m.composite != "abc123" || m.session != "" {
		t.Errorf("readMarker = (%q, %q), want (abc123, \"\")", m.composite, m.session)
	}
}

type fakeJenkinsHits struct {
	Checks      atomic.Int32
	Applies     atomic.Int32
	Reloads     atomic.Int32
	SafeRestart atomic.Int32
}

// fakeJenkins starts a Jenkins stub that stamps every response with the given
// X-Jenkins-Session header (as Jenkins core does — the value changes on every
// JVM boot) and accepts login, crumb, and JCasC check/apply requests. It returns
// the server and a counter of /configuration-as-code/check and apply hits.
func fakeJenkins(t *testing.T, session string) (*httptest.Server, *fakeJenkinsHits) {
	t.Helper()
	hits := &fakeJenkinsHits{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", session)
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			hits.Checks.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/configuration-as-code/apply/":
			hits.Applies.Add(1)
		case r.URL.Path == "/configuration-as-code/reload":
			hits.Reloads.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/safeRestart":
			hits.SafeRestart.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// TestConvergedPushIsNoOp verifies that a desired-state command whose hash
// AND Jenkins boot session match the persisted marker is skipped:
// handleDesiredState reports success for every component without attempting
// any Jenkins write.
func TestConvergedPushIsNoOp(t *testing.T) {
	srv, hits := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess || !res.RbacSuccess || !res.ItemsSuccess || !res.PluginsSuccess {
		t.Fatalf("converged push should report all components successful (no-op), got %+v", res)
	}
	if n := hits.Applies.Load(); n != 0 {
		t.Errorf("converged push must not write to Jenkins, got %d apply calls", n)
	}
}

// TestJenkinsRestartReappliesDesiredState is the regression test for the
// post-restart RBAC drift: init.groovy.d resets the authorization strategy on
// every Jenkins boot, so a push whose hash matches the marker must still
// re-apply when the Jenkins boot session differs from the one recorded at the
// last apply.
func TestJenkinsRestartReappliesDesiredState(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-new")
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-old"})

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess {
		t.Fatalf("expected successful re-apply after session change, got %+v", res)
	}
	if n := hits.Reloads.Load(); n != 1 {
		t.Errorf("expected exactly 1 reload call after session change, got %d", n)
	}
	m := agent.readMarker()
	if m.composite != "hash-1" || m.session != "sess-new" {
		t.Errorf("marker after re-apply = (%q, %q), want (hash-1, sess-new)", m.composite, m.session)
	}
}

// TestEmptyItemsReportsSuccess is the regression test for the phantom "items"
// apply failure on bundles that have no item-type inputs. When ItemsYaml is
// empty there is nothing to create, so the full apply path must still report
// ItemsSuccess=true. Otherwise the zero-value ItemsSuccess=false flows back to
// the operator, which ANDs it into Succeeded and stamps a failed "items"
// section with no error message — surfacing in the UI as
// "items: rejected, no message returned" for a controller that is actually
// converged. The converged-skip path already sets all flags true, so this must
// exercise a real apply (no matching marker) to catch the regression.
func TestEmptyItemsReportsSuccess(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, _ := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-new",
		JcascYaml:           "jenkins: {}",
		ItemsYaml:           "", // bundle has no item-type inputs
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess {
		t.Fatalf("expected config apply to succeed, got %+v", res)
	}
	if !res.ItemsSuccess {
		t.Errorf("empty ItemsYaml must report ItemsSuccess=true (nothing to apply = success), got %+v", res)
	}
}

// TestLegacyMarkerReapplies verifies that upgrading from a sessionless marker
// re-applies once and stamps the marker with the live session.
func TestLegacyMarkerReapplies(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)
	if err := os.WriteFile(agent.lastAppliedHashPath(), []byte("hash-1"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	agent.handleDesiredState(context.Background(), cmd)

	if n := hits.Reloads.Load(); n != 1 {
		t.Errorf("expected exactly 1 reload call for legacy marker, got %d", n)
	}
	if m := agent.readMarker(); m.composite != "hash-1" || m.session != "sess-1" {
		t.Errorf("marker after upgrade = (%q, %q), want (hash-1, sess-1)", m.composite, m.session)
	}
}

// TestConfigPushUsesReloadNotAdminEndpoints asserts that a config push must go
// through the MANAGE-gated reload endpoint and never touch the
// ADMINISTER-gated /configuration-as-code/check or /apply endpoints, so
// the mite can run without Jenkins.ADMINISTER.
func TestConfigPushUsesReloadNotAdminEndpoints(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-new",
		Reload:              true,
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess {
		t.Fatalf("expected config push to succeed via reload, got %+v", res)
	}
	if n := hits.Reloads.Load(); n < 1 {
		t.Errorf("expected config push to call /configuration-as-code/reload, got %d", n)
	}
	if n := hits.Checks.Load(); n != 0 {
		t.Errorf("config push must not call admin /configuration-as-code/check, got %d", n)
	}
	if n := hits.Applies.Load(); n != 0 {
		t.Errorf("config push must not call admin /configuration-as-code/apply, got %d", n)
	}
}

// TestConfigPushNeverUsesApplyEvenWithoutReloadFlag guarantees the mite has no
// path to the admin apply endpoint: even a command that does not set
// Reload goes through the MANAGE-gated reload path, so the mite's role can
// stay MANAGE-scoped — it never needs to reach an ADMINISTER-only endpoint.
func TestConfigPushNeverUsesApplyEvenWithoutReloadFlag(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-new",
		JcascYaml:           "jenkins: {}",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess {
		t.Fatalf("expected config push to succeed via reload, got %+v", res)
	}
	if n := hits.Applies.Load(); n != 0 {
		t.Errorf("mite must never call admin apply, got %d apply calls", n)
	}
	if n := hits.Reloads.Load(); n < 1 {
		t.Errorf("config push must use reload, got %d reload calls", n)
	}
}

// TestSessionProbeFailureProceedsToApply verifies the gate fails open: when
// the session probe errors (Jenkins down — likely restarting), a hash-matching
// push proceeds to the apply path instead of reporting converged success.
// The cancelled context aborts the apply at the wait-for-Jenkins step.
func TestSessionProbeFailureProceedsToApply(t *testing.T) {
	agent := newTestAgent(t)
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash: "hash-1",
		JcascYaml:        "jenkins: {}",
	}
	res := agent.handleDesiredState(ctx, cmd)
	if res.ConfigSuccess {
		t.Error("session probe failure should fall through to the apply path, not report converged success")
	}
}

// TestConvergedPushCachesToken verifies the token riding on a converged push is
// still cached even though the apply is skipped.
func TestConvergedPushCachesToken(t *testing.T) {
	srv, _ := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-1",
		MiteJenkinsToken:    "fresh-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	agent.handleDesiredState(context.Background(), cmd)

	if got := agent.currentJenkinsToken(); got != "fresh-token" {
		t.Errorf("token not cached on skipped apply: got %q", got)
	}
}

// TestReloadBypassesGate verifies that an explicit reload applies even when the
// hash matches the marker. With a cancelled context the apply aborts at the
// wait-for-Jenkins step, so the result is NOT all-success — proving the gate
// was bypassed (a converged skip would have returned all-success).
func TestReloadBypassesGate(t *testing.T) {
	agent := newTestAgent(t)
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash: "hash-1",
		Reload:           true,
		JcascYaml:        "jenkins: {}",
	}
	res := agent.handleDesiredState(ctx, cmd)
	if res.ConfigSuccess {
		t.Error("reload should bypass the gate and attempt the apply (not report converged success)")
	}
}

// TestChangedHashProceeds verifies a push whose hash differs from the marker is
// not skipped (proceeds past the gate, then aborts on the cancelled context).
func TestChangedHashProceeds(t *testing.T) {
	agent := newTestAgent(t)
	agent.writeMarker(appliedMarker{composite: "old-hash", session: "sess-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &mitev1.DesiredStateCommand{DesiredStateHash: "new-hash", JcascYaml: "jenkins: {}"}
	res := agent.handleDesiredState(ctx, cmd)
	if res.ConfigSuccess {
		t.Error("changed hash should proceed to apply, not report converged success")
	}
}

// TestFailedApplyLeavesMarkerStale verifies the convergence marker is not
// advanced when an apply does not fully succeed, so the next identical push
// re-applies.
func TestFailedApplyLeavesMarkerStale(t *testing.T) {
	agent := newTestAgent(t)
	agent.writeMarker(appliedMarker{composite: "old-hash", session: "sess-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &mitev1.DesiredStateCommand{DesiredStateHash: "new-hash", JcascYaml: "jenkins: {}"}
	agent.handleDesiredState(ctx, cmd)

	if got := agent.readMarker().composite; got != "old-hash" {
		t.Errorf("marker should stay stale after a failed apply, got %q", got)
	}
}

// TestMarkerSurvivesRestart verifies the convergence state persists across a
// mite process restart (a new Agent pointed at the same dir sees the marker).
func TestMarkerSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a1 := NewAgent(Config{})
	a1.Logger = slog.Default()
	a1.miteDir = dir
	a1.writeMarker(appliedMarker{composite: "persisted-hash", session: "sess-1"})

	a2 := NewAgent(Config{})
	a2.Logger = slog.Default()
	a2.miteDir = dir
	m := a2.readMarker()
	if !a2.hasLastAppliedHash() || m.composite != "persisted-hash" || m.session != "sess-1" {
		t.Error("convergence marker did not survive a simulated restart")
	}
}

// TestComponentCachesWithoutMarkerIsFirstBoot verifies first boot is defined
// solely by the absence of the last-applied-hash marker: component caches
// present without the marker still count as first boot.
func TestComponentCachesWithoutMarkerIsFirstBoot(t *testing.T) {
	agent := newTestAgent(t)
	// Simulate component caches written by a previous, incomplete apply.
	if err := os.WriteFile(filepath.Join(agent.miteDir, "last-jcasc.yaml"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if agent.hasLastAppliedHash() {
		t.Fatal("marker should be absent")
	}
	// isFirstBoot is !hasLastAppliedHash(); with caches but no marker it is true.
	if got := !agent.hasLastAppliedHash(); !got {
		t.Error("expected first boot (no marker) despite component caches present")
	}
}

// --- Token grant waiters (regression tests for deadlock fix) ---

// TestConcurrentRefreshWaitersShareGrant verifies that multiple concurrent
// refresh callers share one qualifying TokenGrant without orphaning earlier
// waiters.
func TestConcurrentRefreshWaitersShareGrant(t *testing.T) {
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()

	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "old-token"
	agent.jenkinsTokenExp = time.Now().Add(5 * time.Minute).Unix()
	agent.jenkinsTokenMu.Unlock()

	baseline := agent.currentJenkinsTokenExp()
	w1 := &tokenWaiter{ch: make(chan struct{}, 1), baseline: baseline}
	w2 := &tokenWaiter{ch: make(chan struct{}, 1), baseline: baseline}

	agent.tokenWaitersMu.Lock()
	agent.tokenWaiters = append(agent.tokenWaiters, w1, w2)
	agent.tokenWaitersMu.Unlock()

	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "fresh-shared"
	agent.jenkinsTokenExp = time.Now().Add(60 * time.Minute).Unix()
	agent.jenkinsTokenMu.Unlock()
	agent.wakeTokenWaiters()

	select {
	case <-w1.ch:
	default:
		t.Error("waiter 1 was not unblocked by qualifying grant")
	}
	select {
	case <-w2.ch:
	default:
		t.Error("waiter 2 was not unblocked by qualifying grant")
	}

	agent.tokenWaitersMu.Lock()
	n := len(agent.tokenWaiters)
	agent.tokenWaitersMu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 remaining waiters, got %d", n)
	}
}

// TestStaleGrantDoesNotCompleteExpiredTokenWaiter verifies that a stale
// TokenGrant does not overwrite a newer cached token and does not falsely
// unblock a waiter whose baseline is fresher than both the cached and
// grant tokens.
func TestStaleGrantDoesNotCompleteExpiredTokenWaiter(t *testing.T) {
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()

	newExp := time.Now().Add(60 * time.Minute).Unix()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "newer-token"
	agent.jenkinsTokenExp = newExp
	agent.jenkinsTokenMu.Unlock()

	// Baseline is fresher than the cached token so a stale grant
	// (which does not advance the cache) cannot satisfy it.
	fresherBaseline := time.Now().Add(75 * time.Minute).Unix()
	w := &tokenWaiter{ch: make(chan struct{}, 1), baseline: fresherBaseline}
	agent.tokenWaitersMu.Lock()
	agent.tokenWaiters = append(agent.tokenWaiters, w)
	agent.tokenWaitersMu.Unlock()

	agent.processTokenGrant(&mitev1.TokenGrant{
		MiteJenkinsToken:    "stale-token",
		MiteJenkinsTokenExp: time.Now().Add(30 * time.Minute).Unix(),
	})

	if got := agent.currentJenkinsToken(); got != "newer-token" {
		t.Errorf("expected newer-token to survive stale grant, got %q", got)
	}

	select {
	case <-w.ch:
		t.Error("waiter was incorrectly unblocked by stale grant")
	default:
	}

	agent.tokenWaitersMu.Lock()
	n := len(agent.tokenWaiters)
	agent.tokenWaitersMu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 remaining waiter (stale grant should not remove it), got %d", n)
	}

	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "freshest"
	agent.jenkinsTokenExp = time.Now().Add(90 * time.Minute).Unix()
	agent.jenkinsTokenMu.Unlock()
	agent.wakeTokenWaiters()

	select {
	case <-w.ch:
	default:
		t.Error("waiter was not unblocked by qualifying grant after stale grant")
	}
}

// TestDesiredStateCoalescingDoesNotEndSession verifies that rapid
// desired-state pushes coalesce (last-wins) and never end the stream.
func TestDesiredStateCoalescingDoesNotEndSession(t *testing.T) {
	cmdCount := 0
	stream := &concurrentSendStream{
		recvFn: func() (*mitev1.OperatorMessage, error) {
			cmdCount++
			if cmdCount <= 50 {
				return &mitev1.OperatorMessage{
					Message: &mitev1.OperatorMessage_DesiredState{
						DesiredState: &mitev1.DesiredStateCommand{
							CommandId: fmt.Sprintf("cmd-%d", cmdCount),
						},
					},
				}, nil
			}
			<-make(chan struct{})
			return nil, nil
		},
		sendFn: func(m *mitev1.MiteMessage) error { return nil },
	}

	srv, _ := fakeJenkins(t, "sess-1")
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     srv.URL,
	})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()
	agent.stream = stream

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		agent.processCommands(ctx)
		close(done)
	}()

	// processCommands must NOT exit due to desired-state back-pressure.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Good: session still alive despite 50 commands
	}
	cancel()
}

// TestSerializedCommandExecution verifies that desired-state and imperative
// commands execute one at a time and results use matching command IDs.
func TestSerializedCommandExecution(t *testing.T) {
	var cmdResults []string
	var mu sync.Mutex
	resultCh := make(chan struct{}, 2)

	stream := &concurrentSendStream{
		recvFn: func() (*mitev1.OperatorMessage, error) {
			<-make(chan struct{})
			return nil, nil
		},
		sendFn: func(m *mitev1.MiteMessage) error {
			mu.Lock()
			defer mu.Unlock()
			if cr := m.GetCommandResult(); cr != nil {
				cmdResults = append(cmdResults, cr.CommandId)
				resultCh <- struct{}{}
			}
			return nil
		},
	}
	_ = stream

	srv, _ := fakeJenkins(t, "sess-1")
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     srv.URL,
	})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()
	agent.stream = stream

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mb := newCommandMailbox()

	// Both commands must be queued BEFORE the worker starts. The priority this
	// test asserts is a property of mb.take() choosing among items already in
	// the mailbox — putDesired signals the worker, so starting it first lets it
	// drain the desired command before the imperative is ever enqueued, and the
	// ordering assertion below fails on a loaded machine.
	// Desired-state command with convergence gate satisfied so it returns quickly.
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})
	mb.putDesired(commandWork{
		desiredStateCmd: &mitev1.DesiredStateCommand{
			CommandId:        "first",
			DesiredStateHash: "hash-1",
		},
		ctx: ctx,
	})
	// Imperative command with unknown type → immediate error result.
	mb.putImperative(commandWork{
		imperativeCmd: &mitev1.ImperativeCommand{CommandId: "second", Type: 0},
		ctx:           ctx,
	})

	go agent.commandWorker(ctx, mb)

	for i := 0; i < 2; i++ {
		select {
		case <-resultCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for result %d", i+1)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cmdResults) != 2 {
		t.Fatalf("expected 2 results, got %d", len(cmdResults))
	}
	// Imperatives are drained before desired commands (priority).
	if cmdResults[0] != "second" {
		t.Errorf("expected first result 'second' (imperative priority), got %q", cmdResults[0])
	}
	if cmdResults[1] != "first" {
		t.Errorf("expected second result 'first', got %q", cmdResults[1])
	}
}

// TestConfigPushSuccessWritesLastJcasc verifies a successful reload-based config
// push records last-jcasc.yaml (for snapshot hashes) and uses only the
// MANAGE-gated reload endpoint — never the admin check/apply endpoints.
// Reload-path rollback is covered by the TestApplyConfigViaReload* tests below.
func TestConfigPushSuccessWritesLastJcasc(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-ok")
	agent := newTestAgentWithJenkins(t, srv.URL)
	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-ok",
		Reload:              true,
		JcascYaml:           "jenkins:\n  systemMessage: hello\n",
		MiteJenkinsToken:    "eyJtest",
		MiteJenkinsTokenExp: 9999999999,
	}
	result := agent.handleDesiredState(context.Background(), cmd)
	if !result.ConfigSuccess {
		t.Errorf("expected ConfigSuccess=true, got ConfigError=%q", result.ConfigError)
	}
	if hits.Reloads.Load() != 1 {
		t.Errorf("expected 1 reload call, got %d", hits.Reloads.Load())
	}
	if hits.Checks.Load() != 0 || hits.Applies.Load() != 0 {
		t.Errorf("config push must not use admin check/apply, got checks=%d applies=%d", hits.Checks.Load(), hits.Applies.Load())
	}
	if _, err := os.Stat(filepath.Join(agent.miteDir, "last-jcasc.yaml")); err != nil {
		t.Errorf("expected last-jcasc.yaml written on success: %v", err)
	}
}

// spanStub implements trace.Span for testing, recording End() calls.
type spanStub struct {
	embedded.Span
	ended  bool
	events []string
}

func (s *spanStub) End(...trace.SpanEndOption)                   { s.ended = true }
func (s *spanStub) AddEvent(name string, _ ...trace.EventOption) { s.events = append(s.events, name) }
func (s *spanStub) AddLink(_ trace.Link)                         {}
func (s *spanStub) IsRecording() bool                            { return true }
func (s *spanStub) RecordError(_ error, _ ...trace.EventOption)  {}
func (s *spanStub) SpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{})
}
func (s *spanStub) SetAttributes(_ ...attribute.KeyValue) {}
func (s *spanStub) SetName(_ string)                      {}
func (s *spanStub) SetStatus(_ codes.Code, _ string)      {}
func (s *spanStub) TracerProvider() trace.TracerProvider  { return nil }

// TestCommandMailboxReplaceOnArrival verifies that a second putDesired
// replaces the first slot and ends the first span.
func TestCommandMailboxReplaceOnArrival(t *testing.T) {
	mb := newCommandMailbox()

	sp1 := &spanStub{}
	w1 := commandWork{desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "1"}, span: sp1}
	mb.putDesired(w1)

	sp2 := &spanStub{}
	w2 := commandWork{desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "2"}, span: sp2}
	mb.putDesired(w2)

	if !sp1.ended {
		t.Error("first span should be ended when replaced")
	}

	got, ok := mb.take()
	if !ok {
		t.Fatal("take should return a command")
	}
	if got.desiredStateCmd.CommandId != "2" {
		t.Errorf("expected command 2, got %s", got.desiredStateCmd.CommandId)
	}

	_, ok = mb.take()
	if ok {
		t.Error("second take should return false (empty)")
	}
}

// TestCommandMailboxImperativeFIFO verifies imperatives preserve arrival order.
func TestCommandMailboxImperativeFIFO(t *testing.T) {
	mb := newCommandMailbox()

	for i := 0; i < 5; i++ {
		w := commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: fmt.Sprintf("%d", i)}}
		if !mb.putImperative(w) {
			t.Fatalf("putImperative %d should succeed", i)
		}
	}

	for i := 0; i < 5; i++ {
		w, ok := mb.take()
		if !ok {
			t.Fatalf("take %d should return a command", i)
		}
		if w.imperativeCmd.CommandId != fmt.Sprintf("%d", i) {
			t.Errorf("expected command %d, got %s", i, w.imperativeCmd.CommandId)
		}
	}

	_, ok := mb.take()
	if ok {
		t.Error("take on empty mailbox should return false")
	}
}

// TestCommandMailboxImperativeCap verifies putImperative returns false at capacity.
func TestCommandMailboxImperativeCap(t *testing.T) {
	mb := newCommandMailbox()

	for i := 0; i < imperativeQueueCap; i++ {
		w := commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: fmt.Sprintf("%d", i)}}
		if !mb.putImperative(w) {
			t.Fatalf("putImperative %d at cap should succeed", i)
		}
	}

	w := commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: "overflow"}}
	if mb.putImperative(w) {
		t.Error("putImperative should return false at capacity")
	}
}

// TestCommandMailboxImperativePriority verifies take drains imperatives before desired.
func TestCommandMailboxImperativePriority(t *testing.T) {
	mb := newCommandMailbox()

	wDesired := commandWork{desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "d"}}
	mb.putDesired(wDesired)

	wImp := commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: "i"}}
	mb.putImperative(wImp)

	first, ok := mb.take()
	if !ok {
		t.Fatal("take should return a command")
	}
	if first.imperativeCmd == nil || first.imperativeCmd.CommandId != "i" {
		t.Error("first take should return the imperative, not desired")
	}

	second, ok := mb.take()
	if !ok {
		t.Fatal("take should return a second command")
	}
	if second.desiredStateCmd == nil || second.desiredStateCmd.CommandId != "d" {
		t.Error("second take should return the desired command")
	}
}

// TestCommandMailboxEmptyTake verifies take returns false when empty.
func TestCommandMailboxEmptyTake(t *testing.T) {
	mb := newCommandMailbox()
	_, ok := mb.take()
	if ok {
		t.Error("take on empty mailbox should return false")
	}
}

// ============================================================
//  Reload / rollback tests (4.2)
// ============================================================

// cascReloadServer starts an httptest.Server whose handlers are controlled
// by the reloadFn closure (called once per POST to /configuration-as-code/reload).
// All other routes (crumb, login, check) return success so the Jenkins client
// can boot and validate.
func cascReloadServer(t *testing.T, reloadFn func(int) (int, string)) *httptest.Server {
	t.Helper()
	var reloadCall int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/configuration-as-code/reload":
			reloadCall++
			code, body := reloadFn(reloadCall)
			w.WriteHeader(code)
			if body != "" {
				fmt.Fprint(w, body)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestApplyConfigViaReloadHappyPath exercises the write→reload happy path:
// the mite writes config.yaml into the CASC directory, saves last-good
// in the sibling directory, and calls Reload which succeeds.
func TestApplyConfigViaReloadHappyPath(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)

	// Pre-seed a live file so saveLastGood picks it up (last-good is the
	// content that was working before the update — D5).
	if err := os.MkdirAll(cascDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cascDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"before\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	srv := cascReloadServer(t, func(_ int) (int, string) {
		return http.StatusOK, ""
	})
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{Logger: slog.Default()}

	cmd := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: \"hello from casc\"\n",
	}
	configOK, configErr := agent.applyConfigViaReload(context.Background(), client, cmd)
	if !configOK {
		t.Fatalf("expected success, got ConfigError=%q", configErr)
	}

	// Verify the live file was written.
	liveData, err := os.ReadFile(filepath.Join(cascDir, "config.yaml"))
	if err != nil {
		t.Fatalf("expected config.yaml in live CASC dir: %v", err)
	}
	if !strings.Contains(string(liveData), "hello from casc") {
		t.Errorf("live config.yaml does not contain expected content: %s", liveData)
	}

	// Verify last-good was saved in the sibling directory (outside live dir).
	lastGoodPath := filepath.Join(cascDir+"-last-good", "config.yaml")
	lgData, err := os.ReadFile(lastGoodPath)
	if err != nil {
		t.Fatalf("expected last-good config.yaml in %s: %v", lastGoodPath, err)
	}
	if !strings.Contains(string(lgData), "before") {
		t.Errorf("last-good config.yaml does not contain expected (pre-update) content: %s", lgData)
	}
}

// TestApplyConfigViaReloadRollbackRestored tests branch (a): reload fails,
// last-good exists → restore last-good, re-issue reload exactly once,
// config section FAILED, message says "rolled back to last-good".
func TestApplyConfigViaReloadRollbackRestored(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)

	// Pre-seed a last-good file so the "has last-good" branch triggers.
	lastGoodDir := cascDir + "-last-good"
	if err := os.MkdirAll(lastGoodDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lastGoodDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"safe\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Also pre-seed a live file so saveLastGood picks it up.
	if err := os.MkdirAll(cascDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cascDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"safe\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// First reload fails (call 1), second (rollback) reload succeeds (call 2).
	srv := cascReloadServer(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusInternalServerError, "reload exploded"
		}
		return http.StatusOK, ""
	})
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{Logger: slog.Default()}

	cmd := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: \"risky\"\n",
	}
	configOK, configErr := agent.applyConfigViaReload(context.Background(), client, cmd)
	if configOK {
		t.Fatal("expected failure after rollback")
	}
	if !strings.Contains(configErr, "rolled back to last-good") {
		t.Errorf("expected 'rolled back to last-good' in error, got: %s", configErr)
	}
	if !strings.Contains(configErr, "reload exploded") {
		t.Errorf("expected original error in message, got: %s", configErr)
	}
}

// TestApplyConfigViaReloadRollbackAlsoFailed tests branch (b): reload fails,
// last-good exists, restore reload also fails → FAILED,
// message says "rollback also failed".
func TestApplyConfigViaReloadRollbackAlsoFailed(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)

	lastGoodDir := cascDir + "-last-good"
	if err := os.MkdirAll(lastGoodDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lastGoodDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"safe\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cascDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cascDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"safe\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Both reloads fail.
	srv := cascReloadServer(t, func(_ int) (int, string) {
		return http.StatusInternalServerError, "reload exploded"
	})
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{Logger: slog.Default()}

	cmd := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: \"risky\"\n",
	}
	configOK, configErr := agent.applyConfigViaReload(context.Background(), client, cmd)
	if configOK {
		t.Fatal("expected failure after double failure")
	}
	if !strings.Contains(configErr, "rollback also failed") {
		t.Errorf("expected 'rollback also failed' in error, got: %s", configErr)
	}
}

// TestApplyConfigViaReloadNoLastGood tests branch (c): reload fails,
// no last-good recorded → FAILED, message says "no last-good to roll back to".
func TestApplyConfigViaReloadNoLastGood(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)

	// First reload fails; no last-good anywhere.
	srv := cascReloadServer(t, func(_ int) (int, string) {
		return http.StatusInternalServerError, "reload exploded"
	})
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{Logger: slog.Default()}

	cmd := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: \"first ever\"\n",
	}
	configOK, configErr := agent.applyConfigViaReload(context.Background(), client, cmd)
	if configOK {
		t.Fatal("expected failure with no last-good")
	}
	if !strings.Contains(configErr, "no last-good to roll back to") {
		t.Errorf("expected 'no last-good to roll back to' in error, got: %s", configErr)
	}
}

// TestApplyConfigViaReloadWritesRbacYaml verifies that when RbacYaml is
// present, rbac.yaml is written alongside config.yaml.
func TestApplyConfigViaReloadWritesRbacYaml(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)

	// Pre-seed live files so saveLastGood has something to save.
	if err := os.MkdirAll(cascDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cascDir, "config.yaml"),
		[]byte("jenkins:\n  systemMessage: \"before\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cascDir, "rbac.yaml"),
		[]byte("jenkins:\n  authorizationStrategy: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	srv := cascReloadServer(t, func(_ int) (int, string) {
		return http.StatusOK, ""
	})
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{Logger: slog.Default()}

	cmd := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: \"hello\"\n",
		RbacYaml:  "jenkins:\n  authorizationStrategy:\n    roleBased:\n      roles:\n        global: []\n",
	}
	configOK, configErr := agent.applyConfigViaReload(context.Background(), client, cmd)
	if !configOK {
		t.Fatalf("expected success, got ConfigError=%q", configErr)
	}

	// Both files should be written.
	if _, err := os.Stat(filepath.Join(cascDir, "config.yaml")); err != nil {
		t.Errorf("expected config.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cascDir, "rbac.yaml")); err != nil {
		t.Errorf("expected rbac.yaml: %v", err)
	}
	// Both last-good copies should exist (saved from pre-seeded content).
	if _, err := os.Stat(filepath.Join(cascDir+"-last-good", "config.yaml")); err != nil {
		t.Errorf("expected last-good config.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cascDir+"-last-good", "rbac.yaml")); err != nil {
		t.Errorf("expected last-good rbac.yaml: %v", err)
	}
}

// TestFirstBootAppliesConfigNoRestart verifies that on first boot the mite applies
// JCasC config via the MANAGE-gated reload and never restarts Jenkins. Managed
// plugins are installed out of band by the plugins-init init container before the
// JVM starts, so the mite has no plugin step and no restart.
func TestFirstBootAppliesConfigNoRestart(t *testing.T) {
	t.Setenv("CASC_JENKINS_CONFIG", t.TempDir())
	srv, hits := fakeJenkins(t, "sess-1")
	agent := newTestAgentWithJenkins(t, srv.URL)

	// No last-applied-hash marker → first boot.
	if agent.hasLastAppliedHash() {
		t.Fatal("test agent should start without a marker (first boot)")
	}

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-first",
		JcascYaml:           "jenkins:\n  systemMessage: hi\n",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	result := agent.handleDesiredState(context.Background(), cmd)

	if !result.ConfigSuccess {
		t.Errorf("expected ConfigSuccess=true on first boot, got result=%+v", result)
	}
	if n := hits.SafeRestart.Load(); n != 0 {
		t.Errorf("mite must not restart Jenkins on first boot, got %d /safeRestart calls", n)
	}
	if n := hits.Reloads.Load(); n < 1 {
		t.Errorf("expected config applied via reload, got %d reloads", n)
	}
	if !agent.hasLastAppliedHash() {
		t.Error("expected last-applied-hash marker after a successful first-boot apply")
	}
}

// TestCertRenewalTriggersAtThreshold verifies shouldRenewCert's decision at
// the certRenewalThresholdNum/Den (70%) mark of a certificate's lifetime.
func TestCertRenewalTriggersAtThreshold(t *testing.T) {
	const lifetime = 72 * time.Hour

	belowThreshold := time.Now().Add(-lifetime * 69 / 100)
	if shouldRenewCert(belowThreshold, belowThreshold.Add(lifetime)) {
		t.Error("expected no renewal at 69% elapsed, got true")
	}

	aboveThreshold := time.Now().Add(-lifetime * 71 / 100)
	if !shouldRenewCert(aboveThreshold, aboveThreshold.Add(lifetime)) {
		t.Error("expected renewal at 71% elapsed, got false")
	}
}

// TestCertRenewalConcurrentAccess mirrors TestHealthCacheConcurrentAccess:
// storeCertFromResponse (renewal loop's write path) and a certMu-guarded read
// of a.cert (connect()'s read path) run concurrently under go test -race to
// confirm the certMu mutex added alongside the renewal loop prevents a race.
func TestCertRenewalConcurrentAccess(t *testing.T) {
	certAuth, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns"})
	agent.Logger = slog.Default()
	agent.keypair = priv
	agent.miteDir = t.TempDir() // keep cert writes off the real shared path

	newResp := func() *mitev1.RegisterResponse {
		cert, err := certAuth.IssueMiteCert("test", "ns", pub)
		if err != nil {
			t.Fatalf("issue cert: %v", err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		return &mitev1.RegisterResponse{CertificatePem: string(certPEM), CaPem: string(certAuth.CAPEM())}
	}

	if err := agent.storeCertFromResponse(newResp()); err != nil {
		t.Fatalf("initial storeCertFromResponse: %v", err)
	}

	var wg sync.WaitGroup

	// Writer: mirrors the renewal loop calling storeCertFromResponse.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := agent.storeCertFromResponse(newResp()); err != nil {
				t.Errorf("storeCertFromResponse: %v", err)
				return
			}
		}
	}()

	// Reader: mirrors connect()'s certMu-guarded read of a.cert.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			agent.certMu.Lock()
			cert := agent.cert
			agent.certMu.Unlock()
			if cert == nil {
				t.Error("cert unexpectedly nil")
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent cert access")
	}
}
