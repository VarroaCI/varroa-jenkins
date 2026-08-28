package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Bundle validation and action tests (6.4)
// ---------------------------------------------------------------------------

func TestBundle_ValidateBareSpec(t *testing.T) {
	testSetup(t)
	var body map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = fmt.Fprint(w, `{"valid":true}`)
	})
	defer srv.Close()

	tmp := tempFile(t, "spec.yaml", "replicas: 3\nitems:\n  - item1\n")
	root := newRootCmd()
	root.SetArgs([]string{"validate", "bundle", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("validate bundle failed: %v", err)
	}
	if body == nil || body["replicas"] != float64(3) {
		t.Errorf("expected bare spec passthrough, got %v", body)
	}
}

func TestBundle_ValidateCRUnwrap(t *testing.T) {
	testSetup(t)
	var body map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = fmt.Fprint(w, `{"valid":true}`)
	})
	defer srv.Close()

	tmp := tempFile(t, "cr.yaml", "apiVersion: v1\nkind: ComposedBundle\nmetadata:\n  name: test\nspec:\n  replicas: 3\n")
	root := newRootCmd()
	root.SetArgs([]string{"validate", "bundle", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("validate bundle CR failed: %v", err)
	}
	if body == nil || body["replicas"] != float64(3) {
		t.Errorf("expected spec extraction, got %v", body)
	}
}

func TestBundle_ValidateCRWithoutSpec(t *testing.T) {
	testSetup(t)
	tmp := tempFile(t, "cr.yaml", "apiVersion: v1\nkind: ComposedBundle\nmetadata:\n  name: test\n")
	root := newRootCmd()
	root.SetArgs([]string{"validate", "bundle", "-f", tmp})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for CR without spec")
	}
	if !strings.Contains(err.Error(), "file is a CR but has no spec") {
		t.Errorf("expected 'no spec' error, got %v", err)
	}
}

func TestBundle_ValidateValidFalse(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"valid":false,"errors":["item ref missing"]}`)
	})
	defer srv.Close()

	tmp := tempFile(t, "spec.yaml", "replicas: 3\n")
	root := newRootCmd()
	root.SetArgs([]string{"validate", "bundle", "-f", tmp})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for valid:false")
	}
}

func TestBundle_PreviewRequiresN(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"preview", "bundle", "-f", "/dev/null"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error for missing -n")
	}
}

func TestBundle_Pause(t *testing.T) {
	testSetup(t)
	var (
		path   string
		method string
	)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		_, _ = fmt.Fprint(w, `{"status":"paused"}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"pause", "bundle", "ns1/b1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("pause bundle failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/composedbundles/ns1/b1/pause") {
		t.Errorf("expected /api/v1/clusters/core/composedbundles/ns1/b1/pause, got %s", path)
	}
	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
}

func TestBundle_Resume(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = fmt.Fprint(w, `{"status":"resumed"}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"resume", "bundle", "ns1/b1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("resume bundle failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/composedbundles/ns1/b1/resume") {
		t.Errorf("expected /api/v1/clusters/core/composedbundles/ns1/b1/resume, got %s", path)
	}
}

func TestBundle_PausedColumn(t *testing.T) {
	// Paused=true
	item := map[string]any{
		"metadata": map[string]any{
			"name":      "b1",
			"namespace": "ns1",
			"annotations": map[string]any{
				"varroa.dev/rollout-paused": "true",
			},
		},
		"status": map[string]any{
			"phase":     "Ready",
			"itemCount": float64(3),
		},
	}
	cols := bundleColumns(item)
	if len(cols) < 5 || cols[4] != "true" {
		t.Errorf("expected PAUSED=true, got %v", cols)
	}

	// Paused=false (no annotation)
	item2 := map[string]any{
		"metadata": map[string]any{
			"name": "b2",
		},
		"status": map[string]any{
			"phase":     "Pending",
			"itemCount": float64(0),
		},
	}
	cols2 := bundleColumns(item2)
	if len(cols2) >= 5 && cols2[4] != "" {
		t.Errorf("expected PAUSED=\"\", got %q", cols2[4])
	}
}
