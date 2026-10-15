package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// restartRecord builds the run.completed payload the plugin writes: the
// envelope sits under the "run.completed" key, and the restart answer sits
// under the envelope's own payload key.
func restartRecord(t *testing.T, stages []string, refusal string) json.RawMessage {
	t.Helper()
	inner := map[string]any{}
	if stages != nil {
		inner["restart_stages"] = stages
	}
	if refusal != "" {
		inner["restart_refusal"] = refusal
	}
	raw, err := json.Marshal(map[string]any{
		"run.completed": map[string]any{"payload": inner},
	})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return raw
}

// restartFixture seeds one completed restartable run of tenant "test-ns/ci"
// at job path "svc/main", recorded on cell "ci" of cluster "core". It answers
// a server wired to the reader and the transport.
func restartFixture(t *testing.T, verbs []string, stages []string) (*Server, *fakeRecordReader, *fakeTriggerTransport) {
	t.Helper()
	reader := newFakeRecordReader()
	run := replicatedRun(t, "test-ns/ci", 300)
	run.Cluster, run.Cell = "core", "ci"
	run.Params = json.RawMessage(`{"branch":"main"}`)
	buildNumber := int64(7)
	run.BuildNumber = &buildNumber
	run.Payload = restartRecord(t, stages, "")
	reader.runs[scriptRunID] = run
	mt := &fakeTriggerTransport{}
	srv := writeTestServerWithVerbs(reader, mt, verbs)
	return srv, reader, mt
}

// TestRunRestartSource_Reasons asserts each refusal reason and a restartable
// run, in the order the rules run.
func TestRunRestartSource_Reasons(t *testing.T) {
	stages := []string{"build", "deploy"}
	tests := []struct {
		name       string
		mutate     func(run *recordstore.Run, reader *fakeRecordReader)
		wantReason string
	}{
		{name: "restartable", wantReason: ""},
		{name: "not-terminal", mutate: func(run *recordstore.Run, _ *fakeRecordReader) {
			run.Status = recordstore.StatusRunning
		}, wantReason: restartReasonNotTerminal},
		{name: "template-defined", mutate: func(run *recordstore.Run, _ *fakeRecordReader) {
			run.Payload = json.RawMessage(`{"run.started":{"payload":{"template_name":"deploy"}}}`)
		}, wantReason: restartReasonTemplateDefined},
		{name: "not-recorded", mutate: func(run *recordstore.Run, _ *fakeRecordReader) {
			run.Payload = json.RawMessage(`{}`)
		}, wantReason: restartReasonNotRecorded},
		{name: "not-declarative", mutate: func(run *recordstore.Run, _ *fakeRecordReader) {
			run.Payload = restartRecord(t, nil, restartReasonNotDeclarative)
		}, wantReason: restartReasonNotDeclarative},
		{name: "no-restartable-stage", mutate: func(run *recordstore.Run, _ *fakeRecordReader) {
			run.Payload = restartRecord(t, []string{}, "")
		}, wantReason: restartReasonNoStage},
		{name: "home-moved", mutate: func(_ *recordstore.Run, reader *fakeRecordReader) {
			reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}
		}, wantReason: restartReasonHomeMoved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, reader, _ := restartFixture(t, []string{"read", "write"}, stages)
			run := reader.runs[scriptRunID]
			if tc.mutate != nil {
				tc.mutate(&run, reader)
				reader.runs[scriptRunID] = run
			}
			got, serr := srv.runRestartSource(context.Background(), run)
			if serr != nil {
				t.Fatalf("runRestartSource error: %v", serr)
			}
			if got.Reason != tc.wantReason || got.Restartable != (tc.wantReason == "") {
				t.Fatalf("answer = %+v, want reason %q", got, tc.wantReason)
			}
			if got.Stages == nil {
				t.Fatalf("stages is nil; want a non-nil list")
			}
		})
	}
}

// TestRestartRunPublishesRestartBuild asserts a permitted restart answers 202
// with a fresh UUIDv7 and the restart key, and publishes one RESTART_BUILD
// command that names the source build number and the stage.
func TestRestartRunPublishesRestartBuild(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read", "write"}, []string{"build", "deploy"})
	claims := &auth.Claims{Subject: "u", PreferredUsername: "ana", Email: "ana@example.invalid", Groups: []string{"payments"}}

	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, claims, restartRunRequest{Stage: "deploy"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if id, err := uuid.Parse(resp.RunID); err != nil || id.Version() != 7 {
		t.Fatalf("runId %q is not a UUIDv7 (err %v)", resp.RunID, err)
	}
	if resp.IntentKey != "restart:"+scriptRunID+":"+resp.RunID || resp.State != "requested" {
		t.Fatalf("response = %+v, want the restart key and the requested state", resp)
	}
	if _, err := recordstore.ParseIntentKey(resp.IntentKey); err != nil {
		t.Fatalf("intent key %q does not parse: %v", resp.IntentKey, err)
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeRestartBuild || cmd.RestartBuild == nil {
		t.Fatalf("command = %+v, want one RESTART_BUILD", cmd)
	}
	p := cmd.RestartBuild
	if p.RunId != resp.RunID || p.IntentKey != resp.IntentKey || p.SourceRunId != scriptRunID || p.JobPath != "svc/main" {
		t.Fatalf("payload = %+v, want the new identity, the source run, and the job path", p)
	}
	if p.SourceBuildNumber != 7 || p.Stage != "deploy" {
		t.Fatalf("payload = %+v, want source build 7 and stage deploy", p)
	}
	if p.ActingUser == nil || p.ActingUser.Name != "ana" {
		t.Fatalf("acting user = %+v, want ana", p.ActingUser)
	}
}

// TestRestartRunRefusesUnlistedStage asserts a stage the record does not list
// answers 409 and publishes nothing.
func TestRestartRunRefusesUnlistedStage(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read", "write"}, []string{"build", "deploy"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: "publish"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stage publish is not restartable") {
		t.Fatalf("status = %d, body = %s, want 409 naming the stage", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestRestartRunRequiresStage asserts a blank stage answers 400 and publishes
// nothing.
func TestRestartRunRequiresStage(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read", "write"}, []string{"build"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: ""})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestRestartRunForbidden asserts a caller with the read grant alone answers
// 403 and publishes nothing.
func TestRestartRunForbidden(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read"}, []string{"build"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: "build"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestRestartRunHomeMovedIsAConflict asserts a job whose home moved off the
// source cell answers 409 and publishes nothing.
func TestRestartRunHomeMovedIsAConflict(t *testing.T) {
	srv, reader, mt := restartFixture(t, []string{"read", "write"}, []string{"build"})
	reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: "build"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), restartReasonHomeMoved) {
		t.Fatalf("status = %d, body = %s, want 409 home-moved", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestHandleGetRunRestart asserts GET /runs/{run}/restart answers the restart
// object of a restartable run.
func TestHandleGetRunRestart(t *testing.T) {
	srv, _, _ := restartFixture(t, []string{"read"}, []string{"build", "deploy"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), nil)
	req.Method = http.MethodGet
	rec := httptest.NewRecorder()
	srv.handleGetRunRestart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var got RunRestart
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Restartable || len(got.Stages) != 2 || got.Stages[1] != "deploy" {
		t.Fatalf("answer = %+v, want restartable with two stages", got)
	}
}

// TestRestartRunKeepsAPaddedStageNameExact asserts a stage whose recorded name
// carries spaces reaches the plugin byte for byte, and an unpadded twin is not
// selected in its place.
func TestRestartRunKeepsAPaddedStageNameExact(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read", "write"}, []string{" deploy ", "deploy"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: " deploy "})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 || calls[0].cmd.RestartBuild == nil {
		t.Fatalf("published %+v, want one RESTART_BUILD", calls)
	}
	if got := calls[0].cmd.RestartBuild.Stage; got != " deploy " {
		t.Fatalf("stage = %q, want the padded name unchanged", got)
	}
}

// withRestartCopy adds a restart-copy manifest to a run's payload. The copy
// holds one file of 10 bytes, and complete sets whether that file arrived.
func withRestartCopy(t *testing.T, run *recordstore.Run, complete bool) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(run.Payload, &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	files := []map[string]any{{"relpath": "jobs/svc/jobs/main/builds/7/build.xml", "bytes": 10}}
	if !complete {
		files = []map[string]any{}
	}
	copyRec := map[string]any{
		"epoch": 1, "expected_files": 1, "expected_bytes": 10, "complete": complete, "files": files,
	}
	raw, err := json.Marshal(copyRec)
	if err != nil {
		t.Fatalf("marshal copy: %v", err)
	}
	doc["restart_source"] = raw
	run.Payload, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
}

// TestRunRestartSource_MovedJobWithCopyIsRestartable asserts a moved job
// stays restartable while the run keeps a complete copy, and answers
// home-moved when the copy is partial.
func TestRunRestartSource_MovedJobWithCopyIsRestartable(t *testing.T) {
	tests := []struct {
		name       string
		complete   bool
		wantReason string
	}{
		{name: "complete copy", complete: true, wantReason: ""},
		{name: "partial copy", complete: false, wantReason: restartReasonHomeMoved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, reader, _ := restartFixture(t, []string{"read", "write"}, []string{"build", "deploy"})
			reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}
			run := reader.runs[scriptRunID]
			withRestartCopy(t, &run, tc.complete)
			got, serr := srv.runRestartSource(context.Background(), run)
			if serr != nil {
				t.Fatalf("runRestartSource error: %v", serr)
			}
			if got.Reason != tc.wantReason || got.Restartable != (tc.wantReason == "") {
				t.Fatalf("answer = %+v, want reason %q", got, tc.wantReason)
			}
		})
	}
}

// TestRestartRunSendsCopyFilesAfterPodRoll asserts the command carries the
// copy's file list whenever a complete copy exists, even on the same cell.
func TestRestartRunSendsCopyFilesAfterPodRoll(t *testing.T) {
	srv, reader, mt := restartFixture(t, []string{"read", "write"}, []string{"build", "deploy"})
	run := reader.runs[scriptRunID]
	withRestartCopy(t, &run, true)
	reader.runs[scriptRunID] = run

	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: "deploy"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	files := calls[0].cmd.RestartBuild.SourceFiles
	if len(files) != 1 || files[0].Relpath != "jobs/svc/jobs/main/builds/7/build.xml" || files[0].Bytes != 10 {
		t.Fatalf("source files = %+v, want the one copied build.xml", files)
	}
}

// TestRestartRunOmitsCopyFilesWithoutCopy asserts a run with no copy sends no
// file list.
func TestRestartRunOmitsCopyFilesWithoutCopy(t *testing.T) {
	srv, _, mt := restartFixture(t, []string{"read", "write"}, []string{"build", "deploy"})
	req := doWriteRequest("/runs/"+scriptRunID+"/restart", map[string]string{"run": scriptRunID}, testClaims(), restartRunRequest{Stage: "deploy"})
	rec := httptest.NewRecorder()
	srv.handleRestartRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	if files := mt.calls()[0].cmd.RestartBuild.SourceFiles; len(files) != 0 {
		t.Fatalf("source files = %+v, want none", files)
	}
}
