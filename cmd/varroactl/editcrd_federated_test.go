package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestEditJenkinsRole_FederatedCopyPrintsServerMessage pins that a 409 with a
// machine code prints the server text, not the generic conflict text.
func TestEditJenkinsRole_FederatedCopyPrintsServerMessage(t *testing.T) {
	testSetup(t)
	const serverMsg = "JenkinsRole viewer is a federated copy of VarroaRole alpha. Federation replaces each change to the copy. Change the source on the core cluster."
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"error":%q,"code":"federated_copy"}`, serverMsg)
			return
		}
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"viewer","resourceVersion":"7"},"spec":{"description":"a"}}`)
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = runEditorStub{writeContent: "metadata:\n  name: viewer\nspec:\n  description: b\n"}.edit
	defer func() { runEditorC3 = origEditor }()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newRootCmd()
	root.SetArgs([]string{"edit", "jenkinsrole", "viewer"})
	err := root.Execute()

	_ = w.Close()
	os.Stderr = oldStderr
	var stderrBuf strings.Builder
	_ = copyBuffer(&stderrBuf, r)

	if err == nil {
		t.Fatal("expected a non-zero exit on a refused copy")
	}
	if !strings.Contains(stderrBuf.String(), serverMsg) {
		t.Errorf("stderr must hold the server message, got: %s", stderrBuf.String())
	}
	if strings.Contains(stderrBuf.String(), editConflictMessage) {
		t.Errorf("stderr must not hold the generic conflict message, got: %s", stderrBuf.String())
	}
}
