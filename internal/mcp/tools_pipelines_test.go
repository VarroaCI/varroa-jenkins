package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// --- fakePipelineReader: a minimal recordstore.Reader for the pipeline
// tool tests. Embeds the interface (nil) so an unmodeled method panics
// on use rather than silently returning a zero value. ---

type fakePipelineReader struct {
	recordstore.Reader

	runs    map[string]recordstore.Run
	stages  map[string][]recordstore.Stage
	intents map[string]recordstore.Intent

	// lastQuery captures the RunQuery ListRuns was last called with, so
	// a test can assert on the clamped Limit without a real store.
	lastQuery recordstore.RunQuery
	// listErr, when set, is returned by ListRuns instead of a page.
	listErr error
}

// JobHome answers no placement row, so the trigger write keeps the tenant
// reference the resolver handed it. These tests assert the tool's own
// answer, not the placement.
func (f *fakePipelineReader) JobHome(_ context.Context, _, _, _ string) (recordstore.JobHome, error) {
	return recordstore.JobHome{}, nil
}

func newFakePipelineReader() *fakePipelineReader {
	return &fakePipelineReader{
		runs:    map[string]recordstore.Run{},
		stages:  map[string][]recordstore.Stage{},
		intents: map[string]recordstore.Intent{},
	}
}

func (f *fakePipelineReader) GetRun(_ context.Context, runID string) (recordstore.Run, []recordstore.Stage, error) {
	run, ok := f.runs[runID]
	if !ok {
		return recordstore.Run{}, nil, recordstore.ErrRunNotFound
	}
	return run, f.stages[runID], nil
}

func (f *fakePipelineReader) ListRuns(_ context.Context, q recordstore.RunQuery) (recordstore.RunPage, error) {
	f.lastQuery = q
	if f.listErr != nil {
		return recordstore.RunPage{}, f.listErr
	}
	return recordstore.RunPage{}, nil
}

func (f *fakePipelineReader) GetIntentByKey(_ context.Context, key string) (recordstore.Intent, bool, error) {
	intent, ok := f.intents[key]
	return intent, ok, nil
}

// --- fakePipelineTenantResolver: a fixed-answer api.TenantResolver.
// Every team resolves to refs, unconditionally — the pipeline tool
// tests exercise Authorizer's own tenant/job decision, not
// TenantResolver's candidate search. ---

type fakePipelineTenantResolver struct {
	refs []api.TenantRef
}

func (f *fakePipelineTenantResolver) Resolve(_ context.Context, _ *auth.Claims, _ string) ([]api.TenantRef, error) {
	return f.refs, nil
}

func (f *fakePipelineTenantResolver) ResolveRun(_ context.Context, _ *auth.Claims, r recordstore.Run) (api.TenantRef, error) {
	ref, err := api.TenantRefFromRow(r.Cluster, r.Namespace, r.Cell, r.Tenant)
	if err != nil {
		return api.TenantRef{}, err
	}
	return ref, nil
}

// --- fakePipelineTransport: a transport.Transport spy recording every
// SendImperative call, so a test can assert a command was (or was not)
// published. ---

type fakePipelineTransport struct {
	transport.Transport
	sent []*mitev1.ImperativeCommand
}

func (f *fakePipelineTransport) SendImperative(_ context.Context, _, _ string, cmd *mitev1.ImperativeCommand) error {
	f.sent = append(f.sent, cmd)
	return nil
}

// --- fakeLogBlobStore: a blob.Store serving one fixed console object
// for every key, native-object style (offset/length honored, no
// chunking). ---

type fakeLogBlobStore struct {
	blob.Store
	data []byte
}

func (f *fakeLogBlobStore) NewRangeReader(_ context.Context, _ string, offset, length int64) (io.ReadCloser, error) {
	end := int64(len(f.data))
	if offset > end {
		offset = end
	}
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(f.data[offset:end])), nil
}

// pipelinesTestAuthorizer grants verb on "pipelines" for tenant
// "team-a" only, bound to claims{Subject: "u"}, matching
// authorizer_tenant_test.go's pattern in internal/api. Every test in
// this file scopes to the one tenant "team-a", so the tenant name is
// not a parameter.
func pipelinesTestAuthorizer(verb string) *api.Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "role"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{verb}}}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "role",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		},
	}}
	return api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var pipelinesTestClaims = &auth.Claims{Subject: "u"}

func callPipelineTool(t *testing.T, deps *api.Dependencies, name string, args map[string]interface{}) toolResult {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	}, pipelinesTestClaims)
	return parseToolResult(t, resp.Result)
}

// --- Task 9.4: list_runs clamps limit at 200. ---

func TestListRunsClampsLimitAt200(t *testing.T) {
	reader := newFakePipelineReader()
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
	}

	tr := callPipelineTool(t, deps, "list_runs", map[string]interface{}{
		"team": "team-a", "job": "team-a/build", "limit": float64(1000),
	})
	if tr.IsError {
		t.Fatalf("list_runs returned error: %v", tr.Content)
	}
	if reader.lastQuery.Limit != 200 {
		t.Errorf("ListRuns Limit = %d, want 200 (clamped)", reader.lastQuery.Limit)
	}

	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.Count != 0 {
		t.Errorf("count = %d, want 0 for an empty page", payload.Count)
	}
}

// --- Task 9.4: run_pipeline returns runId and intentKey. ---

func TestRunPipelineReturnsRunIDAndIntentKey(t *testing.T) {
	reader := newFakePipelineReader()
	sent := &fakePipelineTransport{}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("write"),
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
		MiteRegistry:   sent,
	}

	tr := callPipelineTool(t, deps, "run_pipeline", map[string]interface{}{
		"team": "team-a", "job": "team-a/build",
	})
	if tr.IsError {
		t.Fatalf("run_pipeline returned error: %v", tr.Content)
	}

	var payload struct {
		RunID     string `json:"runId"`
		IntentKey string `json:"intentKey"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.RunID == "" {
		t.Error("runId is empty")
	}
	if payload.IntentKey != "manual:"+payload.RunID {
		t.Errorf("intentKey = %q, want %q", payload.IntentKey, "manual:"+payload.RunID)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("published %d commands, want 1", len(sent.sent))
	}
	if sent.sent[0].TriggerBuild.RunId != payload.RunID {
		t.Errorf("published command RunId = %q, want %q", sent.sent[0].TriggerBuild.RunId, payload.RunID)
	}
}

// run_pipeline accepts a job path that omits the tenant folder, the
// short form, and publishes the same folder-qualified job path the long
// form would.
func TestRunPipelineAcceptsTheShortFormJobPath(t *testing.T) {
	reader := newFakePipelineReader()
	sent := &fakePipelineTransport{}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("write"),
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
		MiteRegistry:   sent,
	}

	tr := callPipelineTool(t, deps, "run_pipeline", map[string]interface{}{
		"team": "team-a", "job": "build",
	})
	if tr.IsError {
		t.Fatalf("run_pipeline returned error: %v", tr.Content)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("published %d commands, want 1", len(sent.sent))
	}
	const wantJobPath = "team-a/build"
	if got := sent.sent[0].TriggerBuild.JobPath; got != wantJobPath {
		t.Errorf("published job_path = %q, want %q", got, wantJobPath)
	}
}

// A read-only caller cannot start a build (mcp-jenkins-proxy scenario).
func TestRunPipelineDeniesReadOnlyCaller(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    newFakePipelineReader(),
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
		MiteRegistry:   sent,
	}

	tr := callPipelineTool(t, deps, "run_pipeline", map[string]interface{}{
		"team": "team-a", "job": "team-a/build",
	})
	if !tr.IsError {
		t.Fatal("expected run_pipeline to deny a read-only caller")
	}
	if len(sent.sent) != 0 {
		t.Errorf("published %d commands, want 0", len(sent.sent))
	}
}

// --- Task 9.4: a caller permitted on one tenant of a shared cell gets a
// not-found error for a second tenant's run. ---

func TestGetRunNotFoundForUnreadableTenant(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	reader.runs["run-b"] = recordstore.Run{RunID: "run-b", Tenant: "team-b", JobPath: "team-b/build", Status: "COMPLETED"}
	deps := &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
	}

	if tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"}); tr.IsError {
		t.Fatalf("get_run for the permitted tenant returned error: %v", tr.Content)
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-b"})
	if !tr.IsError {
		t.Fatal("expected get_run for the second tenant's run to return an error")
	}
	if got := toolText(tr); got != "run not found" {
		t.Errorf("error text = %q, want %q", got, "run not found")
	}
}

// --- Task 9.6: get_run_log's bounded tail. ---

func fakeConsoleLog(lines int) []byte {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func getRunLogDeps(reader *fakePipelineReader, store *fakeLogBlobStore) *api.Dependencies {
	return &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
		BlobStore:   store,
	}
}

func TestGetRunLogDefaultTailIs100Lines(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	store := &fakeLogBlobStore{data: fakeConsoleLog(600)}

	tr := callPipelineTool(t, getRunLogDeps(reader, store), "get_run_log", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run_log returned error: %v", tr.Content)
	}
	var payload struct {
		Text       string `json:"text"`
		TotalLines int64  `json:"totalLines"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.TotalLines != 600 {
		t.Errorf("totalLines = %d, want 600", payload.TotalLines)
	}
	got := strings.Split(payload.Text, "\n")
	if len(got) != 100 {
		t.Fatalf("returned %d lines, want 100 (the default tail)", len(got))
	}
	if got[0] != "line 501" {
		t.Errorf("first tail line = %q, want %q", got[0], "line 501")
	}
}

func TestGetRunLogClampsTailLinesAt500(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	store := &fakeLogBlobStore{data: fakeConsoleLog(600)}

	tr := callPipelineTool(t, getRunLogDeps(reader, store), "get_run_log", map[string]interface{}{
		"run": "run-a", "tailLines": float64(5000),
	})
	if tr.IsError {
		t.Fatalf("get_run_log returned error: %v", tr.Content)
	}
	var payload struct {
		Text       string `json:"text"`
		TotalLines int64  `json:"totalLines"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	got := strings.Split(payload.Text, "\n")
	if len(got) != 500 {
		t.Fatalf("returned %d lines, want 500 (clamped)", len(got))
	}
	if got[0] != "line 101" {
		t.Errorf("first tail line = %q, want %q", got[0], "line 101")
	}
}

// get_run_log starts no live tail: the run is still RUNNING, and the
// handler reads only the record store and the blob store. BusConn is
// left unset (a nil *bus.Conn); the only place a tail-control request
// could be sent is runTailRegistry.attach, which dereferences conn and
// would panic here if GetRunLog ever reached it.
func TestGetRunLogStartsNoTail(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "RUNNING"}
	store := &fakeLogBlobStore{data: fakeConsoleLog(10)}

	tr := callPipelineTool(t, getRunLogDeps(reader, store), "get_run_log", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run_log returned error: %v", tr.Content)
	}
}
