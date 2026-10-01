package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeFleetLockReleaser answers a fixed reply and records each request.
type fakeFleetLockReleaser struct {
	resp recordstore.FleetLockForceReleaseResponse
	err  error
	reqs []recordstore.FleetLockForceReleaseRequest
}

func (f *fakeFleetLockReleaser) ForceRelease(_ context.Context,
	req recordstore.FleetLockForceReleaseRequest) (recordstore.FleetLockForceReleaseResponse, error) {
	f.reqs = append(f.reqs, req)
	return f.resp, f.err
}

// releaseTestServer answers a server whose caller holds rules on one
// cluster-wide binding, wired to releaser and an activity sink.
func releaseTestServer(t *testing.T, releaser *fakeFleetLockReleaser, rules ...v1alpha1.APIRule) (*Server, *recordingActivitySink) {
	t.Helper()
	reader := newFakeRecordReader()
	reader.allLocks = []recordstore.FleetLock{
		lockRow("acme/deploy-prod", "run-1", "acme", "acme/release", "releasing", lockTestBase),
	}
	srv := pipelinesTestServer(lockTeamClient(t), fleetLockAuthorizer(rules...), reader, nil)
	if releaser != nil {
		srv.deps.FleetLockReleaser = releaser
	}
	sink := &recordingActivitySink{}
	srv.deps.ActivityPublisher = sink
	return srv, sink
}

// doReleaseRequest posts body to the release handler as the test caller.
func doReleaseRequest(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/locks/release", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), testClaims()))
	rec := httptest.NewRecorder()
	srv.handleReleaseFleetLock(rec, req)
	return rec
}

var releaseRule = v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read", "release"}}

const releaseBody = `{"name":"acme/deploy-prod","runId":"run-1","reason":"the old pod is gone"}`

// TestReleaseFleetLock_ReleasesAndAudits asserts a release answers the
// released row, forwards the actor and the reason, and writes one global
// fleetlock.force_released activity row on the lock's cluster.
func TestReleaseFleetLock_ReleasesAndAudits(t *testing.T) {
	releaser := &fakeFleetLockReleaser{resp: recordstore.FleetLockForceReleaseResponse{
		Outcome: recordstore.FleetLockForceReleased, Cluster: "member", Tenant: "acme",
		JobPath: "acme/release", CellInstance: "pod-a",
	}}
	srv, sink := releaseTestServer(t, releaser, releaseRule)

	rec := doReleaseRequest(t, srv, releaseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got FleetLockReleaseResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := FleetLockReleaseResult{Name: "acme/deploy-prod", RunID: "run-1", Cluster: "member", Tenant: "acme",
		JobPath: "acme/release", CellInstance: "pod-a", State: "released"}
	if got != want {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	if len(releaser.reqs) != 1 || releaser.reqs[0].Reason != "the old pod is gone" || releaser.reqs[0].Actor == "" {
		t.Fatalf("requests = %+v, want one with the reason and an actor", releaser.reqs)
	}
	if len(sink.events) != 1 {
		t.Fatalf("activity events = %d, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Type != "fleetlock.force_released" || e.Controller != "" || e.Cluster != "member" ||
		e.Reason != "the old pod is gone" || e.Source != "user" || e.Actor == "" {
		t.Fatalf("activity event = %+v", e)
	}
}

// TestReleaseFleetLock_MapsRefusals asserts each recorder refusal maps to
// its status and writes no activity row.
func TestReleaseFleetLock_MapsRefusals(t *testing.T) {
	cases := []struct {
		name   string
		resp   recordstore.FleetLockForceReleaseResponse
		err    error
		status int
	}{
		{"no row answers 404", recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceNotFound}, nil, http.StatusNotFound},
		{"a held row answers 409", recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceNotReleasing, State: "held"}, nil, http.StatusConflict},
		{"a live holder answers 409", recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceHolderLive, CellInstance: "pod-a"}, nil, http.StatusConflict},
		{"no recorder answers 503", recordstore.FleetLockForceReleaseResponse{}, ErrFleetLockReleaseNotDelivered, http.StatusServiceUnavailable},
		{"an error reply answers 503", recordstore.FleetLockForceReleaseResponse{}, ErrFleetLockReleaseFailed, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, sink := releaseTestServer(t, &fakeFleetLockReleaser{resp: tc.resp, err: tc.err}, releaseRule)
			rec := doReleaseRequest(t, srv, releaseBody)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if len(sink.events) != 0 {
				t.Fatalf("a refusal wrote activity: %+v", sink.events)
			}
		})
	}
}

// TestReleaseFleetLock_AuditsAnUnknownOutcome asserts a request the
// recorder can have acted on with no reply writes one unconfirmed audit
// row, so a committed release never lacks its audit record.
func TestReleaseFleetLock_AuditsAnUnknownOutcome(t *testing.T) {
	srv, sink := releaseTestServer(t, &fakeFleetLockReleaser{err: errors.New("nats: timeout")}, releaseRule)
	member := lockRow("acme/deploy-prod", "run-1", "acme", "acme/release", "releasing", lockTestBase)
	member.Cluster = "member"
	reader := newFakeRecordReader()
	reader.allLocks = []recordstore.FleetLock{member}
	srv.deps.RecordStore = reader
	rec := doReleaseRequest(t, srv, releaseBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("activity events = %d, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Type != "fleetlock.force_release_unconfirmed" || e.Reason != "the old pod is gone" || e.Actor == "" ||
		e.Cluster != "member" {
		t.Fatalf("activity event = %+v", e)
	}
}

// TestReleaseFleetLock_CancelBeforeSendSendsNothing asserts a caller that
// left before the request went out sends no request and writes no audit.
func TestReleaseFleetLock_CancelBeforeSendSendsNothing(t *testing.T) {
	releaser := &fakeFleetLockReleaser{resp: recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceReleased}}
	srv, sink := releaseTestServer(t, releaser, releaseRule)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, serr := srv.ReleaseFleetLock(ctx, testClaims(), "acme/deploy-prod", "run-1", "x", "user"); serr == nil {
		t.Fatal("a cancelled caller released a lock")
	}
	if len(releaser.reqs) != 0 || len(sink.events) != 0 {
		t.Fatalf("requests = %d, events = %d; want 0, 0", len(releaser.reqs), len(sink.events))
	}
}

// TestReleaseFleetLock_WaitIgnoresCallerCancel asserts the releaser gets a
// context that a cancel during the wait does not end.
func TestReleaseFleetLock_WaitIgnoresCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	releaser := &ctxCheckingReleaser{cancelCaller: cancel}
	srv, _ := releaseTestServer(t, nil, releaseRule)
	srv.deps.FleetLockReleaser = releaser
	if _, serr := srv.ReleaseFleetLock(ctx, testClaims(), "acme/deploy-prod", "run-1", "x", "user"); serr != nil {
		t.Fatalf("ReleaseFleetLock: %v", serr)
	}
	if releaser.ctxErr != nil {
		t.Fatalf("the releaser saw a cancelled context: %v", releaser.ctxErr)
	}
}

// ctxCheckingReleaser cancels the caller's context during the wait, then
// records whether its own context ended with it.
type ctxCheckingReleaser struct {
	cancelCaller context.CancelFunc
	ctxErr       error
}

func (c *ctxCheckingReleaser) ForceRelease(ctx context.Context,
	_ recordstore.FleetLockForceReleaseRequest) (recordstore.FleetLockForceReleaseResponse, error) {
	c.cancelCaller()
	c.ctxErr = ctx.Err()
	return recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceReleased}, nil
}

// TestReleaseFleetLock_RefusesBeforeTheRecorder asserts the gate, the body
// rules, and a missing record store refuse with no request to the recorder.
func TestReleaseFleetLock_RefusesBeforeTheRecorder(t *testing.T) {
	readOnly := v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}}
	cases := []struct {
		name   string
		rules  []v1alpha1.APIRule
		nilRel bool
		body   string
		status int
	}{
		{"a caller with read only is forbidden", []v1alpha1.APIRule{readOnly}, false, releaseBody, http.StatusForbidden},
		{"a missing reason is a bad request", []v1alpha1.APIRule{releaseRule}, false,
			`{"name":"acme/deploy-prod","runId":"run-1"}`, http.StatusBadRequest},
		{"a blank reason is a bad request", []v1alpha1.APIRule{releaseRule}, false,
			`{"name":"acme/deploy-prod","runId":"run-1","reason":"  "}`, http.StatusBadRequest},
		{"an unknown key is a bad request", []v1alpha1.APIRule{releaseRule}, false,
			`{"name":"acme/deploy-prod","runId":"run-1","reason":"x","force":true}`, http.StatusBadRequest},
		{"no record store answers 503", []v1alpha1.APIRule{releaseRule}, true, releaseBody, http.StatusServiceUnavailable},
		{"a lock the pre-read does not find answers 404", []v1alpha1.APIRule{releaseRule}, false,
			`{"name":"acme/other","runId":"run-1","reason":"x"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			releaser := &fakeFleetLockReleaser{resp: recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceReleased}}
			if tc.nilRel {
				releaser = nil
			}
			srv, _ := releaseTestServer(t, releaser, tc.rules...)
			rec := doReleaseRequest(t, srv, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if releaser != nil && len(releaser.reqs) != 0 {
				t.Fatalf("the recorder was asked: %+v", releaser.reqs)
			}
		})
	}
}

// TestReleaseFleetLock_FailedPreReadSendsNothing asserts a failed read of
// the lock answers 503 and sends no request, so no release can go out with
// no cluster for its audit row.
func TestReleaseFleetLock_FailedPreReadSendsNothing(t *testing.T) {
	releaser := &fakeFleetLockReleaser{resp: recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceReleased}}
	srv, sink := releaseTestServer(t, releaser, releaseRule)
	srv.deps.RecordStore.(*fakeRecordReader).locksErr = errors.New("read timeout")
	rec := doReleaseRequest(t, srv, releaseBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if len(releaser.reqs) != 0 || len(sink.events) != 0 {
		t.Fatalf("requests = %d, events = %d; want 0, 0", len(releaser.reqs), len(sink.events))
	}
}

// TestCanReleaseFleetLocks asserts the release gate takes a cluster-wide
// binding: the same verb narrowed to a namespace is denied, and so is a
// nil caller.
func TestCanReleaseFleetLocks(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{releaseRule}},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "operator",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}
	claims := &auth.Claims{Subject: "user-a"}
	scoped := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
	if scoped.CanReleaseFleetLocks(claims) {
		t.Error("a namespace-scoped binding must be denied")
	}
	wide := *binding
	wide.Spec.Scope = nil
	wideAuth := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{&wide}), false)
	if !wideAuth.CanReleaseFleetLocks(claims) {
		t.Error("a cluster-wide binding with fleetlocks:release must be allowed")
	}
	if wideAuth.CanReleaseFleetLocks(nil) {
		t.Error("a nil caller must be denied")
	}
	admin := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}
	adminBinding := wide
	adminBinding.Spec.RoleRef = "admin"
	adminAuth := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{admin}, []*v1alpha1.VarroaRoleBinding{&adminBinding}), false)
	if !adminAuth.CanReleaseFleetLocks(claims) {
		t.Error("the admin wildcard must be allowed")
	}
}

// TestReleaseReplyError asserts an error reply from the commit is an
// unknown outcome, and any other error reply changed no row.
func TestReleaseReplyError(t *testing.T) {
	if err := releaseReplyError(recordstore.FleetLockForceReleaseResponse{Outcome: recordstore.FleetLockForceReleased}); err != nil {
		t.Fatalf("a released reply = %v, want nil", err)
	}
	err := releaseReplyError(recordstore.FleetLockForceReleaseResponse{Error: "bad request"})
	if !errors.Is(err, ErrFleetLockReleaseFailed) {
		t.Fatalf("an error reply = %v, want ErrFleetLockReleaseFailed", err)
	}
	err = releaseReplyError(recordstore.FleetLockForceReleaseResponse{Error: "commit failed", Unknown: true})
	if err == nil || errors.Is(err, ErrFleetLockReleaseFailed) || errors.Is(err, ErrFleetLockReleaseNotDelivered) {
		t.Fatalf("a commit fault = %v, want an unknown outcome", err)
	}
}
