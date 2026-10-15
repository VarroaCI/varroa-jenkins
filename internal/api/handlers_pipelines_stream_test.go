package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// --------------------------------------------------------------------------
// Test doubles
// --------------------------------------------------------------------------

// runFanoutStub is the RunEventSource test double. It records the keys and
// the filter of each subscription and applies the filter on delivery,
// exactly as the fan-out does.
type runFanoutStub struct {
	mu     sync.Mutex
	keys   []string
	filter func(recordstore.RunNotification) bool
	ch     chan sse.Record
	subs   int
}

func (f *runFanoutStub) Subscribe(keys []string, filter func(recordstore.RunNotification) bool) <-chan sse.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append([]string(nil), keys...)
	f.filter = filter
	f.ch = make(chan sse.Record, 16)
	f.subs++
	return f.ch
}

func (f *runFanoutStub) Unsubscribe(_ []string, _ <-chan sse.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = nil
}

// deliver routes one notification through the live filter and reports
// whether a subscriber received it.
func (f *runFanoutStub) deliver(n recordstore.RunNotification) bool {
	f.mu.Lock()
	ch, filter := f.ch, f.filter
	f.mu.Unlock()
	if ch == nil {
		return false
	}
	if filter != nil && !filter(n) {
		return false
	}
	select {
	case ch <- sse.Record{Event: n.Kind, Data: n}:
		return true
	default:
		return false
	}
}

// keysOf answers the keys of the most recent subscription.
func (f *runFanoutStub) keysOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// subscriptions answers the number of subscriptions the handler made.
func (f *runFanoutStub) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs
}

// waitForSubscription blocks until the handler subscribes, or fails.
func (f *runFanoutStub) waitForSubscription(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.subscriptions() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the handler never subscribed to the run fan-out")
}

// sseRecorder is a mutex-guarded writer that also flushes, so a test reads
// whole frames while the handler writes them.
type sseRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	status int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *sseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *sseRecorder) Flush() {}

// body answers every byte the handler wrote so far.
func (r *sseRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitForFrame blocks until the body holds want, or fails.
func (r *sseRecorder) waitForFrame(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.body(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no frame %q in the stream body %q", want, r.body())
}

// expectNoFrame fails when the body holds want after a short wait.
func (r *sseRecorder) expectNoFrame(t *testing.T, want string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if body := r.body(); strings.Contains(body, want) {
		t.Fatalf("unexpected frame %q in the stream body %q", want, body)
	}
}

// refusingTenantResolver mirrors the answer Resolve gives a team whose
// candidate tenants the read check all refuses.
type refusingTenantResolver struct{}

func (refusingTenantResolver) Resolve(context.Context, *auth.Claims, string) ([]TenantRef, error) {
	return nil, &ServiceError{Status: http.StatusNotFound, Message: "not found"}
}

func (refusingTenantResolver) ResolveRun(context.Context, *auth.Claims, recordstore.Run) (TenantRef, error) {
	return TenantRef{}, nil
}

// pipelineGlobAuthorizer grants pipelines read on team-a, narrowed by the
// listed folder-relative job globs.
func pipelineGlobAuthorizer(jobs []string) *Authorizer {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pl-glob"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pl-glob-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "pl-glob",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: jobs}},
			},
		},
	}
	return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
}

// streamTestServer wires a server for the team run stream over stubFanout.
func streamTestServer(resolver TenantResolver, authorizer *Authorizer, stubFanout sse.RunEventSource) *Server {
	return NewServer(&Dependencies{
		RecordStore:    newFakeRecordReader(),
		TenantResolver: resolver,
		Authorizer:     authorizer,
		RunFanout:      stubFanout,
		Logger:         slog.Default(),
	})
}

// startTeamRunStream runs the handler on its own goroutine and answers the
// recorder and a stop function that ends the stream.
func startTeamRunStream(t *testing.T, srv *Server, team string) (*sseRecorder, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(authContext(testClaims()))
	req := httptest.NewRequest(http.MethodGet, "/pipelines/"+team+"/stream", nil).WithContext(ctx)
	req.SetPathValue("team", team)

	rec := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleTeamRunStream(rec, req)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the stream handler did not return after cancellation")
		}
	}
	return rec, stop
}

// teamStreamRefs answers two placed tenants in one cluster.
func teamStreamRefs() []TenantRef {
	return []TenantRef{
		{Cluster: "core", Tenant: "team-a", Folder: "team-a"},
		{Cluster: "core", Tenant: "team-b", Folder: "team-b"},
	}
}

// notification builds one notification for a key and a job path.
func notification(kind, tenant, jobPath string) recordstore.RunNotification {
	return recordstore.RunNotification{
		Kind: kind, Cluster: "core", Tenant: tenant, JobPath: jobPath, RunID: "run-1",
	}
}

// --------------------------------------------------------------------------
// The stream handler
// --------------------------------------------------------------------------

// TestHandleTeamRunStream_ServesItsOwnTeamTenantsOnly proves the handler
// subscribes one key per resolved tenant, and that a notification for a
// tenant outside the team reaches no caller.
func TestHandleTeamRunStream_ServesItsOwnTeamTenantsOnly(t *testing.T) {
	stub := &runFanoutStub{}
	srv := streamTestServer(
		fixedTenantResolver{refs: teamStreamRefs()},
		pipelineGrantAuthorizer([]string{"read"}, "team-a", "team-b"),
		stub,
	)

	rec, stop := startTeamRunStream(t, srv, "payments")
	defer stop()
	stub.waitForSubscription(t)

	wantKeys := []string{"core/team-a", "core/team-b"}
	got := stub.keysOf()
	if len(got) != len(wantKeys) {
		t.Fatalf("subscription keys = %v, want %v", got, wantKeys)
	}
	for i := range wantKeys {
		if got[i] != wantKeys[i] {
			t.Errorf("subscription key %d = %q, want %q", i, got[i], wantKeys[i])
		}
	}

	if stub.deliver(notification("run", "team-c", "team-c/app")) {
		t.Error("a notification for another tenant passed the filter")
	}
	rec.expectNoFrame(t, "team-c")

	if !stub.deliver(notification("run", "team-a", "team-a/app")) {
		t.Fatal("a notification for the caller's own tenant was refused")
	}
	rec.waitForFrame(t, `"tenant":"team-a"`)
}

// TestHandleTeamRunStream_JobGlobsRefuseAJob proves a grant narrowed by job
// globs admits only the jobs the globs cover.
func TestHandleTeamRunStream_JobGlobsRefuseAJob(t *testing.T) {
	stub := &runFanoutStub{}
	srv := streamTestServer(
		fixedTenantResolver{refs: []TenantRef{{Cluster: "core", Tenant: "team-a", Folder: "team-a"}}},
		pipelineGlobAuthorizer([]string{"release/*"}),
		stub,
	)

	rec, stop := startTeamRunStream(t, srv, "team-a")
	defer stop()
	stub.waitForSubscription(t)

	if stub.deliver(notification("run", "team-a", "team-a/app")) {
		t.Error("a job outside the caller's globs passed the filter")
	}
	rec.expectNoFrame(t, "team-a/app")

	if !stub.deliver(notification("stage", "team-a", "team-a/release/app")) {
		t.Fatal("a job inside the caller's globs was refused")
	}
	rec.waitForFrame(t, `"job_path":"team-a/release/app"`)
}

// TestHandleTeamRunStream_EmptyJobPathIsDropped proves the filter drops a
// notification whose job path is empty, for a caller who holds a full read
// grant and for a caller narrowed by job globs. The narrowed caller still
// opens the stream, and still receives the jobs its globs cover.
func TestHandleTeamRunStream_EmptyJobPathIsDropped(t *testing.T) {
	refs := []TenantRef{{Cluster: "core", Tenant: "team-a", Folder: "team-a"}}

	t.Run("a full read grant drops the empty job path", func(t *testing.T) {
		stub := &runFanoutStub{}
		srv := streamTestServer(fixedTenantResolver{refs: refs}, pipelineGrantAuthorizer([]string{"read"}, "team-a"), stub)

		rec, stop := startTeamRunStream(t, srv, "team-a")
		defer stop()
		stub.waitForSubscription(t)

		if stub.deliver(notification("run", "team-a", "")) {
			t.Error("an empty job path passed the filter")
		}
		rec.expectNoFrame(t, `"run_id":"run-1"`)

		// The same caller receives a notification that carries a job path.
		if !stub.deliver(notification("run", "team-a", "team-a/app")) {
			t.Fatal("a job path was refused for a full read grant")
		}
		rec.waitForFrame(t, `"job_path":"team-a/app"`)
	})

	t.Run("a jobs-narrowed caller opens the stream and drops the empty path", func(t *testing.T) {
		stub := &runFanoutStub{}
		srv := streamTestServer(fixedTenantResolver{refs: refs}, pipelineGlobAuthorizer([]string{"release/*"}), stub)

		rec, stop := startTeamRunStream(t, srv, "team-a")
		defer stop()
		stub.waitForSubscription(t)

		if got := stub.keysOf(); len(got) != 1 || got[0] != "core/team-a" {
			t.Fatalf("subscription keys = %v, want [core/team-a]", got)
		}
		if stub.deliver(notification("run", "team-a", "")) {
			t.Error("an empty job path passed the filter for a narrowed caller")
		}
		rec.expectNoFrame(t, `"run_id":"run-1"`)

		if !stub.deliver(notification("stage", "team-a", "team-a/release/app")) {
			t.Fatal("a job inside the caller's globs was refused")
		}
		rec.waitForFrame(t, `"job_path":"team-a/release/app"`)
	})
}

// TestHandleTeamRunStream_ForwardsALockNotification proves a lock
// notification meets the same tenant set and job-grant filter a run
// notification meets: one whose job path the caller reads is forwarded
// with its own fields unchanged, and one whose job path is empty is
// dropped.
func TestHandleTeamRunStream_ForwardsALockNotification(t *testing.T) {
	stub := &runFanoutStub{}
	srv := streamTestServer(
		fixedTenantResolver{refs: []TenantRef{{Cluster: "core", Tenant: "team-a", Folder: "team-a"}}},
		pipelineGrantAuthorizer([]string{"read"}, "team-a"),
		stub,
	)

	rec, stop := startTeamRunStream(t, srv, "team-a")
	defer stop()
	stub.waitForSubscription(t)

	if stub.deliver(notification("lock", "team-a", "")) {
		t.Error("a lock notification with an empty job path passed the filter")
	}
	rec.expectNoFrame(t, `"kind":"lock"`)

	locked := notification("lock", "team-a", "team-a/release/app")
	locked.LockName = "team-a/gate"
	locked.State = "released"
	if !stub.deliver(locked) {
		t.Fatal("a lock notification with a readable job path was refused")
	}
	rec.waitForFrame(t, `"kind":"lock"`)
	rec.waitForFrame(t, `"lock_name":"team-a/gate"`)
	rec.waitForFrame(t, `"state":"released"`)
}

// TestHandleTeamRunStream_ReconnectReceivesNoEarlierEvent proves the stream
// replays nothing: a second connection sees only what arrives after it.
func TestHandleTeamRunStream_ReconnectReceivesNoEarlierEvent(t *testing.T) {
	stub := &runFanoutStub{}
	srv := streamTestServer(
		fixedTenantResolver{refs: teamStreamRefs()},
		pipelineGrantAuthorizer([]string{"read"}, "team-a", "team-b"),
		stub,
	)

	first, stopFirst := startTeamRunStream(t, srv, "payments")
	stub.waitForSubscription(t)
	stub.deliver(notification("run", "team-a", "team-a/app"))
	first.waitForFrame(t, `"run_id":"run-1"`)
	stopFirst()

	second, stopSecond := startTeamRunStream(t, srv, "payments")
	defer stopSecond()
	deadline := time.Now().Add(2 * time.Second)
	for stub.subscriptions() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	second.expectNoFrame(t, "event: run")

	if !stub.deliver(notification("stage", "team-a", "team-a/app")) {
		t.Fatal("the second connection refused a fresh notification")
	}
	second.waitForFrame(t, "event: stage")
}

// TestHandleTeamRunStream_UnavailableWithoutDependencies proves the handler
// answers 503 when the record store or the run fan-out is absent.
func TestHandleTeamRunStream_UnavailableWithoutDependencies(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/pipelines/team-a/stream", nil).
		WithContext(authContext(testClaims()))
	req.SetPathValue("team", "team-a")

	t.Run("no record store", func(t *testing.T) {
		srv := NewServer(&Dependencies{RunFanout: &runFanoutStub{}, Logger: slog.Default()})
		rec := httptest.NewRecorder()
		srv.handleTeamRunStream(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("no run fan-out", func(t *testing.T) {
		srv := NewServer(&Dependencies{RecordStore: newFakeRecordReader(), Logger: slog.Default()})
		rec := httptest.NewRecorder()
		srv.handleTeamRunStream(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})
}

// TestHandleTeamRunStream_RefusedTeamAnswers404 proves the handler writes the
// resolution error unchanged, so a team the caller may not read answers 404.
func TestHandleTeamRunStream_RefusedTeamAnswers404(t *testing.T) {
	srv := NewServer(&Dependencies{
		RecordStore:    newFakeRecordReader(),
		TenantResolver: refusingTenantResolver{},
		Authorizer:     pipelineGrantAuthorizer([]string{"read"}, "team-a"),
		RunFanout:      &runFanoutStub{},
		Logger:         slog.Default(),
	})

	req := httptest.NewRequest(http.MethodGet, "/pipelines/team-a/stream", nil).
		WithContext(authContext(testClaims()))
	req.SetPathValue("team", "team-a")
	rec := httptest.NewRecorder()
	srv.handleTeamRunStream(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
}

// TestHandleTeamRunStream_FrameCarriesTheNotification proves one record
// yields one frame, with the notification's own kind as the event name and
// the notification body as data.
func TestHandleTeamRunStream_FrameCarriesTheNotification(t *testing.T) {
	stub := &runFanoutStub{}
	srv := streamTestServer(
		fixedTenantResolver{refs: teamStreamRefs()},
		pipelineGrantAuthorizer([]string{"read"}, "team-a"),
		stub,
	)

	rec, stop := startTeamRunStream(t, srv, "payments")
	defer stop()
	stub.waitForSubscription(t)

	stub.deliver(notification("stage", "team-a", "team-a/app"))
	rec.waitForFrame(t, "event: stage\n")

	body := rec.body()
	const prefix = "data: "
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("no data line in %q", body)
	}
	line := body[idx+len(prefix):]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	var decoded recordstore.RunNotification
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("frame data is not a notification: %v (%q)", err, line)
	}
	if decoded.Kind != "stage" || decoded.Tenant != "team-a" {
		t.Errorf("frame data = %+v, want a stage notification for team-a", decoded)
	}
}
