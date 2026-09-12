package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ci": {ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "test-ns"}},
	}
	authz := pipelineGrantAuthorizer([]string{"read", "write"}, "test-ns/ci")
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
		name       string
		refs       []TenantRef
		jobPath    string
		cluster    string
		wantRef    TenantRef
		wantStatus int
	}{
		{
			name:    "one surviving tenant is used",
			refs:    []TenantRef{teamA, teamB},
			jobPath: "team-a/checkout",
			wantRef: teamA,
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
			name:       "no surviving tenant answers 404",
			refs:       []TenantRef{teamA, teamB},
			jobPath:    "team-c/checkout",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, serr := selectWriteTenant(tc.refs, tc.jobPath, tc.cluster)
			if tc.wantStatus != 0 {
				if serr == nil {
					t.Fatalf("want status %d, got a resolved ref %+v", tc.wantStatus, ref)
				}
				if serr.Status != tc.wantStatus {
					t.Fatalf("status = %d, want %d (%s)", serr.Status, tc.wantStatus, serr.Message)
				}
				return
			}
			if serr != nil {
				t.Fatalf("unexpected error: %v", serr)
			}
			if ref != tc.wantRef {
				t.Fatalf("ref = %+v, want %+v", ref, tc.wantRef)
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
