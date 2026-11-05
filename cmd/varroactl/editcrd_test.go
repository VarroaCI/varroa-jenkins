package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// editLoop golden tests
// ---------------------------------------------------------------------------

// runEditorStub is a test helper that replaces runEditorC3.
type runEditorStub struct {
	// writeContent, if non-empty, is written to the temp file instead of the original.
	writeContent string
	// err, if non-nil, is returned from the editor.
	err error
}

func (s runEditorStub) edit(path string) error {
	if s.err != nil {
		return s.err
	}
	if s.writeContent != "" {
		return os.WriteFile(path, []byte(s.writeContent), 0644)
	}
	return nil
}

// TestEditLoop_ChangeSendsPut verifies that editing the fetched doc and
// writing a changed YAML file results in a PUT with the full edited doc.
func TestEditLoop_ChangeSendsPut(t *testing.T) {
	fetched := map[string]any{
		"apiVersion": "v1",
		"kind":       "Test",
		"metadata": map[string]any{
			"name":              "myobj",
			"resourceVersion":   "12345",
			"uid":               "abc-123",
			"generation":        float64(1),
			"creationTimestamp": "2024-01-01T00:00:00Z",
		},
		"spec": map[string]any{
			"replicas": float64(3),
		},
		"status": map[string]any{
			"phase": "Ready",
		},
	}

	var putCalled bool
	var putDoc map[string]any

	stub := runEditorStub{
		// Edit: change replicas from 3 to 5
		writeContent: `apiVersion: v1
kind: Test
metadata:
  name: myobj
spec:
  replicas: 5
`,
	}

	origEditor := runEditorC3
	runEditorC3 = stub.edit
	defer func() { runEditorC3 = origEditor }()

	err := editLoop(
		func() (map[string]any, error) { return fetched, nil },
		func(_, mod map[string]any) error {
			putCalled = true
			putDoc = mod
			return nil
		},
		"test", "myobj",
	)

	if err != nil {
		t.Fatalf("editLoop returned error: %v", err)
	}
	if !putCalled {
		t.Fatal("put was not called")
	}
	if putDoc == nil {
		t.Fatal("putDoc is nil")
	}
	// Verify the status and metadata noise were stripped from the PUT doc
	if _, ok := putDoc["status"]; ok {
		t.Error("status field should have been stripped from PUT doc")
	}
	meta, ok := putDoc["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata missing from PUT doc")
	}
	if got, _ := meta["resourceVersion"].(string); got != "12345" {
		t.Errorf("PUT resourceVersion = %q, want the version the GET read (12345)", got)
	}
	if _, ok := meta["uid"]; ok {
		t.Error("uid should have been stripped")
	}
	if _, ok := meta["generation"]; ok {
		t.Error("generation should have been stripped")
	}
	if _, ok := meta["creationTimestamp"]; ok {
		t.Error("creationTimestamp should have been stripped")
	}
	if _, ok := meta["managedFields"]; ok {
		t.Error("managedFields should have been stripped")
	}
	// Verify the edit took effect
	spec, ok := putDoc["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec missing from PUT doc")
	}
	replicas, ok := spec["replicas"].(float64)
	if !ok {
		t.Fatal("replicas missing or wrong type")
	}
	if replicas != 5 {
		t.Errorf("expected replicas=5, got %v", replicas)
	}
}

// TestEditLoop_NoOpDoesNotCallPut verifies that an edit with no changes
// prints a cancellation message and does not call put.
func TestEditLoop_NoOpDoesNotCallPut(t *testing.T) {
	fetched := map[string]any{
		"apiVersion": "v1",
		"kind":       "Test",
		"metadata": map[string]any{
			"name": "myobj",
		},
		"spec": map[string]any{
			"replicas": float64(3),
		},
	}

	var putCalled bool

	stub := runEditorStub{
		// Write back exactly the same content (after stripping noise)
		writeContent: `apiVersion: v1
kind: Test
metadata:
  name: myobj
spec:
  replicas: 3
`,
	}

	origEditor := runEditorC3
	runEditorC3 = stub.edit
	defer func() { runEditorC3 = origEditor }()

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := editLoop(
		func() (map[string]any, error) { return fetched, nil },
		func(_, _ map[string]any) error {
			putCalled = true
			return nil
		},
		"test", "myobj",
	)

	w.Close()
	os.Stdout = oldStdout
	var stdoutBuf strings.Builder
	_ = copyBuffer(&stdoutBuf, r)

	if err != nil {
		t.Fatalf("editLoop returned error: %v", err)
	}
	if putCalled {
		t.Fatal("put should not have been called for no-op edit")
	}
	if !strings.Contains(stdoutBuf.String(), "Edit cancelled") {
		t.Errorf("expected cancellation message, got: %s", stdoutBuf.String())
	}
}

// TestEditLoop_ParseErrorKeepsTemp verifies that a parse error prints the
// temp file path and returns an error.
func TestEditLoop_ParseErrorKeepsTemp(t *testing.T) {
	fetched := map[string]any{
		"apiVersion": "v1",
		"kind":       "Test",
		"metadata": map[string]any{
			"name": "myobj",
		},
		"spec": map[string]any{
			"replicas": float64(3),
		},
	}

	stub := runEditorStub{
		// Write invalid YAML
		writeContent: `{{{{{ invalid yaml }}}}}`,
	}

	origEditor := runEditorC3
	runEditorC3 = stub.edit
	defer func() { runEditorC3 = origEditor }()

	var putCalled bool

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := editLoop(
		func() (map[string]any, error) { return fetched, nil },
		func(_, _ map[string]any) error {
			putCalled = true
			return nil
		},
		"test", "myobj",
	)

	w.Close()
	os.Stderr = oldStderr
	var stderrBuf strings.Builder
	_ = copyBuffer(&stderrBuf, r)

	if err == nil {
		t.Fatal("expected error from parse failure")
	}
	if putCalled {
		t.Fatal("put should not have been called for parse error")
	}
	if !strings.Contains(stderrBuf.String(), "Error: failed to parse edited YAML") {
		t.Errorf("expected parse error on stderr, got: %s", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "varroactl-edit-") {
		t.Errorf("expected temp file path on stderr, got: %s", stderrBuf.String())
	}
}

// TestEditLoop_StatusStripped verifies that status and metadata noise keys
// are stripped from the buffer before sending PUT.
func TestEditLoop_StatusStripped(t *testing.T) {
	fetched := map[string]any{
		"apiVersion": "v1",
		"kind":       "Test",
		"metadata": map[string]any{
			"name":              "myobj",
			"namespace":         "default",
			"resourceVersion":   "999",
			"uid":               "abc-def",
			"generation":        float64(2),
			"creationTimestamp": "2024-06-01T00:00:00Z",
			"managedFields":     []any{map[string]any{"manager": "test"}},
			"labels":            map[string]any{"env": "prod"},
		},
		"spec": map[string]any{
			"replicas": float64(3),
		},
		"status": map[string]any{
			"phase": "Ready",
			"conditions": []any{
				map[string]any{"type": "Available"},
			},
		},
	}

	var putDoc map[string]any

	stub := runEditorStub{
		// Edit replicas
		writeContent: `apiVersion: v1
kind: Test
metadata:
  labels:
    env: prod
  name: myobj
  namespace: default
spec:
  replicas: 5
`,
	}

	origEditor := runEditorC3
	runEditorC3 = stub.edit
	defer func() { runEditorC3 = origEditor }()

	err := editLoop(
		func() (map[string]any, error) { return fetched, nil },
		func(_, mod map[string]any) error {
			putDoc = mod
			return nil
		},
		"test", "myobj",
	)

	if err != nil {
		t.Fatalf("editLoop returned error: %v", err)
	}
	if putDoc == nil {
		t.Fatal("putDoc is nil")
	}

	// Status must be absent
	if _, ok := putDoc["status"]; ok {
		t.Error("status should be stripped")
	}

	meta, ok := putDoc["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata missing")
	}

	// These must be stripped
	for _, k := range []string{"uid", "generation", "creationTimestamp", "managedFields"} {
		if _, ok := meta[k]; ok {
			t.Errorf("%s should have been stripped", k)
		}
	}

	// Labels should be preserved
	if _, ok := meta["labels"]; !ok {
		t.Error("labels should be preserved")
	}
}

// ---------------------------------------------------------------------------
// TODO: Canary test asserting every C3 verb exists
//
// This test is DEFERRED to the final C3 batch — the verbs (validate, sync,
// passwd, apikey, activity, search, events, mite, watch, mcp, pause, resume;
// preview bundle; logs -f; get controller -w) don't exist yet. When the last
// C3 file lands, uncomment and run:
//
//   func TestEditLoop_AllC3VerbsExist(t *testing.T) {
//       root := newRootCmd()
//       expected := []string{"validate", "sync", "passwd", "apikey",
//           "activity", "search", "events", "mite", "watch", "mcp",
//           "pause", "resume"}
//       for _, v := range expected {
//           if _, _, err := root.Find([]string{v}); err != nil {
//               t.Errorf("expected verb %q to exist but it was not found", v)
//           }
//       }
//       // Check preview bundle exists
//       if _, _, err := root.Find([]string{"preview", "bundle"}); err != nil {
//           t.Error("expected 'preview bundle' to exist")
//       }
//       // Check logs -f flag
//       logsCmd, _, _ := root.Find([]string{"logs"})
//       if logsCmd == nil {
//           t.Fatal("logs command not found")
//       }
//       if logsCmd.Flag("follow") == nil && logsCmd.Flag("f") == nil {
//           t.Error("logs should have -f/--follow flag")
//       }
//       // Check apply does NOT exist
//       if _, _, err := root.Find([]string{"apply"}); err == nil {
//           t.Error("apply should NOT be a verb")
//       }
//   }
// ---------------------------------------------------------------------------

// TestMain is already in attach_test.go, no need to duplicate.

// copyBuffer copies data from an os.File to a strings.Builder.
func copyBuffer(buf *strings.Builder, r *os.File) error {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			buf.Write(b[:n])
		}
		if err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------

// TestEditProvisioningDefaults_SendsResourceVersion verifies that the PUT
// body carries the resourceVersion the editor's GET returned, and that the
// editor buffer does not show it.
func TestEditProvisioningDefaults_SendsResourceVersion(t *testing.T) {
	testSetup(t)
	var putBody string
	var editorBuf string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			putBody = string(b)
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"varroa-defaults","resourceVersion":"7"},"spec":{"rootDomain":"a.example"}}`)
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = func(path string) error {
		raw, _ := os.ReadFile(path)
		editorBuf = string(raw)
		return os.WriteFile(path, []byte("metadata:\n  name: varroa-defaults\nspec:\n  rootDomain: b.example\n"), 0644)
	}
	defer func() { runEditorC3 = origEditor }()

	root := newRootCmd()
	root.SetArgs([]string{"edit", "provisioningdefaults"})
	if err := root.Execute(); err != nil {
		t.Fatalf("edit provisioningdefaults failed: %v", err)
	}
	if strings.Contains(editorBuf, "resourceVersion") {
		t.Errorf("editor buffer must not show resourceVersion, got:\n%s", editorBuf)
	}
	var sent struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(putBody), &sent); err != nil {
		t.Fatalf("PUT body is not JSON: %v: %s", err, putBody)
	}
	if sent.Metadata.ResourceVersion != "7" {
		t.Errorf("PUT resourceVersion = %q, want 7; body: %s", sent.Metadata.ResourceVersion, putBody)
	}
}

// TestEditProvisioningDefaults_ConflictExitsNonZero verifies that a 409 from
// the server stops the edit with the conflict message on stderr.
func TestEditProvisioningDefaults_ConflictExitsNonZero(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":"provisioningdefaults changed after it was read"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"varroa-defaults","resourceVersion":"7"},"spec":{"rootDomain":"a.example"}}`)
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = runEditorStub{writeContent: "metadata:\n  name: varroa-defaults\nspec:\n  rootDomain: b.example\n"}.edit
	defer func() { runEditorC3 = origEditor }()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newRootCmd()
	root.SetArgs([]string{"edit", "provisioningdefaults"})
	err := root.Execute()

	_ = w.Close()
	os.Stderr = oldStderr
	var stderrBuf strings.Builder
	_ = copyBuffer(&stderrBuf, r)

	if err == nil {
		t.Fatal("expected a non-zero exit on a conflict")
	}
	if !strings.Contains(stderrBuf.String(), "the object changed while you edited it; run the edit again") {
		t.Errorf("expected conflict message on stderr, got: %s", stderrBuf.String())
	}
}

// TestStripEditNoise_DropsEmptyMetadata asserts that stripEditNoise removes a
// metadata map that it leaves empty, and keeps a metadata map that has a name.
func TestStripEditNoise_DropsEmptyMetadata(t *testing.T) {
	onlyVersion := map[string]any{"metadata": map[string]any{"resourceVersion": "7"}}
	stripEditNoise(onlyVersion)
	if _, ok := onlyVersion["metadata"]; ok {
		t.Fatalf("metadata = %v, want the empty map dropped", onlyVersion["metadata"])
	}

	withName := map[string]any{"metadata": map[string]any{"name": "x", "resourceVersion": "7"}}
	stripEditNoise(withName)
	meta, ok := withName["metadata"].(map[string]any)
	if !ok || meta["name"] != "x" {
		t.Fatalf("metadata = %v, want metadata.name kept", withName["metadata"])
	}
}
