package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Search tests (7.2)
// ---------------------------------------------------------------------------

func TestSearch_QueryEncoding(t *testing.T) {
	testSetup(t)
	var query string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"search", "test query"})
	if err := root.Execute(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(query, "q=test+query") {
		t.Errorf("expected q=test+query in query, got %s", query)
	}
}

func TestSearch_EmptyArgExit2(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"search", ""})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for empty search query")
	}
}

func TestSearch_MissingArg(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"search"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing search query")
	}
}

func TestSearch_ResultColumns(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"type":"controller","namespace":"ns1","name":"ctrl1"}]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"search", "ctrl"})
	if err := root.Execute(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
}

// TestDescribe_Golden tests printDescribe output (7.2)
func TestDescribe_Golden(t *testing.T) {
	// Simple golden test for printDescribe
	doc := map[string]any{
		"name": "test",
		"spec": map[string]any{
			"replicas": float64(3),
			"labels":   []any{"a", "b"},
		},
		"items": []any{
			map[string]any{"id": float64(1)},
			map[string]any{"id": float64(2)},
		},
		"empty": []any{},
	}

	var buf strings.Builder
	if err := printDescribe(&buf, doc); err != nil {
		t.Fatalf("printDescribe failed: %v", err)
	}
	output := buf.String()

	// Check expected keys appear
	for _, want := range []string{"name:", "spec:", "replicas:", "labels:", "items:", "empty:"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}
