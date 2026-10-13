package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCatalogsourceColumns_Source(t *testing.T) {
	tests := []struct {
		name string
		item map[string]any
		want string
	}{
		{
			name: "git source shows the repo URL",
			item: map[string]any{"metadata": map[string]any{"name": "src1"}, "spec": map[string]any{"repoURL": "https://git.example/repo.git"}},
			want: "https://git.example/repo.git",
		},
		{
			name: "oci source shows the bare OCI ref",
			item: map[string]any{"metadata": map[string]any{"name": "src2"}, "spec": map[string]any{"ociRef": "ghcr.io/varroaci/catalog:v1"}},
			want: "ghcr.io/varroaci/catalog:v1",
		},
		{
			name: "update-center source shows its name",
			item: map[string]any{"metadata": map[string]any{"name": "varroa-update-center"}, "spec": map[string]any{}},
			want: "update-center",
		},
		{
			name: "empty source shows a dash",
			item: map[string]any{"metadata": map[string]any{"name": "foo"}},
			want: "-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols := catalogsourceColumns(tt.item)
			if len(cols) < 3 {
				t.Fatalf("expected at least 3 columns, got %d", len(cols))
			}
			if cols[2] != tt.want {
				t.Errorf("SOURCE column: want %q, got %q", tt.want, cols[2])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Catalog tests (6.4)
// ---------------------------------------------------------------------------

func TestCatalog_SyncSlashPath(t *testing.T) {
	testSetup(t)
	var (
		method string
		path   string
	)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"status":"accepted"}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"sync", "catalogsource", "ns1/src1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/catalogsources/ns1/src1/sync") {
		t.Errorf("expected suffix /api/v1/clusters/core/catalogsources/ns1/src1/sync, got %s", path)
	}
}

func TestCatalog_ItemsQueryParams(t *testing.T) {
	testSetup(t)
	var query string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "catalogitems", "-n", "testns", "--source", "src1", "--type", "plugin", "--query", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get catalogitems failed: %v", err)
	}
	if !strings.Contains(query, "namespace=testns") {
		t.Errorf("expected namespace=testns in query, got %s", query)
	}
	if !strings.Contains(query, "source=src1") {
		t.Errorf("expected source=src1 in query, got %s", query)
	}
	if !strings.Contains(query, "type=plugin") {
		t.Errorf("expected type=plugin in query, got %s", query)
	}
	if !strings.Contains(query, "q=hello") {
		t.Errorf("expected q=hello in query, got %s", query)
	}
}

func TestCatalog_ItemsJSONEnvelope(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"metadata":{"name":"item1","namespace":"ns1"}}],"operatorNamespace":"operators-ns"}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "catalogitems", "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get catalogitems -o json failed: %v", err)
	}
}

func TestCatalog_ItemsSingleGet(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"item1","namespace":"ns1"},"spec":{"type":"plugin","sourceRef":"src1","displayName":"Item 1"}}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "catalogitems", "ns1/item1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get catalogitems single failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/catalogitems/ns1/item1") {
		t.Errorf("expected suffix /api/v1/clusters/core/catalogitems/ns1/item1, got %s", path)
	}
}
