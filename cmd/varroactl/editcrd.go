package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"sigs.k8s.io/yaml"
)

// editConflictMessage is printed when the server refuses the write because
// another writer changed the object after the edit read it.
const editConflictMessage = "the object changed while you edited it; run the edit again"

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

// editLoop implements the edit flow (design §3.8).
//
// fetch is called to get the current document. It must return the full
// server representation (incl. status, metadata fields).
//
// Before presenting to the editor, status and selected metadata fields
// (managedFields, resourceVersion, uid, generation, creationTimestamp)
// are stripped. The resourceVersion the fetch returned is kept aside and
// set on the edited document before put runs, so the write carries the
// version the editor read. After the user edits, the file is re-parsed and
// if changed, put is called with the stripped fetched document and the
// edited document (which carries that resourceVersion).
// A noun with no patch callback sends put's second argument as a
// full-replace PUT and ignores the first; a noun with a patch callback
// computes the merge patch between the two and sends that instead.
func editLoop(fetch func() (map[string]any, error), put func(orig, mod map[string]any) error, kind, name string) error {
	doc, err := fetch()
	if err != nil {
		return err
	}

	// The version the editor read. The buffer never shows it; the write
	// sends it back so the server can refuse a stale edit.
	resourceVersion := metadataString(doc, "resourceVersion")

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

	// Only mod carries the version the editor read. A merge patch built from
	// orig and mod then includes metadata.resourceVersion. A noun whose fetch
	// returns no version sends none.
	if resourceVersion != "" {
		setMetadataField(mod, "resourceVersion", resourceVersion)
	}

	// Send the change: a full-replace PUT, or a merge patch when the noun
	// supplies a patch callback (put decides which).
	if err := put(doc, mod); err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.code == http.StatusConflict {
			_, _ = fmt.Fprintln(os.Stderr, editConflictMessage)
			_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
			return fmt.Errorf("edit conflict")
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("edit failed")
	}

	_ = os.Remove(tmpPath)
	fmt.Printf("%s %q edited\n", kind, name)
	return nil
}

// metadataString returns metadata.<key> of doc, or "" when it is absent.
func metadataString(doc map[string]any, key string) string {
	meta, ok := doc["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := meta[key].(string)
	return v
}

// setMetadataField sets metadata.<key> of doc, creating metadata when absent.
func setMetadataField(doc map[string]any, key, value string) {
	meta, ok := doc["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta[key] = value
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
