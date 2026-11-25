package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// errorBody decodes the error envelope of a refusal.
func errorBody(t *testing.T, w *httptest.ResponseRecorder) (msg, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return body.Error, body.Code
}

// assertNoOperator asserts the 503 operator_unavailable answer that names
// cluster.
func assertNoOperator(t *testing.T, w *httptest.ResponseRecorder, cluster string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	msg, code := errorBody(t, w)
	if code != codeOperatorUnavailable {
		t.Errorf("code = %q, want %q", code, codeOperatorUnavailable)
	}
	if msg != noOperatorText(cluster) {
		t.Errorf("error = %q, want %q", msg, noOperatorText(cluster))
	}
}

// assertUnreachable asserts the 502 answer that names cluster.
func assertUnreachable(t *testing.T, w *httptest.ResponseRecorder, cluster string) {
	t.Helper()
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	msg, code := errorBody(t, w)
	if code != "" {
		t.Errorf("code = %q, want none", code)
	}
	if want := "cluster " + cluster + " unreachable"; msg != want {
		t.Errorf("error = %q, want %q", msg, want)
	}
}

// assertSpec503 asserts that the body of w matches the 503 schema that the
// embedded spec declares for the operation.
func assertSpec503(t *testing.T, w *httptest.ResponseRecorder, method, path string) {
	t.Helper()
	doc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	item := doc.Paths.Find(path)
	if item == nil || item.GetOperation(method) == nil {
		t.Fatalf("spec has no %s %s", method, path)
	}
	resp := item.GetOperation(method).Responses.Status(http.StatusServiceUnavailable)
	if resp == nil || resp.Value == nil {
		t.Fatalf("%s %s declares no 503 response", method, path)
	}
	var body any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if err := resp.Value.Content.Get("application/json").Schema.Value.VisitJSON(body); err != nil {
		t.Errorf("%s %s 503 body %s does not match the spec: %v", method, path, w.Body.String(), err)
	}
}

func TestClusterRequestFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		msg    string
	}{
		{"no responder", nats.ErrNoResponders, http.StatusServiceUnavailable, codeOperatorUnavailable, noOperatorText("core")},
		{"wrapped no responder", fmt.Errorf("hibernate relay: %w", nats.ErrNoResponders), http.StatusServiceUnavailable, codeOperatorUnavailable, noOperatorText("core")},
		{"timeout", nats.ErrTimeout, http.StatusBadGateway, "", "cluster core unreachable"},
		{"connection closed", nats.ErrConnectionClosed, http.StatusBadGateway, "", "cluster core unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serr := clusterRequestFailure("core", tc.err)
			if serr.Status != tc.status || serr.Code != tc.code || serr.Message != tc.msg {
				t.Errorf("serr = %+v, want status %d, code %q, message %q", serr, tc.status, tc.code, tc.msg)
			}
		})
	}
}

func TestWriteUnreachable(t *testing.T) {
	w := httptest.NewRecorder()
	if !writeUnreachable(w, fmt.Errorf("sync: %w", &ErrClusterUnreachable{Cluster: "member-b", Err: nats.ErrNoResponders})) {
		t.Fatal("writeUnreachable = false for a wrapped ErrClusterUnreachable")
	}
	assertNoOperator(t, w, "member-b")

	w = httptest.NewRecorder()
	if !writeUnreachable(w, &ErrClusterUnreachable{Cluster: "member-b", Err: nats.ErrTimeout}) {
		t.Fatal("writeUnreachable = false for a timeout")
	}
	assertUnreachable(t, w, "member-b")

	w = httptest.NewRecorder()
	if writeUnreachable(w, &BroodError{Code: bus.CodeNotFound, Msg: "gone"}) {
		t.Error("writeUnreachable = true for a BroodError")
	}
	if w.Body.Len() != 0 {
		t.Errorf("writeUnreachable wrote %q for a BroodError", w.Body.String())
	}
}

func TestActionRequestError(t *testing.T) {
	err := ActionRequestError("core", fmt.Errorf("wake relay: %w", nats.ErrNoResponders))
	if err.Error() != noOperatorText("core") {
		t.Errorf("no responder = %q, want the no-operator text", err.Error())
	}
	err = ActionRequestError("core", fmt.Errorf("wake relay: %w", nats.ErrTimeout))
	if err.Error() != "cluster core unreachable: wake relay: nats: timeout" {
		t.Errorf("timeout = %q, want the unreachable text", err.Error())
	}
	once := &ErrClusterUnreachable{Cluster: "member-b", Err: nats.ErrTimeout}
	if got := ActionRequestError("core", once); got != once {
		t.Errorf("an ErrClusterUnreachable = %v, want it unchanged", got)
	}
	refusal := &BroodError{Code: bus.CodeConflict, Msg: "stopped"}
	if got := ActionRequestError("core", refusal); got != refusal {
		t.Errorf("a refusal = %v, want it unchanged", got)
	}
}

func TestClusterRowFailure(t *testing.T) {
	code, msg := clusterRowFailure("member-b", nats.ErrNoResponders)
	if code != codeOperatorUnavailable || msg != noOperatorText("member-b") {
		t.Errorf("no responder row = (%q, %q), want the no-operator code and text", code, msg)
	}
	code, msg = clusterRowFailure("member-b", nats.ErrTimeout)
	if code != "" || msg != "cluster member-b unreachable: nats: timeout" {
		t.Errorf("timeout row = (%q, %q), want no code and the unreachable text", code, msg)
	}
}

func TestFanoutTransportStatus(t *testing.T) {
	noOp := fanoutRow{code: codeOperatorUnavailable, err: noOperatorText("core")}
	timeout := fanoutRow{err: "cluster member-b unreachable: nats: timeout"}
	invalid := fanoutRow{code: bus.CodeInvalid, err: "bad spec"}
	cases := []struct {
		name string
		rows []fanoutRow
		want int
	}{
		{"reply codes only", []fanoutRow{invalid}, 0},
		{"no operator only", []fanoutRow{noOp, noOp}, http.StatusServiceUnavailable},
		{"no operator and a reply code", []fanoutRow{invalid, noOp}, http.StatusServiceUnavailable},
		{"timeout only", []fanoutRow{timeout}, http.StatusBadGateway},
		{"no operator and a timeout", []fanoutRow{noOp, timeout}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fanoutTransportStatus(tc.rows); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWriteConfigBroodError_NoOperatorIs503(t *testing.T) {
	w := httptest.NewRecorder()
	writeConfigBroodError(w, &ErrClusterUnreachable{Cluster: "member-b", Err: nats.ErrNoResponders}, "member-b")
	assertNoOperator(t, w, "member-b")

	w = httptest.NewRecorder()
	writeConfigBroodError(w, &ErrClusterUnreachable{Cluster: "member-b", Err: nats.ErrTimeout}, "member-b")
	assertUnreachable(t, w, "member-b")
}

func TestWriteBroodError_NoOperatorIs503(t *testing.T) {
	w := httptest.NewRecorder()
	writeBroodError(w, &ErrClusterUnreachable{Cluster: "core", Err: nats.ErrNoResponders})
	assertNoOperator(t, w, "core")

	w = httptest.NewRecorder()
	writeBroodError(w, &ErrClusterUnreachable{Cluster: "core", Err: nats.ErrTimeout})
	assertUnreachable(t, w, "core")
}

// getErrBrood answers every controller read with err.
type getErrBrood struct {
	Brood
	err error
}

func (b *getErrBrood) LocalCluster() string { return "core" }

func (b *getErrBrood) Get(context.Context, string, string, string) (*v1alpha1.Controller, error) {
	return nil, b.err
}

func TestControllerDetailAndYAML_NoOperatorIs503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		assert func(*testing.T, *httptest.ResponseRecorder, string)
	}{
		{"no responder", nats.ErrNoResponders, assertNoOperator},
		{"timeout", nats.ErrTimeout, assertUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(&Dependencies{
				Authorizer: adminTestAuthorizer(),
				Store:      crdstore.NewFake(),
				Brood:      &getErrBrood{Brood: newFakeBrood(newFakeResourceClient()), err: &ErrClusterUnreachable{Cluster: "member-b", Err: tc.err}},
				Logger:     slog.Default(),
			})
			w := httptest.NewRecorder()
			srv.handleControllerDetail(w, reqCtxRequest(http.MethodGet, "/", ""), "member-b", "ns1", "c1")
			tc.assert(t, w, "member-b")
			w = httptest.NewRecorder()
			srv.handleControllerYAML(w, reqCtxRequest(http.MethodGet, "/", ""), "member-b", "ns1", "c1")
			tc.assert(t, w, "member-b")
		})
	}
}

func TestControllerActions_NoOperatorIs503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		assert func(*testing.T, *httptest.ResponseRecorder, string)
	}{
		{"no responder", fmt.Errorf("hibernate relay: %w", nats.ErrNoResponders), assertNoOperator},
		{"timeout", fmt.Errorf("hibernate relay: %w", nats.ErrTimeout), assertUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newActionTestServer()
			srv.deps.Reconciler = &fakeActionReconciler{hibernateErr: tc.err, wakeErr: tc.err}
			for _, action := range []string{"hibernate", "wake"} {
				w := postAction(t, srv, managerClaims, "/clusters/core/controllers/team-a/ci/"+action)
				tc.assert(t, w, "core")
			}

			approve, _ := approveTestServer(tc.err)
			tc.assert(t, postApprove(t, approve, `{"action":"restart"}`), "core")

			approve.deps.Reconciler = &fakeActionReconciler{deletionErr: tc.err}
			req := httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/approve-deletion", strings.NewReader(`{"path":"jobs/a"}`))
			req = req.WithContext(contextWithClaims(req.Context(), publishClaims))
			w := httptest.NewRecorder()
			approve.handleApproveDeletion(w, req, "core", "ns", "ctrl")
			tc.assert(t, w, "core")
		})
	}
}

// TestReconcileAndReprovision_NoReplyMapsThroughClusterRequestFailure pins
// that both actions answer 503 or 502 when the operator does not reply, 500
// when the operator refuses, and publish no activity event in either case.
func TestReconcileAndReprovision_NoReplyMapsThroughClusterRequestFailure(t *testing.T) {
	assertInternal := func(t *testing.T, w *httptest.ResponseRecorder, _ string) {
		t.Helper()
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		name   string
		err    error
		assert func(*testing.T, *httptest.ResponseRecorder, string)
	}{
		{"no responder", fmt.Errorf("reconcile relay: %w", nats.ErrNoResponders), assertNoOperator},
		{"timeout", fmt.Errorf("reconcile relay: %w", nats.ErrTimeout), assertUnreachable},
		{"operator refusal", errors.New("reconcile: bad request"), assertInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sink := publishTestServer(t)
			srv.deps.Reconciler = &fakeActionReconciler{reconcileErr: tc.err, reprovisionErr: tc.err}
			for action, handle := range map[string]func(http.ResponseWriter, *http.Request, string, string, string){
				"reconcile":   srv.handleReconcile,
				"reprovision": srv.handleReprovision,
			} {
				w := httptest.NewRecorder()
				r := withClaims(httptest.NewRequest(http.MethodPost, "/clusters/member-b/controllers/ns/ctrl/"+action, nil))
				handle(w, r, "member-b", "ns", "ctrl")
				tc.assert(t, w, "member-b")
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}

func TestCellGrants_MemberNoOperatorIs503(t *testing.T) {
	srv := memberCellGrantsServer(&memberGrantsBrood{err: &ErrClusterUnreachable{Cluster: "member-c", Err: nats.ErrNoResponders}})
	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants?cluster=member-c",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	assertNoOperator(t, rec, "member-c")
	assertSpec503(t, rec, http.MethodGet, "/api/v1/cells/{namespace}/{name}/grants")
}

func TestLintJenkinsfile_MemberNoOperatorIs503(t *testing.T) {
	fx, _ := memberLintFixture(t, v1alpha1.ControllerPhaseConnected, bus.CellLintResponse{},
		&ErrClusterUnreachable{Cluster: "east", Err: nats.ErrNoResponders})
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusServiceUnavailable || serr.Code != codeOperatorUnavailable {
		t.Fatalf("serr = %+v, want 503 operator_unavailable", serr)
	}
}

func TestCellPools_NoOperatorMemberIs503(t *testing.T) {
	srv, _, members, _, ctx := broodCellPoolServer(t)
	members.noOperator = map[string]bool{"member-c": true}

	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		req := httptest.NewRequest(method, "/cellpools/member-2-570?cluster=member-c",
			strings.NewReader(`{"spec":{"warm":2}}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleCellPoolDispatch(w, req)
		assertNoOperator(t, w, "member-c")
		assertSpec503(t, w, method, "/api/v1/cellpools/{name}")
	}
}

func TestBroodScheduleHandlers_NoOperatorIs503(t *testing.T) {
	failure := &ErrClusterUnreachable{Cluster: "core", Err: nats.ErrNoResponders}
	srv := newBroodScheduleTestServer(&stubBroodSchedules{
		createErr: failure, getErr: failure, deleteErr: failure, suspendErr: failure,
	})
	admin := &auth.Claims{Subject: "admin", Groups: []string{"admins"}}
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/brood-schedules", `{"namespace":"team-ns","name":"s","spec":{"schedule":"*/5 * * * *","template":{"targets":{"names":["ctrl-1"]},"action":{"verb":"reconcile"}}}}`},
		{http.MethodGet, "/brood-schedules/team-ns/s", ""},
		{http.MethodDelete, "/brood-schedules/team-ns/s", ""},
		{http.MethodPost, "/brood-schedules/team-ns/s/suspend", `{"suspend":true}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req = req.WithContext(auth.ContextWithClaims(req.Context(), admin))
			w := httptest.NewRecorder()
			srv.handleBroodSchedules(w, req)
			assertNoOperator(t, w, "core")
		})
	}
}

func TestBusBroodSchedules_NoResponderNamesTheOperator(t *testing.T) {
	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(string, []byte, time.Duration) ([]byte, error) {
			return nil, nats.ErrNoResponders
		},
	}
	err := f.Delete(context.Background(), "", "team-ns", "s")
	var unreachable *ErrClusterUnreachable
	if !errors.As(err, &unreachable) || unreachable.Cluster != "core" {
		t.Fatalf("err = %v, want ErrClusterUnreachable naming core", err)
	}
	if err.Error() != noOperatorText("core") {
		t.Errorf("err = %q, want the no-operator text", err.Error())
	}
}

func TestBusBroodOps_NoResponderRowCarriesTheCode(t *testing.T) {
	f := &busBroodOps{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(string, []byte, time.Duration) ([]byte, error) {
			return nil, nats.ErrNoResponders
		},
	}
	spec := map[string]v1alpha1.BroodOperationSpec{"member-b": {}}
	created := f.Create(context.Background(), []string{"member-b"}, "team-ns", "op", spec, "alice")
	canceled := f.Cancel(context.Background(), []string{"member-b"}, "team-ns", "op")
	previewed := f.Preview(context.Background(), []string{"member-b"}, "team-ns", spec)
	rows := []fanoutRow{
		{code: created[0].Code, err: created[0].Error},
		{code: canceled[0].Code, err: canceled[0].Error},
		{code: previewed[0].Code, err: previewed[0].Error},
	}
	for i, r := range rows {
		if r.code != codeOperatorUnavailable || r.err != noOperatorText("member-b") {
			t.Errorf("row %d = %+v, want the no-operator code and text", i, r)
		}
	}
}

func TestBroodHandlers_NoOperatorOnEveryFailedClusterIs503(t *testing.T) {
	body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"selector":{},"namespaces":["all"]}},"clusters":["core","member-b"]}`
	for _, path := range []string{"/brood-operations", "/brood-operations/preview"} {
		t.Run(path, func(t *testing.T) {
			ops := &countingBroodOps{noOperator: map[string]bool{"core": true, "member-b": true}}
			srv := newBroodTestServer()
			srv.deps.Brood = broodWithMembers()
			srv.deps.BroodOps = ops
			w := postBroodAsAdmin(srv, path, body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Code     string `json:"code"`
				Clusters []struct {
					Cluster string `json:"cluster"`
					Code    string `json:"code"`
				} `json:"clusters"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Code != codeOperatorUnavailable || len(resp.Clusters) != 2 {
				t.Fatalf("body = %s, want the operator_unavailable code and two rows", w.Body.String())
			}
			assertSpec503(t, w, http.MethodPost, "/api/v1"+path)
			for _, row := range resp.Clusters {
				if row.Code != codeOperatorUnavailable {
					t.Errorf("row %s code = %q, want %q", row.Cluster, row.Code, codeOperatorUnavailable)
				}
			}

			ops = &countingBroodOps{
				noOperator:  map[string]bool{"core": true},
				unreachable: map[string]bool{"member-b": true},
			}
			srv.deps.BroodOps = ops
			if w := postBroodAsAdmin(srv, path, body); w.Code != http.StatusBadGateway {
				t.Errorf("a timeout row with a no-operator row: status = %d, want 502: %s", w.Code, w.Body.String())
			}

			// The 503 of a missing store keeps the Error body, which the same
			// spec entry declares.
			srv.deps.BroodOps = nil
			w = postBroodAsAdmin(srv, path, body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("no store: status = %d, want 503: %s", w.Code, w.Body.String())
			}
			assertSpec503(t, w, http.MethodPost, "/api/v1"+path)
		})
	}
}
