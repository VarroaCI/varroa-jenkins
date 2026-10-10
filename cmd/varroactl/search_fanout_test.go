package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSearch_WarnsOnFailedCluster(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"clusters":[{"name":"member-b","ok":false,"error":"cluster unreachable"}]}`)
	})
	defer srv.Close()

	var stderr strings.Builder
	root := newRootCmd()
	root.SetErr(&stderr)
	root.SetArgs([]string{"search", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: cluster member-b: cluster unreachable") {
		t.Errorf("stderr = %q, want the member-b warning", stderr.String())
	}
}

func TestSearch_NoWarningWhenClustersOK(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"clusters":[{"name":"core","ok":true}]}`)
	})
	defer srv.Close()

	var stderr strings.Builder
	root := newRootCmd()
	root.SetErr(&stderr)
	root.SetArgs([]string{"search", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
