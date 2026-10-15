package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/api"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// fakeScriptBlobStore serves stored objects by key through the range
// read, the one read the script source opens: the gateway stores a
// run-state object through Replace, which the bucket store keeps as
// parts.
type fakeScriptBlobStore struct {
	blob.Store
	objects map[string][]byte
}

func (f *fakeScriptBlobStore) NewRangeReader(_ context.Context, key string, _, _ int64) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, blob.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// replayToolDeps seeds one replayable run, "run-a" of tenant "team-a",
// with a sandboxed script replica, and wires the tool's dependencies with
// verb on the tenant.
func replayToolDeps(t *testing.T, reader *fakePipelineReader, mt *fakePipelineTransport, verb string) *api.Dependencies {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"mesh": map[string]any{"files": []map[string]any{
		{"relpath": "team-a/build/builds/3/build.xml", "bytes": 200, "seq": 1, "state_gen": 2, "epoch": 9},
	}}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	run := recordstore.Run{
		RunID: "run-a", Cluster: "core", Namespace: "cell-ns", Cell: "cell-a",
		Tenant: "team-a", JobPath: "team-a/build", Status: recordstore.StatusCompleted, Payload: payload,
	}
	reader.runs["run-a"] = run
	key, ok := recordstore.RunScriptObjectKey(run)
	if !ok {
		t.Fatal("seeded run names no build record")
	}
	doc := "<?xml version='1.1' encoding='UTF-8'?><flow-build><execution>" +
		"<script>echo 'source'</script><loadedScripts/><sandbox>true</sandbox></execution></flow-build>"
	return &api.Dependencies{
		Authorizer:     pipelinesTestAuthorizer(verb),
		Brood:          coreBrood{},
		RecordStore:    reader,
		BlobStore:      &fakeScriptBlobStore{objects: map[string][]byte{key: []byte(doc)}},
		MiteRegistry:   mt,
		TenantResolver: &fakePipelineTenantResolver{},
	}
}

func TestReplayRunToolPublishesOneReplayBuild(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := replayToolDeps(t, newFakePipelineReader(), sent, "write")

	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if tr.IsError {
		t.Fatalf("replay_run returned error: %v", tr.Content)
	}
	var payload struct {
		RunID     string `json:"runId"`
		IntentKey string `json:"intentKey"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.RunID == "" || payload.IntentKey != "replay:run-a:"+payload.RunID || payload.State != "requested" {
		t.Fatalf("result = %+v, want a fresh run, its replay key, and the requested state", payload)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("published %d commands, want 1", len(sent.sent))
	}
	cmd := sent.sent[0]
	if cmd.Type != mitev1.CommandTypeReplayBuild || cmd.ReplayBuild == nil ||
		cmd.ReplayBuild.RunId != payload.RunID || cmd.ReplayBuild.Script != "echo 'edited'" {
		t.Fatalf("command = %+v, want one REPLAY_BUILD carrying the edited script", cmd)
	}
}

// TestReplayRunToolRecordsActivityOnTheTargetCluster asserts the tool
// records one activity event on the cluster the command reached, the
// source run's own cluster here, and not on the BFF's local cluster.
func TestReplayRunToolRecordsActivityOnTheTargetCluster(t *testing.T) {
	sink := &recordingSink{}
	deps := replayToolDeps(t, newFakePipelineReader(), &fakePipelineTransport{}, "write")
	deps.ActivityPublisher = sink

	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if tr.IsError {
		t.Fatalf("replay_run returned error: %v", tr.Content)
	}
	if len(sink.events) != 1 {
		t.Fatalf("published %d events, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Cluster != "core" {
		t.Errorf("event Cluster = %q, want the target cluster core", e.Cluster)
	}
	if e.Type != "pipeline.run_replay_requested" {
		t.Errorf("event Type = %q, want pipeline.run_replay_requested", e.Type)
	}
}

func TestReplayRunToolDeniesReadOnlyCaller(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := replayToolDeps(t, newFakePipelineReader(), sent, "read")

	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if !tr.IsError {
		t.Fatal("expected replay_run to deny a read-only caller")
	}
	if got := toolText(tr); got != "forbidden" {
		t.Errorf("error text = %q, want %q", got, "forbidden")
	}
	if len(sent.sent) != 0 {
		t.Errorf("a denied call published %d commands, want 0", len(sent.sent))
	}
}

func TestReplayRunToolAnswersNotFoundForAnUnknownRun(t *testing.T) {
	sent := &fakePipelineTransport{}
	deps := replayToolDeps(t, newFakePipelineReader(), sent, "write")

	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "absent", "script": "echo 'edited'"})
	if !tr.IsError {
		t.Fatal("expected replay_run to refuse an unknown run")
	}
	if got := toolText(tr); got != "run not found" {
		t.Errorf("error text = %q, want %q", got, "run not found")
	}
	if len(sent.sent) != 0 {
		t.Errorf("an unknown run published %d commands, want 0", len(sent.sent))
	}
}

func TestReplayRunToolAnswersConflictForATemplateRun(t *testing.T) {
	sent := &fakePipelineTransport{}
	reader := newFakePipelineReader()
	deps := replayToolDeps(t, reader, sent, "write")
	run := reader.runs["run-a"]
	run.Payload = json.RawMessage(`{"run.started":{"kind":"run.started","payload":{"template_name":"go-service"}}}`)
	reader.runs["run-a"] = run

	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if !tr.IsError {
		t.Fatal("expected replay_run to refuse a template-defined run")
	}
	if got := toolText(tr); got != "run does not replay: template-defined" {
		t.Errorf("error text = %q, want the template-defined conflict", got)
	}
	if len(sent.sent) != 0 {
		t.Errorf("a refused run published %d commands, want 0", len(sent.sent))
	}
}

func TestReplayRunToolDeniesANilAuthorizer(t *testing.T) {
	deps := &api.Dependencies{RecordStore: newFakePipelineReader()}
	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if !tr.IsError {
		t.Fatal("expected replay_run to deny a nil authorizer")
	}
	if got := toolText(tr); !strings.Contains(got, "authorizer") {
		t.Errorf("error text = %q, want it to name the authorizer", got)
	}
}

func TestReplayRunToolNamesAnUnconfiguredRecordStore(t *testing.T) {
	deps := &api.Dependencies{Authorizer: pipelinesTestAuthorizer("write")}
	tr := callPipelineTool(t, deps, "replay_run", map[string]interface{}{"run": "run-a", "script": "echo 'edited'"})
	if !tr.IsError {
		t.Fatal("expected replay_run to refuse an unconfigured record store")
	}
	if got := toolText(tr); !strings.Contains(got, "record store") {
		t.Errorf("error text = %q, want it to name the record store", got)
	}
}

// TestGetRunToolCarriesReplayOf asserts get_run answers replayOf for a
// replayed build with no tool change.
func TestGetRunToolCarriesReplayOf(t *testing.T) {
	reader := newFakePipelineReader()
	sent := &fakePipelineTransport{}
	deps := replayToolDeps(t, reader, sent, "read")
	source := "0199aa11-2222-7333-8444-555566667777"
	run := reader.runs["run-a"]
	parsed := uuid.MustParse(source)
	run.ReplayOf = &parsed
	reader.runs["run-a"] = run

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	var detail map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &detail); err != nil {
		t.Fatalf("decode the get_run result: %v", err)
	}
	if detail["replayOf"] != source {
		t.Fatalf("replayOf = %v, want %s", detail["replayOf"], source)
	}
}
