package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestPostServability_ChunksAndPreservesOrder covers a request over the
// per-request limit: it is split into two POSTs, and the answers come back in
// request order.
func TestPostServability_ChunksAndPreservesOrder(t *testing.T) {
	const total = servabilityChunk + 1
	plugins := make([]ucServabilityPlugin, total)
	for i := range plugins {
		plugins[i] = ucServabilityPlugin{Name: fmt.Sprintf("p%04d", i), Version: "1.0"}
	}

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var req ucServabilityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := ucServabilityResponse{}
		for _, p := range req.Plugins {
			resp.Plugins = append(resp.Plugins, ucServabilityResult{Name: p.Name, Version: p.Version, Servable: true})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	got, err := postServability(context.Background(), http.DefaultClient, srv.URL, plugins)
	if err != nil {
		t.Fatalf("postServability: %v", err)
	}
	if n := posts.Load(); n != 2 {
		t.Fatalf("expected 2 POSTs for %d plugins, got %d", total, n)
	}
	if len(got) != total {
		t.Fatalf("expected %d answers, got %d", total, len(got))
	}
	for i := range got {
		if got[i].Name != plugins[i].Name {
			t.Fatalf("answer %d is %q, want %q (order not preserved)", i, got[i].Name, plugins[i].Name)
		}
	}
}

// TestPostServability_UnsupportedRoute covers an update center that predates
// the endpoint: a 404 or 405 answer maps to errServabilityUnsupported.
func TestPostServability_UnsupportedRoute(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		_, err := postServability(context.Background(), http.DefaultClient, srv.URL, []ucServabilityPlugin{{Name: "git", Version: "5.6.0"}})
		srv.Close()
		if !errors.Is(err, errServabilityUnsupported) {
			t.Fatalf("HTTP %d: expected errServabilityUnsupported, got %v", status, err)
		}
	}
}

// TestPostServability_CountMismatchErrors covers an answer that does not match
// the request: the call fails and is not reported as an unsupported route.
func TestPostServability_CountMismatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ucServabilityResponse{})
	}))
	defer srv.Close()

	_, err := postServability(context.Background(), http.DefaultClient, srv.URL, []ucServabilityPlugin{{Name: "git", Version: "5.6.0"}})
	if err == nil {
		t.Fatal("expected an error for an answer count mismatch")
	}
	if errors.Is(err, errServabilityUnsupported) {
		t.Fatalf("a count mismatch must not read as an unsupported route: %v", err)
	}
}

// TestClassifyServability covers every answer code the update center sends.
func TestClassifyServability(t *testing.T) {
	cases := []struct {
		name       string
		res        ucServabilityResult
		wantReason string
		wantOpen   bool
	}{
		{"servable", ucServabilityResult{Servable: true, Via: "metadata"}, "", false},
		{"pull-through disabled", ucServabilityResult{Reason: "pull-through-disabled"}, unservablePullThroughDisabled, false},
		{"not listed", ucServabilityResult{Reason: "not-listed"}, unservableNotListed, false},
		{"archive disabled", ucServabilityResult{Reason: "archive-disabled"}, unservableArchiveDisabled, false},
		{"sources degraded", ucServabilityResult{Reason: "sources-degraded"}, "", true},
		{"unknown code", ucServabilityResult{Reason: "something-new"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, undecided := classifyServability(tc.res)
			if reason != tc.wantReason || undecided != tc.wantOpen {
				t.Fatalf("classifyServability = (%q, %v), want (%q, %v)", reason, undecided, tc.wantReason, tc.wantOpen)
			}
		})
	}
}
