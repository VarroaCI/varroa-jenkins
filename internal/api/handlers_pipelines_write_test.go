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

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// --- fakeTriggerTransport: a minimal, thread-safe transport.Transport ---
//
// Embedding transport.Transport (nil) means an unconfigured method
// panics on use, so a test depending on a method this fake does not
// model fails loudly rather than silently.
type fakeTriggerTransport struct {
	transport.Transport

	mu       sync.Mutex
	commands []triggerCall
	err      error
}

type triggerCall struct {
	ns, name string
	cmd      *mitev1.ImperativeCommand
}

func (f *fakeTriggerTransport) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, triggerCall{ns: ns, name: name, cmd: cmd})
	return nil
}

func (f *fakeTriggerTransport) calls() []triggerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]triggerCall, len(f.commands))
	copy(out, f.commands)
	return out
}

// writeTestServer assembles a *Server wired for the pipelines write
// paths: one controller tenant "test-ns/ci" (URL segment "ci", empty
// folder), granted read and write, over the caller's record store and
// mite transport.
func writeTestServer(reader recordstore.Reader, mt transport.Transport) *Server {
	return writeTestServerWithVerbs(reader, mt, []string{"read", "write"})
}

// writeTestServerWithVerbs is writeTestServer with the fixture tenant's
// own verbs, so a case can hold the read grant alone.
func writeTestServerWithVerbs(reader recordstore.Reader, mt transport.Transport, verbs []string) *Server {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ci": {ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "test-ns"}},
	}
	authz := pipelineGrantAuthorizer(verbs, "test-ns/ci")
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authz,
		RecordStore:       reader,
		TenantResolver:    NewTeamTenantResolver(store, brood, authz),
		MiteRegistry:      mt,
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	}
	return NewServer(deps)
}

// doWriteRequest builds a POST request for one of the three pipelines
// write paths: every caller in this file posts.
func doWriteRequest(path string, pathValues map[string]string, claims *auth.Claims, body any) *http.Request {
	var b bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		b = *bytes.NewReader(data)
	}
	req := httptest.NewRequest(http.MethodPost, path, &b)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	return req.WithContext(auth.ContextWithClaims(context.Background(), claims))
}

// postCreateRun posts a run request for the fixture tenant "ci" at job
// path "svc/main", the one team/job pair every createRun test in this
// file exercises.
func postCreateRun(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := doWriteRequest("/pipelines/ci/jobs/svc/main/runs",
		map[string]string{"team": "ci", "job": "svc/main"}, testClaims(), body)
	rec := httptest.NewRecorder()
	srv.handleCreateRun(rec, req)
	return rec
}

// uuidv7At builds a UUIDv7 string whose embedded time is t, for the
// stale/future test cases parseClientToken must reject.
func uuidv7At(t time.Time) string {
	var b [16]byte
	ms := t.UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 // version nibble (7) in the high bits of byte 6
	b[8] = 0x80 // variant bits (10xxxxxx) in byte 8
	id, err := uuid.FromBytes(b[:])
	if err != nil {
		panic(err)
	}
	return id.String()
}

// --- Task 4.1: tenant narrowing for a write ---

func TestSelectWriteTenant(t *testing.T) {
	teamA := TenantRef{Cluster: "core", Tenant: "team-a", Folder: "team-a"}
	teamB := TenantRef{Cluster: "hive", Tenant: "team-b", Folder: "team-b"}
	openA := TenantRef{Cluster: "core", Tenant: "open-a", Folder: ""}
	openB := TenantRef{Cluster: "hive", Tenant: "open-b", Folder: ""}

	cases := []struct {
		name        string
		refs        []TenantRef
		team        string
		jobPath     string
		cluster     string
		wantRef     TenantRef
		wantPath    string
		wantStatus  int
		wantMessage string
	}{
		{
			name:     "the long form already carries the tenant folder",
			refs:     []TenantRef{teamA, teamB},
			team:     "team-a",
			jobPath:  "team-a/checkout",
			wantRef:  teamA,
			wantPath: "team-a/checkout",
		},
		{
			name:     "the short form omits the tenant folder",
			refs:     []TenantRef{teamA},
			team:     "team-a",
			jobPath:  "checkout",
			wantRef:  teamA,
			wantPath: "team-a/checkout",
		},
		{
			name:     "the short form picks the candidate matching team, not the first one",
			refs:     []TenantRef{teamA, teamB},
			team:     "team-b",
			jobPath:  "checkout",
			wantRef:  teamB,
			wantPath: "team-b/checkout",
		},
		{
			name:       "an ambiguous write is refused, never guessed",
			refs:       []TenantRef{openA, openB},
			jobPath:    "svc/main",
			wantStatus: http.StatusConflict,
		},
		{
			name:    "a cluster value breaks a remaining tie",
			refs:    []TenantRef{openA, openB},
			jobPath: "svc/main",
			cluster: "hive",
			wantRef: openB,
		},
		{
			name:        "no surviving tenant answers 404 naming the expected shape",
			refs:        []TenantRef{teamA, teamB},
			team:        "team-c",
			jobPath:     "team-c/checkout",
			wantStatus:  http.StatusNotFound,
			wantMessage: `no tenant hosts job path "team-c/checkout"; expected <team>/<job path below the tenant folder>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, jobPath, serr := selectWriteTenant(tc.refs, tc.team, tc.jobPath, tc.cluster)
			if tc.wantStatus != 0 {
				if serr == nil {
					t.Fatalf("want status %d, got a resolved ref %+v", tc.wantStatus, ref)
				}
				if serr.Status != tc.wantStatus {
					t.Fatalf("status = %d, want %d (%s)", serr.Status, tc.wantStatus, serr.Message)
				}
				if tc.wantMessage != "" && serr.Message != tc.wantMessage {
					t.Fatalf("message = %q, want %q", serr.Message, tc.wantMessage)
				}
				return
			}
			if serr != nil {
				t.Fatalf("unexpected error: %v", serr)
			}
			if ref != tc.wantRef {
				t.Fatalf("ref = %+v, want %+v", ref, tc.wantRef)
			}
			if tc.wantPath != "" && jobPath != tc.wantPath {
				t.Fatalf("jobPath = %q, want %q", jobPath, tc.wantPath)
			}
		})
	}
}

// --- Task 4.2: the client-token validator ---

func TestParseClientToken(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "a valid token", token: uuidv7At(now.Add(-1 * time.Minute)), wantErr: false},
		{name: "a non-UUIDv7 string", token: "not-a-uuid", wantErr: true},
		{name: "a UUIDv4", token: uuid.New().String(), wantErr: true},
		{name: "a 48-hour-old token", token: uuidv7At(now.Add(-48 * time.Hour)), wantErr: true},
		{name: "a future token", token: uuidv7At(now.Add(1 * time.Hour)), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClientToken(tc.token, now)
			if tc.wantErr && err == nil {
				t.Fatalf("want an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- Task 4.3: POST /pipelines/{team}/jobs/{job}/runs ---

func TestHandleCreateRun_PublishesTheSuppliedToken(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	token, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint v7: %v", err)
	}
	rec := postCreateRun(t, srv, createRunRequest{ClientToken: token.String()})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID != token.String() {
		t.Fatalf("runId = %q, want the supplied token %q", resp.RunID, token.String())
	}
	wantIntentKey := "manual:" + token.String()
	if resp.IntentKey != wantIntentKey {
		t.Fatalf("intentKey = %q, want %q", resp.IntentKey, wantIntentKey)
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeTriggerBuild {
		t.Fatalf("command type = %v, want TRIGGER_BUILD", cmd.Type)
	}
	if cmd.TriggerBuild.RunId != token.String() {
		t.Fatalf("published run_id = %q, want the token %q", cmd.TriggerBuild.RunId, token.String())
	}
	if cmd.TriggerBuild.IntentKey != wantIntentKey {
		t.Fatalf("published intent_key = %q, want %q", cmd.TriggerBuild.IntentKey, wantIntentKey)
	}
}

// placedTeamWriteServer assembles a *Server wired for a placed pipelines
// Team named "payments" whose folder is "payments", granted read and
// write, over the caller's mite transport. It mirrors writeTestServer
// but exercises the placed-Team path selectWriteTenant's short-form fix
// covers, rather than a folder-less synthetic controller tenant.
func placedTeamWriteServer(t *testing.T, mt transport.Transport) *Server {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"cell-a": {ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "varroa-cells"}},
	}
	store := storeFromFake(client)
	seedTeam(t, store, placedTeamObject("payments", "core", "varroa-cells", "cell-a"))
	authz := pipelineGrantAuthorizer([]string{"read", "write"}, "payments")
	brood := newFakeBrood(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authz,
		RecordStore:       newFakeRecordReader(),
		TenantResolver:    NewTeamTenantResolver(store, brood, authz),
		MiteRegistry:      mt,
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	}
	return NewServer(deps)
}

// TestHandleCreateRun_AcceptsBothJobPathForms asserts POST
// /pipelines/{team}/jobs/{job}/runs resolves a placed Team's tenant, and
// publishes the same folder-qualified job path, whether the caller
// repeats the tenant folder (the long form) or omits it (the short
// form).
func TestHandleCreateRun_AcceptsBothJobPathForms(t *testing.T) {
	cases := []struct {
		name string
		job  string
	}{
		{name: "the long form repeats the tenant folder", job: "payments/checkout/build"},
		{name: "the short form omits the tenant folder", job: "checkout/build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mt := &fakeTriggerTransport{}
			srv := placedTeamWriteServer(t, mt)
			req := doWriteRequest("/pipelines/payments/jobs/"+tc.job+"/runs",
				map[string]string{"team": "payments", "job": tc.job}, testClaims(), createRunRequest{})
			rec := httptest.NewRecorder()
			srv.handleCreateRun(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
			}
			calls := mt.calls()
			if len(calls) != 1 {
				t.Fatalf("published %d commands, want 1", len(calls))
			}
			const wantJobPath = "payments/checkout/build"
			if got := calls[0].cmd.TriggerBuild.JobPath; got != wantJobPath {
				t.Fatalf("published job_path = %q, want %q", got, wantJobPath)
			}
		})
	}
}

// fixedTenantResolver answers Resolve with a fixed ref list for any
// team, so a handler test can drive selectWriteTenant's own decisions
// (including the 404 no candidate can reach) without depending on
// TeamTenantResolver's folder-equals-team invariant.
type fixedTenantResolver struct {
	refs []TenantRef
}

func (f fixedTenantResolver) Resolve(context.Context, *auth.Claims, string) ([]TenantRef, error) {
	return f.refs, nil
}

func (f fixedTenantResolver) ResolveRun(context.Context, *auth.Claims, recordstore.Run) (TenantRef, error) {
	return TenantRef{}, nil
}

// TestHandleCreateRun_UnresolvedJobPathNamesTheExpectedShape asserts a
// job path no candidate tenant can host, in either form, answers 404
// naming the accepted shape rather than a bare "not found".
func TestHandleCreateRun_UnresolvedJobPathNamesTheExpectedShape(t *testing.T) {
	deps := &Dependencies{
		RecordStore: newFakeRecordReader(),
		TenantResolver: fixedTenantResolver{refs: []TenantRef{
			{Cluster: "core", Tenant: "team-a", Folder: "team-a"},
			{Cluster: "core", Tenant: "team-b", Folder: "team-b"},
		}},
		Logger: slog.Default(),
	}
	srv := NewServer(deps)

	req := doWriteRequest("/pipelines/payments/jobs/other-team/checkout/runs",
		map[string]string{"team": "payments", "job": "other-team/checkout"}, testClaims(), createRunRequest{})
	rec := httptest.NewRecorder()
	srv.handleCreateRun(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	const wantSubstring = "expected <team>/<job path below the tenant folder>"
	if !strings.Contains(body.Error, wantSubstring) {
		t.Fatalf("error = %q, want it to contain %q", body.Error, wantSubstring)
	}
}

func TestHandleCreateRun_RepeatedTokenYieldsOneIntentKeyAndOneRunID(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	token, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint v7: %v", err)
	}

	var runIDs, intentKeys []string
	for i := 0; i < 2; i++ {
		rec := postCreateRun(t, srv, createRunRequest{ClientToken: token.String()})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: status = %d, want 202", i, rec.Code)
		}
		var resp runActionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		runIDs = append(runIDs, resp.RunID)
		intentKeys = append(intentKeys, resp.IntentKey)
	}
	if runIDs[0] != runIDs[1] {
		t.Fatalf("runIds differ across requests: %q vs %q", runIDs[0], runIDs[1])
	}
	if intentKeys[0] != intentKeys[1] {
		t.Fatalf("intentKeys differ across requests: %q vs %q", intentKeys[0], intentKeys[1])
	}

	calls := mt.calls()
	if len(calls) != 2 {
		t.Fatalf("published %d commands, want 2 (the BFF never claims, so it publishes on every request)", len(calls))
	}
	if calls[0].cmd.TriggerBuild.IntentKey != calls[1].cmd.TriggerBuild.IntentKey {
		t.Fatalf("published intent keys differ: %q vs %q", calls[0].cmd.TriggerBuild.IntentKey, calls[1].cmd.TriggerBuild.IntentKey)
	}
}

func TestHandleCreateRun_NoClientTokenMintsOne(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	rec := postCreateRun(t, srv, createRunRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := uuid.Parse(resp.RunID); err != nil {
		t.Fatalf("runId %q is not a UUID: %v", resp.RunID, err)
	}
}

func TestHandleCreateRun_InvalidTokenIs400(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	rec := postCreateRun(t, srv, createRunRequest{ClientToken: "not-a-uuid"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("a rejected token published %d commands, want 0", len(mt.calls()))
	}
}

// --- Task 4.4: ten concurrent requests, one client token, two handlers ---

func TestHandleCreateRun_ConcurrentRequestsAgreeOnOneIdentifier(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	srv1 := writeTestServer(reader, mt)
	srv2 := writeTestServer(reader, mt)

	token, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint v7: %v", err)
	}

	const n = 10
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv := srv1
			if i%2 == 1 {
				srv = srv2
			}
			results[i] = postCreateRun(t, srv, createRunRequest{ClientToken: token.String()})
		}(i)
	}
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: status = %d, want 202", i, rec.Code)
		}
		var resp runActionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("request %d decode: %v", i, err)
		}
		if resp.RunID != token.String() {
			t.Fatalf("request %d: runId = %q, want the token %q", i, resp.RunID, token.String())
		}
	}

	calls := mt.calls()
	if len(calls) != n {
		t.Fatalf("published %d commands, want %d", len(calls), n)
	}
	wantIntentKey := "manual:" + token.String()
	for i, c := range calls {
		if c.cmd.TriggerBuild.IntentKey != wantIntentKey {
			t.Fatalf("call %d: intent_key = %q, want %q", i, c.cmd.TriggerBuild.IntentKey, wantIntentKey)
		}
	}
}

// --- Task 4.5: cancel and rerun ---

func TestHandleCancelRun_PublishesTheRunsIdentity(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	buildNumber := int64(7)
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "RUNNING", BuildNumber: &buildNumber}
	srv := writeTestServer(reader, mt)

	req := doWriteRequest("/runs/r1/cancel", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleCancelRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeCancelBuild {
		t.Fatalf("command type = %v, want CANCEL_BUILD", cmd.Type)
	}
	if cmd.CancelBuild.RunId != "r1" || cmd.CancelBuild.JobPath != "svc/main" || cmd.CancelBuild.BuildNumber != 7 {
		t.Fatalf("cancel payload = %+v, want run_id=r1 job_path=svc/main build_number=7", cmd.CancelBuild)
	}
	if want := ActorFrom(testClaims()); want == "" || cmd.CancelBuild.Actor != want {
		t.Fatalf("cancel actor = %q, want the caller %q", cmd.CancelBuild.Actor, want)
	}
}

func TestHandleCancelRun_CrossTenantIs403(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "other-ns/other-cell", JobPath: "svc/main", Status: "RUNNING"}
	srv := writeTestServer(reader, mt)

	req := doWriteRequest("/runs/r1/cancel", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleCancelRun(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("a forbidden cancel published %d commands, want 0", len(mt.calls()))
	}
}

func TestHandleRerunRun_CopiesParamsAndSetsTheRetryKey(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "COMPLETED",
		Params: json.RawMessage(`{"branch":"main"}`),
	}
	srv := writeTestServer(reader, mt)

	req := doWriteRequest("/runs/r1/rerun", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleRerunRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.IntentKey != "retry:r1" {
		t.Fatalf("intentKey = %q, want retry:r1", resp.IntentKey)
	}
	if _, err := uuid.Parse(resp.RunID); err != nil {
		t.Fatalf("runId %q is not a UUID: %v", resp.RunID, err)
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd.TriggerBuild
	if cmd.IntentKey != "retry:r1" {
		t.Fatalf("published intent_key = %q, want retry:r1", cmd.IntentKey)
	}
	if cmd.Parameters["branch"] != "main" {
		t.Fatalf("published parameters = %+v, want the source run's branch=main copied", cmd.Parameters)
	}
}

func TestHandleRerunRun_ExistingIntentIsReusedAndNothingIsPublished(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "COMPLETED"}
	reader.intents["retry:r1"] = recordstore.Intent{IntentKey: "retry:r1", RunID: "r2", State: "granted"}
	srv := writeTestServer(reader, mt)

	req := doWriteRequest("/runs/r1/rerun", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleRerunRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RunID != "r2" {
		t.Fatalf("runId = %q, want the existing retry's r2", resp.RunID)
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0 (an existing intent publishes nothing)", len(mt.calls()))
	}
}

func TestHandleRerunRun_LookupWritesNoIntent(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "COMPLETED"}
	srv := writeTestServer(reader, mt)

	before := len(reader.intents)
	req := doWriteRequest("/runs/r1/rerun", map[string]string{"run": "r1"}, testClaims(), nil)
	rec := httptest.NewRecorder()
	srv.handleRerunRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(reader.intents) != before {
		t.Fatalf("the retry lookup wrote an intent: had %d, now %d", before, len(reader.intents))
	}
}

// --- Task 4.6: the BFF never claims the ledger from any pipelines path ---

// TestPipelinesNoLedgerClaim exercises every pipelines write path over a
// Server whose BusConn is nil. The only NATS surface the ledger claim
// travels over is deps.BusConn (internal/bus.Conn); the imperative
// commands above publish through deps.MiteRegistry instead. A pipelines
// handler that ever touched the ledger's claim subject would need
// deps.BusConn and would nil-panic here, so this test's mere completion
// is the proof: no pipelines write path sends anything on
// bus.LedgerClaimSubject.
func TestPipelinesNoLedgerClaim(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "RUNNING"}
	srv := writeTestServer(reader, mt)
	srv.deps.BusConn = nil

	token, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint v7: %v", err)
	}
	if rec := postCreateRun(t, srv, createRunRequest{ClientToken: token.String()}); rec.Code != http.StatusAccepted {
		t.Fatalf("createRun status = %d, want 202", rec.Code)
	}

	cancelReq := doWriteRequest("/runs/r1/cancel", map[string]string{"run": "r1"}, testClaims(), nil)
	cancelRec := httptest.NewRecorder()
	srv.handleCancelRun(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusAccepted {
		t.Fatalf("cancelRun status = %d, want 202", cancelRec.Code)
	}

	rerunReq := doWriteRequest("/runs/r1/rerun", map[string]string{"run": "r1"}, testClaims(), nil)
	rerunRec := httptest.NewRecorder()
	srv.handleRerunRun(rerunRec, rerunReq)
	if rerunRec.Code != http.StatusAccepted {
		t.Fatalf("rerunRun status = %d, want 202", rerunRec.Code)
	}
}

// --- The two run input routes ---

// actingClaims is testClaims plus the identity and the groups the two
// input routes carry into the command.
func actingClaims() *auth.Claims {
	return &auth.Claims{
		Subject:           "u",
		PreferredUsername: "ada",
		Email:             "ada@example.test",
		Groups:            []string{"deployers"},
	}
}

// runInputServer wires a server over one RUNNING run of the fixture
// tenant, carrying the build number an input needs, with the caller's
// own verbs for that tenant. The run's own namespace and cell columns
// name the member the command must reach.
func runInputServer(t *testing.T, mt transport.Transport, verbs []string) *Server {
	t.Helper()
	reader := newFakeRecordReader()
	buildNumber := int64(7)
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Cluster: "core", Namespace: "run-ns", Cell: "run-cell",
		Tenant: "test-ns/ci", JobPath: "svc/main", Status: "RUNNING", BuildNumber: &buildNumber,
	}
	return writeTestServerWithVerbs(reader, mt, verbs)
}

// postRunInput posts to one of the two input routes for the given run and
// input identifiers.
func postRunInput(srv *Server, runID, inputID, action string, claims *auth.Claims, body any) *httptest.ResponseRecorder {
	req := doWriteRequest("/runs/"+runID+"/inputs/"+inputID+"/"+action,
		map[string]string{"run": runID, "input": inputID}, claims, body)
	rec := httptest.NewRecorder()
	if action == "proceed" {
		srv.handleProceedRunInput(rec, req)
	} else {
		srv.handleAbortRunInput(rec, req)
	}
	return rec
}

func TestHandleProceedRunInput_PublishesTheCommandToTheRunsOwnTenant(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := runInputServer(t, mt, []string{"read", "write"})

	rec := postRunInput(srv, "r1", "in-1", "proceed", actingClaims(),
		proceedRunInputRequest{Parameters: map[string]string{"env": "prod"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != "requested" {
		t.Fatalf("state = %q, want requested", body["state"])
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	if calls[0].ns != "run-ns" || calls[0].name != "run-cell" {
		t.Fatalf("published to %s/%s, want the run's own run-ns/run-cell", calls[0].ns, calls[0].name)
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeProceedInput {
		t.Fatalf("command type = %v, want PROCEED_INPUT", cmd.Type)
	}
	p := cmd.ProceedInput
	if p == nil {
		t.Fatalf("the proceed payload is nil")
	}
	if p.RunId != "r1" || p.JobPath != "svc/main" || p.BuildNumber != 7 || p.InputId != "in-1" {
		t.Fatalf("payload = %+v, want run_id=r1 job_path=svc/main build_number=7 input_id=in-1", p)
	}
	if p.Parameters["env"] != "prod" {
		t.Fatalf("payload parameters = %+v, want env=prod", p.Parameters)
	}
	if p.ActingUser == nil {
		t.Fatalf("the command carries no acting user")
	}
	if p.ActingUser.Name != "ada" || p.ActingUser.Email != "ada@example.test" {
		t.Fatalf("acting user = %+v, want the caller's own name and email", p.ActingUser)
	}
	if len(p.ActingUser.Groups) != 1 || p.ActingUser.Groups[0] != "deployers" {
		t.Fatalf("acting user groups = %v, want [deployers]", p.ActingUser.Groups)
	}
}

func TestHandleAbortRunInput_PublishesTheActingUser(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := runInputServer(t, mt, []string{"read", "write"})

	rec := postRunInput(srv, "r1", "in-1", "abort", actingClaims(), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeAbortInput {
		t.Fatalf("command type = %v, want ABORT_INPUT", cmd.Type)
	}
	p := cmd.AbortInput
	if p == nil {
		t.Fatalf("the abort payload is nil")
	}
	if p.RunId != "r1" || p.JobPath != "svc/main" || p.BuildNumber != 7 || p.InputId != "in-1" {
		t.Fatalf("payload = %+v, want run_id=r1 job_path=svc/main build_number=7 input_id=in-1", p)
	}
	if p.ActingUser == nil || p.ActingUser.Name != "ada" {
		t.Fatalf("acting user = %+v, want the caller's own name", p.ActingUser)
	}
}

func TestHandleRunInputRoutes_ReadOnlyCallerIs403(t *testing.T) {
	for _, action := range []string{"proceed", "abort"} {
		t.Run(action, func(t *testing.T) {
			mt := &fakeTriggerTransport{}
			srv := runInputServer(t, mt, []string{"read"})

			rec := postRunInput(srv, "r1", "in-1", action, actingClaims(), proceedRunInputRequest{})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
			}
			if n := len(mt.calls()); n != 0 {
				t.Fatalf("a forbidden call published %d commands, want 0", n)
			}
		})
	}
}

func TestHandleRunInputRoutes_UnknownRunIs404(t *testing.T) {
	for _, action := range []string{"proceed", "abort"} {
		t.Run(action, func(t *testing.T) {
			mt := &fakeTriggerTransport{}
			srv := runInputServer(t, mt, []string{"read", "write"})

			rec := postRunInput(srv, "absent", "in-1", action, actingClaims(), proceedRunInputRequest{})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
			}
			if n := len(mt.calls()); n != 0 {
				t.Fatalf("an unknown run published %d commands, want 0", n)
			}
		})
	}
}

func TestHandleRunInputRoutes_EmptyInputIdIs400(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := runInputServer(t, mt, []string{"read", "write"})

	rec := postRunInput(srv, "r1", "", "proceed", actingClaims(), proceedRunInputRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if n := len(mt.calls()); n != 0 {
		t.Fatalf("an empty input id published %d commands, want 0", n)
	}
}

func TestHandleRunInputRoutes_RunWithoutBuildNumberIs404(t *testing.T) {
	mt := &fakeTriggerTransport{}
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "QUEUED"}
	srv := writeTestServer(reader, mt)

	rec := postRunInput(srv, "r1", "in-1", "proceed", actingClaims(), proceedRunInputRequest{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	if n := len(mt.calls()); n != 0 {
		t.Fatalf("a run with no build number published %d commands, want 0", n)
	}
}

// TestRunForWriteStatuses_UnchangedByTheSplit asserts the run lookup the
// two input routes share answers the statuses the cancel and rerun paths
// answered before runForWrite was split out of it.
func TestRunForWriteStatuses_UnchangedByTheSplit(t *testing.T) {
	schemaTooOld := newFakeRecordReader()
	schemaTooOld.schemaTooOld = true

	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ci", JobPath: "svc/main", Status: "RUNNING"}
	reader.runs["cross"] = recordstore.Run{RunID: "cross", Tenant: "other-ns/other-cell", JobPath: "svc/main", Status: "RUNNING"}

	cases := []struct {
		name  string
		srv   *Server
		runID string
		want  int
	}{
		{name: "an unknown run", srv: writeTestServer(reader, &fakeTriggerTransport{}), runID: "absent", want: http.StatusNotFound},
		{name: "a run of another tenant", srv: writeTestServer(reader, &fakeTriggerTransport{}), runID: "cross", want: http.StatusForbidden},
		{name: "a record store that is too old", srv: writeTestServer(schemaTooOld, &fakeTriggerTransport{}), runID: "r1", want: http.StatusServiceUnavailable},
		{name: "no tenant resolver", srv: NewServer(&Dependencies{RecordStore: reader, Logger: slog.Default()}), runID: "r1", want: http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := doWriteRequest("/runs/"+tc.runID+"/cancel", map[string]string{"run": tc.runID}, testClaims(), nil)
			rec := httptest.NewRecorder()
			tc.srv.handleCancelRun(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
