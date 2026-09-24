package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// scriptRunID is the run identifier every script-read case seeds.
const scriptRunID = "0199aa11-2222-7333-8444-555566667777"

// scriptBuildXMLPath is the replicated build record path every case's
// mesh file list names.
const scriptBuildXMLPath = "jobs/ci/jobs/svc/jobs/main/builds/7/build.xml"

// buildXMLDoc answers a minimal build.xml whose execution element holds
// script and sandbox. It opens with the XML 1.1 declaration Jenkins
// writes.
func buildXMLDoc(script, sandbox string) string {
	return "<?xml version='1.1' encoding='UTF-8'?>\n<flow-build>\n" +
		"  <execution class=\"org.jenkinsci.plugins.workflow.cps.CpsFlowExecution\">\n" +
		"    <script>" + script + "</script>\n" +
		"    <loadedScripts class=\"linked-hash-map\"/>\n" +
		"    <sandbox>" + sandbox + "</sandbox>\n" +
		"  </execution>\n</flow-build>\n"
}

// replicatedRun answers a completed run of the tenant tenant whose mesh
// file list names one build.xml entry of size bytes, plus a program.dat
// entry the read never opens.
func replicatedRun(t *testing.T, tenant string, bytes int64) recordstore.Run {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"mesh": map[string]any{"files": []map[string]any{
			{"relpath": "jobs/ci/jobs/svc/jobs/main/builds/7/program.dat", "bytes": 900, "seq": 1, "state_gen": 4, "epoch": 10},
			{"relpath": scriptBuildXMLPath, "bytes": bytes, "seq": 1, "state_gen": 3, "epoch": 10},
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return recordstore.Run{
		RunID: scriptRunID, Tenant: tenant, JobPath: "svc/main", Status: recordstore.StatusCompleted,
		Payload: payload,
	}
}

// storeScriptObject stores doc as run's retained build.xml object.
func storeScriptObject(t *testing.T, blobs *fakeBlobStore, run recordstore.Run, doc string) {
	t.Helper()
	key, ok := recordstore.RunScriptObjectKey(run)
	if !ok {
		t.Fatalf("run names no build record")
	}
	blobs.objects[key] = []byte(doc)
}

// TestParseRunScript_CellFixture parses a build.xml captured from a real
// cell build, so the XStream element names are proven against Jenkins and
// not against text a test author wrote.
func TestParseRunScript_CellFixture(t *testing.T) {
	f, err := os.Open("testdata/cell_build.xml")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	script, loaded, sandbox, err := parseRunScript(f)
	if err != nil {
		t.Fatalf("parseRunScript: %v", err)
	}
	if !strings.HasPrefix(script, "pipeline { agent { label 'smoke-a-default' }") {
		t.Fatalf("script opens with %q, want the pipeline block", script[:min(len(script), 60)])
	}
	if !strings.Contains(script, `<testcase classname="com.example.SmokeTest" name="passes" time="0.25"/>`) {
		t.Fatalf("script lost its decoded entities: %q", script)
	}
	if !strings.Contains(script, "junit 'results.xml'") {
		t.Fatalf("script = %q, want the junit step", script)
	}
	if loaded == nil || len(loaded) != 0 {
		t.Fatalf("loaded = %#v, want an empty non-nil map for the fixture's empty linked-hash-map", loaded)
	}
	if !sandbox {
		t.Fatal("sandbox = false, want the fixture's true")
	}
}

// TestParseRunScript_LoadedScriptsEntries parses a hand-written document,
// because the captured fixture's loadedScripts map is empty. The document
// holds two well-formed entries, one entry of another shape, and a
// "script" element outside the execution element that the reader must
// not bind.
func TestParseRunScript_LoadedScriptsEntries(t *testing.T) {
	doc := `<?xml version='1.1' encoding='UTF-8'?>
<flow-build>
  <actions>
    <some.Action><script>not this one</script></some.Action>
  </actions>
  <execution class="org.jenkinsci.plugins.workflow.cps.CpsFlowExecution">
    <result>SUCCESS</result>
    <script>echo 'main'</script>
    <loadedScripts class="linked-hash-map">
      <entry>
        <string>org.example.Lib</string>
        <string>def call() { echo 'lib' }</string>
      </entry>
      <entry>
        <string>only-a-key</string>
      </entry>
      <entry>
        <string>org.example.Other</string>
        <string>def other() {}</string>
      </entry>
    </loadedScripts>
    <sandbox>false</sandbox>
  </execution>
</flow-build>`
	script, loaded, sandbox, err := parseRunScript(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parseRunScript: %v", err)
	}
	if script != "echo 'main'" {
		t.Fatalf("script = %q, want the execution element's own script", script)
	}
	want := map[string]string{
		"org.example.Lib":   "def call() { echo 'lib' }",
		"org.example.Other": "def other() {}",
	}
	if len(loaded) != len(want) || loaded["org.example.Lib"] != want["org.example.Lib"] || loaded["org.example.Other"] != want["org.example.Other"] {
		t.Fatalf("loaded = %#v, want %#v", loaded, want)
	}
	if sandbox {
		t.Fatal("sandbox = true, want false")
	}
}

// TestParseRunScript_RefusesAMissingScript asserts a document with no
// execution element, and an execution element with no script child, each
// answer an error.
func TestParseRunScript_RefusesAMissingScript(t *testing.T) {
	for name, doc := range map[string]string{
		"no execution": "<?xml version='1.1' encoding='UTF-8'?><flow-build><result>SUCCESS</result></flow-build>",
		"no script":    "<flow-build><execution><sandbox>true</sandbox></execution></flow-build>",
		"not xml":      "this is not a build record",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseRunScript(strings.NewReader(doc)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// getRunScript reads GET /runs/{run}/script over the run fixture server,
// which grants read on test-ns/ctrl-a.
func getRunScript(t *testing.T, reader *fakeRecordReader, blobs *fakeBlobStore) (int, RunScript, string) {
	t.Helper()
	srv := newRunFixtureServer(reader, blobs)
	rec := doRunRequest(t, srv.handleGetRunScript, "/runs/"+scriptRunID+"/script", map[string]string{"run": scriptRunID}, nil)
	var out RunScript
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, out, rec.Body.String()
}

// TestGetRunScript_ReplayableRunAnswersItsScript asserts a completed,
// sandboxed, script-defined run with a stored replica answers its own
// script and replays.
func TestGetRunScript_ReplayableRunAnswersItsScript(t *testing.T) {
	reader := newFakeRecordReader()
	run := replicatedRun(t, "test-ns/ctrl-a", 300)
	reader.runs[scriptRunID] = run
	blobs := newFakeBlobStore()
	fixture, err := os.ReadFile("testdata/cell_build.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	storeScriptObject(t, blobs, run, string(fixture))

	code, out, body := getRunScript(t, reader, blobs)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", code, body)
	}
	if !out.Replayable || out.Reason != "" {
		t.Fatalf("replayable = %v, reason = %q, want true and empty", out.Replayable, out.Reason)
	}
	if out.DefinitionKind != definitionKindScript {
		t.Fatalf("definitionKind = %q, want script", out.DefinitionKind)
	}
	if !strings.Contains(out.Script, "junit 'results.xml'") {
		t.Fatalf("script = %q, want the fixture's script", out.Script)
	}
	for _, field := range []string{`"script":`, `"loadedScripts":{}`, `"definitionKind":`, `"replayable":`, `"reason":""`} {
		if !strings.Contains(body, field) {
			t.Fatalf("body = %s, want the field %s present", body, field)
		}
	}
}

// TestGetRunScript_EachRefusalNamesItsReason drives each eligibility rule
// from a seeded record and a seeded blob object. Every refused run
// answers an empty script and an empty, non-null loadedScripts object.
func TestGetRunScript_EachRefusalNamesItsReason(t *testing.T) {
	sandboxed := buildXMLDoc("echo 'hi'", "true")
	tests := []struct {
		name     string
		mutate   func(*recordstore.Run)
		doc      string
		noObject bool
		want     string
		wantKind string
	}{
		{
			name:   "running run",
			mutate: func(r *recordstore.Run) { r.Status = recordstore.StatusRunning },
			doc:    sandboxed, want: replayReasonNotTerminal, wantKind: definitionKindScript,
		},
		{
			name: "template name",
			mutate: func(r *recordstore.Run) {
				r.Payload = mergePayload(t, r.Payload, map[string]any{"template_name": "go-service"})
			},
			doc: sandboxed, want: replayReasonTemplateDefined, wantKind: definitionKindTemplate,
		},
		{
			name: "marker path",
			mutate: func(r *recordstore.Run) {
				r.Payload = mergePayload(t, r.Payload, map[string]any{"marker_path": ".varroa/pipeline.yaml"})
			},
			doc: sandboxed, want: replayReasonTemplateDefined, wantKind: definitionKindTemplate,
		},
		{
			name:   "masked parameter",
			mutate: func(r *recordstore.Run) { r.Params = json.RawMessage(`{"TOKEN":"<masked>","BRANCH":"main"}`) },
			doc:    sandboxed, want: replayReasonMaskedParameter, wantKind: definitionKindScript,
		},
		{
			name:   "no file list",
			mutate: func(r *recordstore.Run) { r.Payload = json.RawMessage(`{"accepted_at":"2026-01-01T00:00:00Z"}`) },
			doc:    sandboxed, want: replayReasonNoReplica, wantKind: definitionKindScript,
		},
		{
			name:     "object gone",
			mutate:   func(*recordstore.Run) {},
			noObject: true, want: replayReasonNoReplica, wantKind: definitionKindScript,
		},
		{
			name: "recorded size over the bound",
			mutate: func(r *recordstore.Run) {
				*r = replicatedRun(t, r.Tenant, runScriptMaxObjectBytes+1)
			},
			doc: sandboxed, want: replayReasonUnreadable, wantKind: definitionKindScript,
		},
		{
			name:   "no execution script",
			mutate: func(*recordstore.Run) {},
			doc:    "<?xml version='1.1' encoding='UTF-8'?><flow-build><execution><sandbox>true</sandbox></execution></flow-build>",
			want:   replayReasonUnreadable, wantKind: definitionKindScript,
		},
		{
			name:   "not sandboxed",
			mutate: func(*recordstore.Run) {},
			doc:    buildXMLDoc("echo 'hi'", "false"),
			want:   replayReasonNotSandbox, wantKind: definitionKindScript,
		},
		{
			name:   "script over 256 KiB",
			mutate: func(*recordstore.Run) {},
			doc:    buildXMLDoc(strings.Repeat("a", runScriptMaxScriptBytes+1), "true"),
			want:   replayReasonScriptTooLarge, wantKind: definitionKindScript,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeRecordReader()
			run := replicatedRun(t, "test-ns/ctrl-a", 300)
			blobs := newFakeBlobStore()
			if !tc.noObject {
				storeScriptObject(t, blobs, run, tc.doc)
			}
			tc.mutate(&run)
			reader.runs[scriptRunID] = run

			code, out, body := getRunScript(t, reader, blobs)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200", code, body)
			}
			if out.Replayable || out.Reason != tc.want {
				t.Fatalf("replayable = %v, reason = %q, want false and %q", out.Replayable, out.Reason, tc.want)
			}
			if out.DefinitionKind != tc.wantKind {
				t.Fatalf("definitionKind = %q, want %q", out.DefinitionKind, tc.wantKind)
			}
			if out.Script != "" {
				t.Fatalf("script = %q, want empty for a refused run", out.Script)
			}
			if !strings.Contains(body, `"loadedScripts":{}`) {
				t.Fatalf("body = %s, want an empty loadedScripts object", body)
			}
		})
	}
}

// TestGetRunScript_UnreadableRunIs404 asserts a run the caller may not
// read answers 404, and no object read reveals it.
func TestGetRunScript_UnreadableRunIs404(t *testing.T) {
	reader := newFakeRecordReader()
	run := replicatedRun(t, "other-ns/ctrl-b", 300)
	reader.runs[scriptRunID] = run
	blobs := newFakeBlobStore()
	storeScriptObject(t, blobs, run, buildXMLDoc("echo 'secret'", "true"))

	code, _, body := getRunScript(t, reader, blobs)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", code, body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("body = %s, want no script for an unreadable run", body)
	}
}

// TestGetRunScript_UnknownRunIs404 asserts an unknown run answers 404.
func TestGetRunScript_UnknownRunIs404(t *testing.T) {
	code, _, body := getRunScript(t, newFakeRecordReader(), newFakeBlobStore())
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", code, body)
	}
}

// mergePayload answers payload with extra's keys set on it.
func mergePayload(t *testing.T, payload json.RawMessage, extra map[string]any) json.RawMessage {
	t.Helper()
	doc := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &doc); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
	}
	for k, v := range extra {
		doc[k] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}
