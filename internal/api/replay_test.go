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

// replayFixture seeds one replayable run of the write harness's tenant
// "test-ns/ci" at job path "svc/main", with stored parameters and a
// sandboxed script replica, and answers a server wired to the reader, the
// transport, and the blob store.
func replayFixture(t *testing.T, verbs []string) (*Server, *fakeRecordReader, *fakeTriggerTransport) {
	t.Helper()
	reader := newFakeRecordReader()
	run := replicatedRun(t, "test-ns/ci", 300)
	run.Params = json.RawMessage(`{"branch":"main"}`)
	reader.runs[scriptRunID] = run
	blobs := newFakeBlobStore()
	storeScriptObject(t, blobs, run, buildXMLDoc("echo 'source'", "true"))
	mt := &fakeTriggerTransport{}
	srv := writeTestServerWithVerbs(reader, mt, verbs)
	srv.deps.BlobStore = blobs
	return srv, reader, mt
}

// postReplay posts a replay of the fixture run with body.
func postReplay(t *testing.T, srv *Server, claims *auth.Claims, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := doWriteRequest("/runs/"+scriptRunID+"/replay", map[string]string{"run": scriptRunID}, claims, body)
	rec := httptest.NewRecorder()
	srv.handleReplayRun(rec, req)
	return rec
}

// TestHandleReplayRun_PublishesOneReplayBuild asserts a permitted replay
// answers 202 with a fresh UUIDv7 run identifier and the replay key, and
// publishes one REPLAY_BUILD command carrying the edited script, the
// source run's stored parameters, and the acting user.
func TestHandleReplayRun_PublishesOneReplayBuild(t *testing.T) {
	srv, _, mt := replayFixture(t, []string{"read", "write"})
	claims := &auth.Claims{Subject: "u", PreferredUsername: "ana", Email: "ana@example.invalid", Groups: []string{"payments"}}

	rec := postReplay(t, srv, claims, replayRunRequest{Script: "echo 'edited'"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	var resp runActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, err := uuid.Parse(resp.RunID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("runId %q is not a UUIDv7 (err %v)", resp.RunID, err)
	}
	if resp.IntentKey != "replay:"+scriptRunID+":"+resp.RunID || resp.State != "requested" {
		t.Fatalf("response = %+v, want the replay key and the requested state", resp)
	}
	if _, err := recordstore.ParseIntentKey(resp.IntentKey); err != nil {
		t.Fatalf("intent key %q does not parse: %v", resp.IntentKey, err)
	}

	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	cmd := calls[0].cmd
	if cmd.Type != mitev1.CommandTypeReplayBuild || cmd.ReplayBuild == nil {
		t.Fatalf("command = %+v, want one REPLAY_BUILD", cmd)
	}
	p := cmd.ReplayBuild
	if p.RunId != resp.RunID || p.IntentKey != resp.IntentKey || p.SourceRunId != scriptRunID || p.JobPath != "svc/main" {
		t.Fatalf("payload = %+v, want the new identity, the source run, and the job path", p)
	}
	if p.Script != "echo 'edited'" {
		t.Fatalf("script = %q, want the edited script", p.Script)
	}
	if p.Parameters["branch"] != "main" {
		t.Fatalf("parameters = %+v, want the source run's stored branch=main", p.Parameters)
	}
	if p.ActingUser == nil || p.ActingUser.Name != "ana" || p.ActingUser.Email != "ana@example.invalid" ||
		len(p.ActingUser.Groups) != 1 || p.ActingUser.Groups[0] != "payments" {
		t.Fatalf("acting user = %+v, want ana with her email and groups", p.ActingUser)
	}
}

// TestHandleReplayRun_ReadOnlyCallerIsForbidden asserts a caller with the
// tenant read grant alone answers 403 and publishes nothing.
func TestHandleReplayRun_ReadOnlyCallerIsForbidden(t *testing.T) {
	srv, _, mt := replayFixture(t, []string{"read"})
	rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestHandleReplayRun_UnknownRunIsNotFound asserts an unknown run answers
// 404 and publishes nothing.
func TestHandleReplayRun_UnknownRunIsNotFound(t *testing.T) {
	srv, reader, mt := replayFixture(t, []string{"read", "write"})
	delete(reader.runs, scriptRunID)
	rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want 0", len(mt.calls()))
	}
}

// TestHandleReplayRun_ScriptBounds asserts an empty script answers 400 and
// a script over 256 KiB answers 413, and neither publishes.
func TestHandleReplayRun_ScriptBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   int
	}{
		{"empty", "", http.StatusBadRequest},
		{"blank", "  \n\t", http.StatusBadRequest},
		{"oversize", strings.Repeat("a", runScriptMaxScriptBytes+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, mt := replayFixture(t, []string{"read", "write"})
			rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: tc.script})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if len(mt.calls()) != 0 {
				t.Fatalf("published %d commands, want 0", len(mt.calls()))
			}
		})
	}
}

// TestHandleReplayRun_RefusedRunIsAConflict asserts each run the script
// read refuses answers 409 naming the reason, and publishes nothing.
func TestHandleReplayRun_RefusedRunIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*recordstore.Run)
		reason string
	}{
		{"not terminal", func(r *recordstore.Run) { r.Status = recordstore.StatusRunning }, replayReasonNotTerminal},
		{"template defined", func(r *recordstore.Run) {
			r.Payload = mergePayload(t, r.Payload, startedEnvelope(map[string]any{"template_name": "go-service"}))
		}, replayReasonTemplateDefined},
		{"masked parameter", func(r *recordstore.Run) { r.Params = json.RawMessage(`{"TOKEN":"<masked>"}`) }, replayReasonMaskedParameter},
		{"no replica", func(r *recordstore.Run) { r.Payload = nil }, replayReasonNoReplica},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reader, mt := replayFixture(t, []string{"read", "write"})
			run := reader.runs[scriptRunID]
			tc.mutate(&run)
			reader.runs[scriptRunID] = run

			rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "run does not replay: "+tc.reason) {
				t.Fatalf("body = %s, want the reason %q", rec.Body.String(), tc.reason)
			}
			if len(mt.calls()) != 0 {
				t.Fatalf("published %d commands, want 0", len(mt.calls()))
			}
		})
	}

	t.Run("not sandboxed", func(t *testing.T) {
		srv, reader, mt := replayFixture(t, []string{"read", "write"})
		blobs := newFakeBlobStore()
		storeScriptObject(t, blobs, reader.runs[scriptRunID], buildXMLDoc("echo 'source'", "false"))
		srv.deps.BlobStore = blobs
		rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), replayReasonNotSandbox) {
			t.Fatalf("status = %d, body = %s, want 409 naming not-sandbox", rec.Code, rec.Body.String())
		}
		if len(mt.calls()) != 0 {
			t.Fatalf("published %d commands, want 0", len(mt.calls()))
		}
	})
}

// TestHandleReplayRun_PublishesToTheJobsHomeMember asserts the replay goes
// to the member that holds the job's home slot, not to the source run's
// own member.
func TestHandleReplayRun_PublishesToTheJobsHomeMember(t *testing.T) {
	srv, reader, mt := replayFixture(t, []string{"read", "write"})
	run := reader.runs[scriptRunID]
	run.Cluster, run.Namespace, run.Cell = "core", "test-ns", "ci"
	reader.runs[scriptRunID] = run
	reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}

	rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	if calls[0].ns != "cells-ns" || calls[0].name != "cell-b" {
		t.Fatalf("published to %s/%s, want the home member cells-ns/cell-b", calls[0].ns, calls[0].name)
	}
}

// TestHandleReplayRun_TwoReplaysAreTwoBuilds asserts two replays of one
// run answer two run identifiers and two intent keys, and publish two
// commands.
func TestHandleReplayRun_TwoReplaysAreTwoBuilds(t *testing.T) {
	srv, _, mt := replayFixture(t, []string{"read", "write"})
	var responses []runActionResponse
	for i := 0; i < 2; i++ {
		rec := postReplay(t, srv, testClaims(), replayRunRequest{Script: "echo 'edited'"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
		}
		var resp runActionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		responses = append(responses, resp)
	}
	if responses[0].RunID == responses[1].RunID || responses[0].IntentKey == responses[1].IntentKey {
		t.Fatalf("responses = %+v, want two run identifiers and two intent keys", responses)
	}
	if len(mt.calls()) != 2 {
		t.Fatalf("published %d commands, want 2", len(mt.calls()))
	}
}

// TestReplayRun_ServiceMatchesTheHTTPPath asserts the exported service
// method answers the same identity shape and publishes the same command.
func TestReplayRun_ServiceMatchesTheHTTPPath(t *testing.T) {
	srv, _, mt := replayFixture(t, []string{"read", "write"})
	run := srv.deps.RecordStore.(*fakeRecordReader).runs[scriptRunID]
	run.Cluster, run.Namespace, run.Cell = "core", "test-ns", "ci"
	srv.deps.RecordStore.(*fakeRecordReader).runs[scriptRunID] = run
	runID, intentKey, targetCluster, serr := srv.ReplayRun(context.Background(), testClaims(), scriptRunID, "echo 'edited'")
	if serr != nil {
		t.Fatalf("ReplayRun: %v", serr)
	}
	if targetCluster != "core" {
		t.Fatalf("targetCluster = %q, want the home member's cluster core", targetCluster)
	}
	if intentKey != "replay:"+scriptRunID+":"+runID {
		t.Fatalf("intentKey = %q, want the replay key of %q", intentKey, runID)
	}
	if len(mt.calls()) != 1 || mt.calls()[0].cmd.Type != mitev1.CommandTypeReplayBuild {
		t.Fatalf("calls = %+v, want one REPLAY_BUILD", mt.calls())
	}
}

// TestHandleGetRun_CarriesReplayOf asserts the run detail answers replayOf
// for a replayed build and omits it for an ordinary one, on the HTTP path
// and on the service path the MCP get_run tool calls.
func TestHandleGetRun_CarriesReplayOf(t *testing.T) {
	source := uuid.MustParse("0199aa11-2222-7333-8444-555566667777")
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", ReplayOf: &source}
	reader.runs["r2"] = recordstore.Run{RunID: "r2", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail["replayOf"] != source.String() {
		t.Fatalf("replayOf = %v, want %s", detail["replayOf"], source)
	}

	rec = doRunRequest(t, srv.handleGetRun, "/runs/r2", map[string]string{"run": "r2"}, nil)
	if strings.Contains(rec.Body.String(), "replayOf") {
		t.Fatalf("body = %s, want no replayOf for an ordinary build", rec.Body.String())
	}

	out, serr := srv.GetRun(context.Background(), testClaims(), "r1")
	if serr != nil {
		t.Fatalf("GetRun: %v", serr)
	}
	if out.ReplayOf != source.String() {
		t.Fatalf("GetRun replayOf = %q, want %s", out.ReplayOf, source)
	}
}
