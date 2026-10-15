package recordstore

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// scriptKeyRunID is the run identifier every RunScriptObjectKey case uses.
const scriptKeyRunID = "0199aa11-2222-7333-8444-555566667777"

// runWithMeshFiles builds a Run whose payload holds files as its mesh
// file list.
func runWithMeshFiles(t *testing.T, files []runStateFile) Run {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mesh": map[string]any{"files": files}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return Run{RunID: scriptKeyRunID, Payload: raw}
}

// TestRunScriptObjectKey covers a record with one build-record entry, a
// record with none, a record whose list is absent, and the three-rule
// order over two build-record entries.
func TestRunScriptObjectKey(t *testing.T) {
	const runID = scriptKeyRunID
	const buildXML = "jobs/t/jobs/j/builds/7/build.xml"
	const program = "jobs/t/jobs/j/builds/7/program.dat"

	tests := []struct {
		name     string
		run      Run
		wantKey  string
		wantSize int64
		wantOK   bool
	}{
		{
			name: "one build record",
			run: runWithMeshFiles(t, []runStateFile{
				{RelPath: program, Bytes: 900, Seq: 1, StateGen: 4, Epoch: 10},
				{RelPath: buildXML, Bytes: 300, Seq: 1, StateGen: 3, Epoch: 10},
			}),
			wantKey:  blob.RunStateKey(runID, 10, 3, buildXML),
			wantSize: 300,
			wantOK:   true,
		},
		{
			name:   "no build record",
			run:    runWithMeshFiles(t, []runStateFile{{RelPath: program, Bytes: 900, StateGen: 4, Epoch: 10}}),
			wantOK: false,
		},
		{
			name:   "absent list",
			run:    Run{RunID: runID, Payload: json.RawMessage(`{"accepted_at":"2026-01-01T00:00:00Z"}`)},
			wantOK: false,
		},
		{
			name:   "null payload",
			run:    Run{RunID: runID},
			wantOK: false,
		},
		{
			name:   "entry that names no object",
			run:    runWithMeshFiles(t, []runStateFile{{RelPath: buildXML, Bytes: 300, StateGen: 0, Epoch: 10}}),
			wantOK: false,
		},
		{
			name: "highest epoch wins",
			run: runWithMeshFiles(t, []runStateFile{
				{RelPath: "a/build.xml", Bytes: 1, StateGen: 9, Epoch: 10},
				{RelPath: "b/build.xml", Bytes: 2, StateGen: 1, Epoch: 11},
			}),
			wantKey:  blob.RunStateKey(runID, 11, 1, "b/build.xml"),
			wantSize: 2,
			wantOK:   true,
		},
		{
			name: "highest generation wins inside one epoch",
			run: runWithMeshFiles(t, []runStateFile{
				{RelPath: "a/build.xml", Bytes: 1, StateGen: 9, Epoch: 10},
				{RelPath: "b/build.xml", Bytes: 2, StateGen: 8, Epoch: 10},
			}),
			wantKey:  blob.RunStateKey(runID, 10, 9, "a/build.xml"),
			wantSize: 1,
			wantOK:   true,
		},
		{
			name: "shortest path breaks a tie",
			run: runWithMeshFiles(t, []runStateFile{
				{RelPath: "long/path/build.xml", Bytes: 1, StateGen: 9, Epoch: 10},
				{RelPath: "build.xml", Bytes: 2, StateGen: 9, Epoch: 10},
			}),
			wantKey:  blob.RunStateKey(runID, 10, 9, "build.xml"),
			wantSize: 2,
			wantOK:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := RunScriptObjectKey(tc.run)
			if ok != tc.wantOK || key != tc.wantKey {
				t.Fatalf("RunScriptObjectKey = (%q, %v), want (%q, %v)", key, ok, tc.wantKey, tc.wantOK)
			}
			objKey, size, objOK := RunScriptObject(tc.run)
			if objOK != tc.wantOK || objKey != tc.wantKey || size != tc.wantSize {
				t.Fatalf("RunScriptObject = (%q, %d, %v), want (%q, %d, %v)",
					objKey, size, objOK, tc.wantKey, tc.wantSize, tc.wantOK)
			}
		})
	}
}
