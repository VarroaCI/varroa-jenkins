package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	// downstream is keyed by upstream run id, for ListDownstreamRuns.
	downstream map[string][]recordstore.Run

	// locksByTenant is keyed by tenant, for ListFleetLocks. Every test
	// in this file scopes to one tenant, so the cluster is not part of
	// the key.
	locksByTenant map[string][]recordstore.FleetLock

	// lastQuery captures the RunQuery ListRuns was last called with, so
	// a test can assert on the clamped Limit without a real store.
	lastQuery recordstore.RunQuery
	// listErr, when set, is returned by ListRuns instead of a page.
	listErr error

	// testCases is keyed by run id, for ListTestCases.
	testCases map[string][]recordstore.TestCase
}

// ListTestCases answers the seeded case rows of one run.
func (f *fakePipelineReader) ListTestCases(_ context.Context, runID string) ([]recordstore.TestCase, error) {
	return f.testCases[runID], nil
}

// JobHome answers no placement row, so the trigger write keeps the tenant
// reference the resolver handed it. These tests assert the tool's own
// answer, not the placement.
func (f *fakePipelineReader) JobHome(_ context.Context, _, _, _ string) (recordstore.JobHome, error) {
	return recordstore.JobHome{}, nil
}

// JobInventory answers the zero inventory, which holds no known set, so the
// scan's target check passes for the fixture's job.
func (f *fakePipelineReader) JobInventory(_ context.Context, _, _ string) (recordstore.JobInventory, error) {
	return recordstore.JobInventory{}, nil
}

func newFakePipelineReader() *fakePipelineReader {
	return &fakePipelineReader{
		runs:          map[string]recordstore.Run{},
		stages:        map[string][]recordstore.Stage{},
		intents:       map[string]recordstore.Intent{},
		locksByTenant: map[string][]recordstore.FleetLock{},
		testCases:     map[string][]recordstore.TestCase{},
		downstream:    map[string][]recordstore.Run{},
	}
}

// ListDownstreamRuns answers every seeded run of one upstream run in one
// page. The MCP tests seed few runs, so no cursor is needed.
func (f *fakePipelineReader) ListDownstreamRuns(_ context.Context, q recordstore.DownstreamRunQuery) (recordstore.RunPage, error) {
	return recordstore.RunPage{Runs: append([]recordstore.Run{}, f.downstream[q.UpstreamRunID]...)}, nil
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

// --- fakeLogBlobStore: a blob.Store serving one fixed console object as
// a single log part at sequence 0, with no index. The BFF reader reads
// the index first and lists the parts above it, so the fake answers
// those calls too. ---

type fakeLogBlobStore struct {
	blob.Store
	data []byte
}

func (f *fakeLogBlobStore) NewRangeReader(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "logindex/") {
		return nil, blob.ErrObjectNotFound
	}
	end := int64(len(f.data))
	if offset > end {
		offset = end
	}
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(f.data[offset:end])), nil
}

func (f *fakeLogBlobStore) Parts(_ context.Context, _ string, from int64) ([]blob.Part, error) {
	if from > 0 || len(f.data) == 0 {
		return []blob.Part{}, nil
	}
	return []blob.Part{{Seq: 0, Size: int64(len(f.data))}}, nil
}

func (f *fakeLogBlobStore) PartStamp(_ context.Context, key string, seq int64) (blob.Stamp, error) {
	if strings.HasPrefix(key, "logindex/") || seq != 0 || len(f.data) == 0 {
		return blob.Stamp{}, blob.ErrObjectNotFound
	}
	return blob.Stamp{Digest: blob.Digest(f.data)}, nil
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
		State     string `json:"state"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.RunID == "" {
		t.Error("runId is empty")
	}
	if payload.State != "requested" {
		t.Errorf("state = %q, want %q", payload.State, "requested")
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

// TestGetRunToolCarriesUpstream asserts get_run carries the same upstream
// and downstream links as GET /runs/{run}.
func TestGetRunToolCarriesUpstream(t *testing.T) {
	reader := newFakePipelineReader()
	upstreamID := "0194f6b2-0000-7000-8000-000000000001"
	upTenant, upJob := "team-a", "team-a/build"
	upNumber := int64(4)
	upRun := uuid.MustParse(upstreamID)
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED",
		UpstreamRunID: &upRun, UpstreamTenant: &upTenant, UpstreamJob: &upJob, UpstreamNumber: &upNumber,
	}
	reader.downstream["run-a"] = []recordstore.Run{{
		RunID: "run-c", Tenant: "team-a", JobPath: "team-a/deploy", Status: "COMPLETED",
	}}
	deps := &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var detail struct {
		Upstream *struct {
			JobPath     string `json:"jobPath"`
			RunID       string `json:"runId"`
			BuildNumber int64  `json:"buildNumber"`
		} `json:"upstream"`
		Downstream *struct {
			Items []struct {
				RunID string `json:"runId"`
			} `json:"items"`
		} `json:"downstream"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &detail); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if detail.Upstream == nil || detail.Upstream.RunID != upstreamID || detail.Upstream.JobPath != upJob || detail.Upstream.BuildNumber != 4 {
		t.Fatalf("upstream = %+v, want run %s on %s #4", detail.Upstream, upstreamID, upJob)
	}
	if detail.Downstream == nil || len(detail.Downstream.Items) != 1 || detail.Downstream.Items[0].RunID != "run-c" {
		t.Fatalf("downstream = %+v, want run-c", detail.Downstream)
	}
}

// --- The get_run tool reads the same record as GET /runs/{run}, so its
// result carries the same source object. ---

func TestGetRunCarriesSourceObject(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED",
		Source: json.RawMessage(`{"definition_path":"ci/Jenkinsfile","libraries":[{"name":"acme","version":"1.2.3"}],"pod_templates":[{"name":"maven","image":"maven:3.9.6"}]}`),
	}
	deps := &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var detail struct {
		Source *struct {
			DefinitionPath string `json:"definitionPath"`
			Libraries      []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"libraries"`
			PodTemplates []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"podTemplates"`
		} `json:"source"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &detail); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if detail.Source == nil {
		t.Fatal("the get_run result carries no source object")
	}
	if detail.Source.DefinitionPath != "ci/Jenkinsfile" {
		t.Errorf("definitionPath = %q, want %q", detail.Source.DefinitionPath, "ci/Jenkinsfile")
	}
	if len(detail.Source.Libraries) != 1 || detail.Source.Libraries[0].Name != "acme" || detail.Source.Libraries[0].Version != "1.2.3" {
		t.Errorf("libraries = %+v, want one acme entry at 1.2.3", detail.Source.Libraries)
	}
	if len(detail.Source.PodTemplates) != 1 || detail.Source.PodTemplates[0].Image != "maven:3.9.6" {
		t.Errorf("podTemplates = %+v, want one maven entry", detail.Source.PodTemplates)
	}
}

// --- The get_run tool returns the run's stored test report and its test
// cases, because it returns the same run detail shape as GET /runs/{run}. ---

func TestGetRunCarriesTestReportAndCases(t *testing.T) {
	reader := newFakePipelineReader()
	reportKey := "artifacts/team-a/build/7/artifacts/.varroa/tests.json"
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED",
		Payload:       json.RawMessage(`{"test_report":{"size_bytes":4096,"case_count":12}}`),
		TestReportKey: &reportKey,
	}
	reader.testCases["run-a"] = []recordstore.TestCase{
		{Suite: "unit", ClassName: "com.example.FooTest", Name: "bar", Status: "FAILED", ErrorDetails: "boom"},
	}
	deps := &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var detail struct {
		TestReport *struct {
			BlobKey   string `json:"blobKey"`
			SizeBytes int64  `json:"sizeBytes"`
			CaseCount int32  `json:"caseCount"`
		} `json:"testReport"`
		Cases []struct {
			ClassName    string `json:"className"`
			Name         string `json:"name"`
			Status       string `json:"status"`
			ErrorDetails string `json:"errorDetails"`
			CaseKey      string `json:"caseKey"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &detail); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if detail.TestReport == nil || detail.TestReport.BlobKey != reportKey ||
		detail.TestReport.SizeBytes != 4096 || detail.TestReport.CaseCount != 12 {
		t.Fatalf("testReport = %+v, want the stored report", detail.TestReport)
	}
	if len(detail.Cases) != 1 || detail.Cases[0].Name != "bar" || detail.Cases[0].Status != "FAILED" ||
		detail.Cases[0].CaseKey != api.TestCaseKey("com.example.FooTest", "bar") {
		t.Fatalf("cases = %+v, want the one stored case with its key", detail.Cases)
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

// --- list_fleet_locks reads the same team lock list GET
// /pipelines/{team}/locks serves. ---

// ListFleetLocks answers the seeded rows for one tenant.
func (f *fakePipelineReader) ListFleetLocks(_ context.Context, _, tenant string) ([]recordstore.FleetLock, error) {
	return f.locksByTenant[tenant], nil
}

// TestListFleetLocksAnswersTheTeamRows proves the tool answers the
// team's lock rows, in the holder-then-waiters order the record store
// answers with, each row carrying the run link the caller's own read
// grant allows.
func TestListFleetLocksAnswersTheTeamRows(t *testing.T) {
	reader := newFakePipelineReader()
	reader.locksByTenant["team-a"] = []recordstore.FleetLock{
		{Name: "team-a/deploy", RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", State: "held"},
		{Name: "team-a/deploy", RunID: "run-b", Tenant: "team-a", JobPath: "team-a/build", State: "waiting"},
	}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
	}

	tr := callPipelineTool(t, deps, "list_fleet_locks", map[string]interface{}{"team": "team-a"})
	if tr.IsError {
		t.Fatalf("list_fleet_locks returned error: %v", tr.Content)
	}

	var payload struct {
		Items []struct {
			Name         string  `json:"name"`
			Tenant       string  `json:"tenant"`
			JobPath      string  `json:"jobPath"`
			State        string  `json:"state"`
			RunID        string  `json:"runId"`
			RunLink      string  `json:"runLink"`
			WaitingSince *string `json:"waitingSince"`
		} `json:"items"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.Count != 2 || len(payload.Items) != 2 {
		t.Fatalf("count = %d, items = %d, want 2", payload.Count, len(payload.Items))
	}
	if payload.Items[0].State != "held" || payload.Items[1].State != "waiting" {
		t.Errorf("items = %+v, want the holder before its waiter", payload.Items)
	}
	if payload.Items[0].WaitingSince != nil || payload.Items[1].WaitingSince == nil {
		t.Errorf("items = %+v, want waitingSince on the waiter only", payload.Items)
	}
	for _, item := range payload.Items {
		if item.Name != "team-a/deploy" || item.Tenant != "team-a" || item.JobPath != "team-a/build" {
			t.Errorf("row = %+v, want the seeded lock's own identity", item)
		}
		// The link is the run's own page, /pipelines/{team}/{jobPath}/{runId},
		// with the job path's own slash escaped, as a run search result carries it.
		if want := "/pipelines/team-a/team-a%2Fbuild/" + item.RunID; item.RunLink != want {
			t.Errorf("row %s runLink = %q, want %s", item.RunID, item.RunLink, want)
		}
	}
}

// TestListFleetLocksDeniesAnUnreadableTeam proves the tool denies a
// caller whose own grant reads no tenant of the team.
func TestListFleetLocksDeniesAnUnreadableTeam(t *testing.T) {
	reader := newFakePipelineReader()
	reader.locksByTenant["team-b"] = []recordstore.FleetLock{
		{Name: "team-b/deploy", RunID: "run-b", Tenant: "team-b", JobPath: "team-b/build", State: "held"},
	}
	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-b", Team: "team-b", Folder: "team-b"}}},
	}

	tr := callPipelineTool(t, deps, "list_fleet_locks", map[string]interface{}{"team": "team-b"})
	if !tr.IsError {
		t.Fatal("expected list_fleet_locks to deny a caller with no read grant on the team")
	}
	if got := toolText(tr); got != "team not found" {
		t.Errorf("error text = %q, want %q", got, "team not found")
	}
}

// --- get_run and GET /runs/{run} answer the same record, so the
// two must agree on summary and digest. ---

// TestGetRunParityWithHTTPRunDetail reads one run through both surfaces
// and asserts the summary text and the digest object are equal, so a
// field the read sets reaches the MCP result and the HTTP response
// alike. The run is a failed one, so the digest it compares is a full
// one: the stage, its error, the attributed log tail, and the counts.
func TestGetRunParityWithHTTPRunDetail(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	summary := "shipping a hotfix"
	result := "FAILURE"
	stageError := "deploy refused"
	failed, skipped, total := int32(2), int32(1), int32(11)

	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED",
		Result: &result, StartedAt: &started, Summary: &summary,
		TestsFailed: &failed, TestsSkipped: &skipped, TestsTotal: &total,
	}
	reader.stages["run-a"] = []recordstore.Stage{
		{RunID: "run-a", StageID: "s1", Name: "Build", Status: "SUCCESS"},
		{RunID: "run-a", StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: &stageError},
	}

	ctx := context.Background()
	store, err := blob.Open(ctx, "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: time.Now()}
	if err := store.Put(ctx, blob.LogKey("run-a"), strings.NewReader("prepare\ndeploy refused\n"), opts); err != nil {
		t.Fatalf("put console: %v", err)
	}
	index := `{"seq":0,"stageId":"s1","offset":0,"len":8}` + "\n" +
		`{"seq":1,"stageId":"s2","offset":8,"len":15,"final":true}` + "\n"
	if err := store.Put(ctx, blob.LogIndexKey("run-a"), strings.NewReader(index), opts); err != nil {
		t.Fatalf("put index: %v", err)
	}

	deps := &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer("read"),
		RecordStore:    reader,
		BlobStore:      store,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", Team: "team-a", Folder: "team-a"}}},
	}

	// The HTTP surface: the router wires GET /runs/{run} to the same read.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-a", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), pipelinesTestClaims))
	rec := httptest.NewRecorder()
	api.NewRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var httpDetail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &httpDetail); err != nil {
		t.Fatalf("decode the HTTP body: %v", err)
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var mcpDetail map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &mcpDetail); err != nil {
		t.Fatalf("decode the get_run result: %v", err)
	}

	for _, key := range []string{"summary", "digest"} {
		want, ok := httpDetail[key]
		if !ok {
			t.Fatalf("the HTTP response carries no %s: %s", key, rec.Body.String())
		}
		got, ok := mcpDetail[key]
		if !ok {
			t.Fatalf("the get_run result carries no %s: %s", key, tr.StructuredContent)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v from get_run, want %#v from the HTTP response", key, got, want)
		}
	}

	digest, ok := httpDetail["digest"].(map[string]any)
	if !ok {
		t.Fatalf("digest = %#v, want an object", httpDetail["digest"])
	}
	for _, key := range []string{"stage", "error", "logTail", "failedTests"} {
		if _, ok := digest[key]; !ok {
			t.Errorf("the digest carries no %s: %#v", key, digest)
		}
	}
}

// --- The two run input tools publish the same command the matching
// HTTP route publishes. ---

// runInputDeps wires the deps the two input tools and the two input
// routes share, over one RUNNING run of tenant "team-a" whose own
// namespace and cell columns name the member the command reaches.
func runInputDeps(reader *fakePipelineReader, mt transport.Transport, verb string) *api.Dependencies {
	buildNumber := int64(7)
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Cluster: "core", Namespace: "cell-ns", Cell: "cell-a",
		Tenant: "team-a", JobPath: "team-a/build", Status: "RUNNING", BuildNumber: &buildNumber,
	}
	return &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer(verb),
		Brood:          coreBrood{},
		RecordStore:    reader,
		MiteRegistry:   mt,
		TenantResolver: &fakePipelineTenantResolver{},
	}
}

// coreBrood names "core" as the local cluster, the cluster of the run
// runInputDeps seeds, so its commands take the local send.
type coreBrood struct{ api.Brood }

func (coreBrood) LocalCluster() string { return "core" }

// postRunInputRoute posts to one of the two input routes through the real
// router, the way a dashboard caller reaches them.
func postRunInputRoute(t *testing.T, deps *api.Dependencies, runID, inputID, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/inputs/"+inputID+"/"+action, reader)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), pipelinesTestClaims))
	rec := httptest.NewRecorder()
	api.NewRouter(deps).ServeHTTP(rec, req)
	return rec
}

func TestRunInputToolsPublishTheHTTPCommand(t *testing.T) {
	cases := []struct {
		tool   string
		action string
		args   map[string]interface{}
		body   string
	}{
		{
			tool: "proceed_run_input", action: "proceed",
			args: map[string]interface{}{"run": "run-a", "input": "in-1", "parameters": map[string]interface{}{"env": "prod"}},
			body: `{"parameters":{"env":"prod"}}`,
		},
		{
			tool: "abort_run_input", action: "abort",
			args: map[string]interface{}{"run": "run-a", "input": "in-1"},
			body: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			sent := &fakePipelineTransport{}
			deps := runInputDeps(newFakePipelineReader(), sent, "write")

			tr := callPipelineTool(t, deps, tc.tool, tc.args)
			if tr.IsError {
				t.Fatalf("%s returned error: %v", tc.tool, tr.Content)
			}
			var payload map[string]string
			if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
				t.Fatalf("unmarshal structuredContent: %v", err)
			}
			if payload["state"] != "requested" {
				t.Errorf("state = %q, want requested", payload["state"])
			}
			if len(sent.sent) != 1 {
				t.Fatalf("the tool published %d commands, want 1", len(sent.sent))
			}
			toolCmd := sent.sent[0]

			sent.sent = nil
			rec := postRunInputRoute(t, deps, "run-a", "in-1", tc.action, tc.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("HTTP status = %d, body = %s, want 202", rec.Code, rec.Body.String())
			}
			if len(sent.sent) != 1 {
				t.Fatalf("the HTTP route published %d commands, want 1", len(sent.sent))
			}
			if !reflect.DeepEqual(toolCmd, sent.sent[0]) {
				t.Errorf("%s published %+v, want the HTTP route's own command %+v", tc.tool, toolCmd, sent.sent[0])
			}
		})
	}
}

func TestRunInputToolsDenyReadOnlyCaller(t *testing.T) {
	for _, tool := range []string{"proceed_run_input", "abort_run_input"} {
		t.Run(tool, func(t *testing.T) {
			sent := &fakePipelineTransport{}
			deps := runInputDeps(newFakePipelineReader(), sent, "read")

			tr := callPipelineTool(t, deps, tool, map[string]interface{}{"run": "run-a", "input": "in-1"})
			if !tr.IsError {
				t.Fatalf("expected %s to deny a read-only caller", tool)
			}
			if got := toolText(tr); got != "forbidden" {
				t.Errorf("error text = %q, want %q", got, "forbidden")
			}
			if len(sent.sent) != 0 {
				t.Errorf("a denied call published %d commands, want 0", len(sent.sent))
			}
		})
	}
}

func TestRunInputToolsAnswerNotFoundForAnUnknownRun(t *testing.T) {
	for _, tool := range []string{"proceed_run_input", "abort_run_input"} {
		t.Run(tool, func(t *testing.T) {
			sent := &fakePipelineTransport{}
			deps := runInputDeps(newFakePipelineReader(), sent, "write")

			tr := callPipelineTool(t, deps, tool, map[string]interface{}{"run": "absent", "input": "in-1"})
			if !tr.IsError {
				t.Fatalf("expected %s to refuse an unknown run", tool)
			}
			if got := toolText(tr); got != "run not found" {
				t.Errorf("error text = %q, want %q", got, "run not found")
			}
			if len(sent.sent) != 0 {
				t.Errorf("an unknown run published %d commands, want 0", len(sent.sent))
			}
		})
	}
}

func TestRunInputToolsDenyANilAuthorizer(t *testing.T) {
	for _, tool := range []string{"proceed_run_input", "abort_run_input"} {
		t.Run(tool, func(t *testing.T) {
			deps := &api.Dependencies{RecordStore: newFakePipelineReader()}
			tr := callPipelineTool(t, deps, tool, map[string]interface{}{"run": "run-a", "input": "in-1"})
			if !tr.IsError {
				t.Fatalf("expected %s to deny a nil authorizer", tool)
			}
			if got := toolText(tr); !strings.Contains(got, "authorizer") {
				t.Errorf("error text = %q, want it to name the authorizer", got)
			}
		})
	}
}

func TestRunInputToolsNameAnUnconfiguredRecordStore(t *testing.T) {
	for _, tool := range []string{"proceed_run_input", "abort_run_input"} {
		t.Run(tool, func(t *testing.T) {
			deps := &api.Dependencies{Authorizer: pipelinesTestAuthorizer("write")}
			tr := callPipelineTool(t, deps, tool, map[string]interface{}{"run": "run-a", "input": "in-1"})
			if !tr.IsError {
				t.Fatalf("expected %s to refuse an unconfigured record store", tool)
			}
			if got := toolText(tr); !strings.Contains(got, "record store") {
				t.Errorf("error text = %q, want it to name the record store", got)
			}
		})
	}
}

// TestGetRunParityWithHTTPInputs reads one run whose column holds two
// pending inputs through both surfaces and asserts the two arrays agree,
// so a field the read sets reaches the MCP result and the HTTP response
// alike.
func TestGetRunParityWithHTTPInputs(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{
		RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "RUNNING",
		Inputs: json.RawMessage(`[` +
			`{"id":"in-1","message":"Deploy?","ok":"Ship it","submitter":"deployers","submitter_parameter":"APPROVER",` +
			`"parameters":[{"name":"ENV","type":"ChoiceParameterDefinition","default_value":"staging","choices":["staging","prod"]}]},` +
			`{"id":"in-2","message":"Resume?"}]`),
	}
	deps := &api.Dependencies{
		Authorizer:  pipelinesTestAuthorizer("read"),
		RecordStore: reader,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-a", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), pipelinesTestClaims))
	rec := httptest.NewRecorder()
	api.NewRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var httpDetail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &httpDetail); err != nil {
		t.Fatalf("decode the HTTP body: %v", err)
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var mcpDetail map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &mcpDetail); err != nil {
		t.Fatalf("decode the get_run result: %v", err)
	}

	want, ok := httpDetail["inputs"]
	if !ok {
		t.Fatalf("the HTTP response carries no inputs: %s", rec.Body.String())
	}
	entries, ok := want.([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("HTTP inputs = %#v, want two entries", want)
	}
	got, ok := mcpDetail["inputs"]
	if !ok {
		t.Fatalf("the get_run result carries no inputs: %s", tr.StructuredContent)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inputs = %#v from get_run, want %#v from the HTTP response", got, want)
	}
}
