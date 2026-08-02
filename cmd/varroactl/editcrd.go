package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"sigs.k8s.io/yaml"
)

// runEditorC3 is the injectable editor invocation used by editLoop.
// Tests can replace it with a stub.
var runEditorC3 = func(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		if runtime.GOOS == "windows" {
			editor = "notepad"
		} else {
			editor = "vi"
		}
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// editLoop implements the full-replace edit flow (design §3.8).
//
// fetch is called to get the current document. It must return the full
// server representation (incl. status, metadata fields).
//
// Before presenting to the editor, status and selected metadata fields
// (managedFields, resourceVersion, uid, generation, creationTimestamp)
// are stripped. After the user edits, the file is re-parsed and if
// changed, put is called with the full edited document.
func editLoop(fetch func() (map[string]any, error), put func(doc map[string]any) error, kind, name string) error {
	doc, err := fetch()
	if err != nil {
		return err
	}

	// Strip noise keys before presenting to the editor
	stripEditNoise(doc)

	// Render to YAML
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}

	editHeader := "# Please edit the object below. Lines starting with '#' will be ignored.\n" +
		"# An empty patch (no effective change) cancels the edit. status/immutable metadata edits are ignored.\n#\n"

	tmp, err := os.CreateTemp("", "varroactl-edit-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(editHeader + string(raw)); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	// Run editor
	if err := runEditorC3(tmpPath); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}

	// Read edited file and strip hash comments
	editedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	edited := stripHashComments(string(editedBytes))

	// Parse edited
	var mod map[string]any
	if err := yaml.Unmarshal([]byte(edited), &mod); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: failed to parse edited YAML: %v\n", err)
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("parse error")
	}

	// Compare — full-replace strategy: marshal both and compare
	origBytes := stripHashComments(string(raw))
	if string(editedBytes) == origBytes || reflectDeepEqual(mod, doc) {
		fmt.Println("Edit cancelled, no changes made.")
		_ = os.Remove(tmpPath)
		return nil
	}

	// Send PUT
	if err := put(mod); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("edit failed")
	}

	_ = os.Remove(tmpPath)
	fmt.Printf("%s %q edited\n", kind, name)
	return nil
}

// stripEditNoise removes keys that should never be presented to the editor
// or sent back in the PUT body. It mutates doc in place.
func stripEditNoise(doc map[string]any) {
	delete(doc, "status")

	if meta, ok := doc["metadata"].(map[string]any); ok {
		for _, k := range []string{"managedFields", "resourceVersion", "uid", "generation", "creationTimestamp"} {
			delete(meta, k)
		}
	}
}

// reflectDeepEqual compares two maps for equality.
var reflectDeepEqual = func(a, b map[string]any) bool {
	return deepEqual(a, b)
}

func deepEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !valuesEqual(va, vb) {
			return false
		}
	}
	return true
}

func valuesEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		return deepEqual(am, bm)
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok {
		if len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !valuesEqual(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}
