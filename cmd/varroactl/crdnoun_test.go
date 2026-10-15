package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// buildTestRootForNoun assembles a minimal command tree carrying only the
// verb parents and persistent flags addCRDNoun/crdnoun.go's shared runners
// need, then attaches opts to it. It never touches the real nounRegistrars,
// so a synthetic test noun never leaks into another test's command tree.
func buildTestRootForNoun(opts crdNounOpts) *cobra.Command {
	root := &cobra.Command{Use: "root", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("context", "", "")
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().StringP("namespace", "n", "", "")
	root.PersistentFlags().BoolP("all-namespaces", "A", false, "")
	root.PersistentFlags().StringP("output", "o", "table", "")
	root.PersistentFlags().Bool("no-headers", false, "")

	verbs := &verbCommands{
		get:      newVerbParent("get", ""),
		describe: newVerbParent("describe", ""),
		create:   newVerbParent("create", ""),
		delete:   newVerbParent("delete", ""),
		edit:     newVerbParent("edit", ""),
		patch:    newVerbParent("patch", ""),
	}
	root.AddCommand(verbs.get, verbs.describe, verbs.create, verbs.delete, verbs.edit, verbs.patch)
	addCRDNoun(verbs, opts)
	return root
}

func widgetColumns(item map[string]any) []string {
	return []string{itemName(item)}
}

// ---------------------------------------------------------------------------
// Task 10.0: a noun that supplies all five callbacks never reaches
// rawRequest. The test server fails the test on any request it receives, so
// a callback path is the only way every verb below can succeed.
// ---------------------------------------------------------------------------

func TestCRDNoun_CallbacksBypassRawRequest(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected raw HTTP request via rawRequest: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	var listCalled, getCalled, createCalled, patchCalled, deleteCalled bool
	opts := crdNounOpts{
		Noun:    "widget",
		Path:    "/widgets",
		Headers: []string{"NAME"},
		Columns: widgetColumns,
		List: func(c *client.ClientWithResponses, cmd *cobra.Command) ([]map[string]any, error) {
			listCalled = true
			return []map[string]any{{"metadata": map[string]any{"name": "w1"}}}, nil
		},
		Get: func(c *client.ClientWithResponses, cmd *cobra.Command, name string) (map[string]any, error) {
			getCalled = true
			return map[string]any{"metadata": map[string]any{"name": name}}, nil
		},
		Create: func(c *client.ClientWithResponses, cmd *cobra.Command, body map[string]any) (map[string]any, error) {
			createCalled = true
			return map[string]any{"name": "w1"}, nil
		},
		Patch: func(c *client.ClientWithResponses, cmd *cobra.Command, name string, mergePatch []byte) (map[string]any, error) {
			patchCalled = true
			return map[string]any{"name": name}, nil
		},
		Delete: func(c *client.ClientWithResponses, cmd *cobra.Command, name string) error {
			deleteCalled = true
			return nil
		},
	}

	root := buildTestRootForNoun(opts)
	root.SetArgs([]string{"get", "widget"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get (list): %v", err)
	}
	if !listCalled {
		t.Error("expected List callback to be called")
	}

	root = buildTestRootForNoun(opts)
	root.SetArgs([]string{"get", "widget", "w1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get (single): %v", err)
	}
	if !getCalled {
		t.Error("expected Get callback to be called")
	}

	tmp := tempFile(t, "widget.yaml", "metadata:\n  name: w1\nspec: {}\n")
	defer os.Remove(tmp)
	root = buildTestRootForNoun(opts)
	root.SetArgs([]string{"create", "widget", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !createCalled {
		t.Error("expected Create callback to be called")
	}

	root = buildTestRootForNoun(opts)
	root.SetArgs([]string{"patch", "widget", "w1", "-p", `{"spec":{"a":1}}`})
	if err := root.Execute(); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if !patchCalled {
		t.Error("expected Patch callback to be called")
	}

	root = buildTestRootForNoun(opts)
	root.SetArgs([]string{"delete", "widget", "w1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleteCalled {
		t.Error("expected Delete callback to be called")
	}
}

// ---------------------------------------------------------------------------
// Task 10.0a: edit routes through the Patch callback as a merge patch, and
// never sends a PUT, when a noun supplies one.
// ---------------------------------------------------------------------------

func TestCRDNoun_EditUsesPatchCallback_NeverPUT(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected raw HTTP request via rawRequest: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	fetched := map[string]any{
		"metadata": map[string]any{"name": "w1"},
		"spec":     map[string]any{"replicas": float64(3)},
	}

	var patchCalled bool
	var patchBody []byte
	opts := crdNounOpts{
		Noun:    "widget",
		Path:    "/widgets",
		Headers: []string{"NAME"},
		Columns: widgetColumns,
		Get: func(c *client.ClientWithResponses, cmd *cobra.Command, name string) (map[string]any, error) {
			return fetched, nil
		},
		Patch: func(c *client.ClientWithResponses, cmd *cobra.Command, name string, mergePatch []byte) (map[string]any, error) {
			patchCalled = true
			patchBody = mergePatch
			return map[string]any{"name": name}, nil
		},
	}

	origEditor := runEditorC3
	runEditorC3 = func(path string) error {
		return os.WriteFile(path, []byte("metadata:\n  name: w1\nspec:\n  replicas: 5\n"), 0644)
	}
	defer func() { runEditorC3 = origEditor }()

	root := buildTestRootForNoun(opts)
	root.SetArgs([]string{"edit", "widget", "w1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !patchCalled {
		t.Fatal("expected Patch callback to be called")
	}

	var patch map[string]any
	if err := json.Unmarshal(patchBody, &patch); err != nil {
		t.Fatalf("merge patch is not valid JSON: %v", err)
	}
	spec, ok := patch["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec in merge patch, got %v", patch)
	}
	if spec["replicas"] != float64(5) {
		t.Errorf("expected replicas=5 in merge patch, got %v", spec["replicas"])
	}
	if _, ok := patch["metadata"]; ok {
		t.Errorf("expected no metadata in merge patch (unchanged), got %v", patch["metadata"])
	}
}

// ---------------------------------------------------------------------------
// Task 10.0b: the "patch" subcommand exists only for a noun that supplies
// the callback.
// ---------------------------------------------------------------------------

func TestCRDNoun_PatchSubcommandOnlyWhenCallbackSupplied(t *testing.T) {
	withCallback := buildTestRootForNoun(crdNounOpts{
		Noun: "widget",
		Path: "/widgets",
		Patch: func(c *client.ClientWithResponses, cmd *cobra.Command, name string, mergePatch []byte) (map[string]any, error) {
			return nil, nil
		},
	})
	found, _, err := withCallback.Find([]string{"patch", "widget"})
	if err != nil || found == nil || found.Name() != "widget" {
		t.Errorf("expected \"patch widget\" to be registered, got %v, err %v", found, err)
	}

	withoutCallback := buildTestRootForNoun(crdNounOpts{
		Noun: "gadget",
		Path: "/gadgets",
	})
	// Find falls back to the "patch" parent (with the unmatched remainder in
	// args) rather than erroring, so the registration check is the found
	// command's own name, not just a nil error.
	found, _, err = withoutCallback.Find([]string{"patch", "gadget"})
	if err == nil && found != nil && found.Name() == "gadget" {
		t.Error("expected \"patch gadget\" to NOT be registered when no Patch callback is supplied")
	}
}
