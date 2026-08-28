package ucmeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// richMetaServer serves a full update-center document, including requiredCore and
// the dependency array that metaServer omits.
func richMetaServer(t *testing.T, plugins map[string]ucPlugin) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ucMetadata{Plugins: plugins})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadServer is a source that always fails, so the resolver marks it unhealthy.
func deadServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveExact_AllHealthyHit(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "472.vf7c289a_4b_420", SHA256: "sha-472"}})
	b := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "470.vb_9a_e8b_5b_58b_2", SHA256: "sha-470"}})

	r := NewResolver(staticSources(a.URL, b.URL), time.Hour, nil, nil)
	got := r.ResolveExact(context.Background(), "mailer", "470.vb_9a_e8b_5b_58b_2")
	if got.Outcome != Resolved {
		t.Fatalf("outcome = %v, want Resolved", got.Outcome)
	}
	if got.Meta.SHA256 != "sha-470" {
		t.Errorf("sha = %q, want sha-470", got.Meta.SHA256)
	}
	// Best is the newest listed anywhere, not the one that matched.
	if got.Best == nil || got.Best.Version != "472.vf7c289a_4b_420" {
		t.Errorf("Best = %+v, want 472.vf7c289a_4b_420", got.Best)
	}
}

func TestResolveExact_AllHealthyMiss_NotListed(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "472", SHA256: "s"}})
	r := NewResolver(staticSources(a.URL), time.Hour, nil, nil)

	got := r.ResolveExact(context.Background(), "mailer", "1.0")
	if got.Outcome != NotListed {
		t.Fatalf("outcome = %v, want NotListed", got.Outcome)
	}
	// A non-satisfying hit still populates Best, which is what makes the
	// rejection diff say "upstream's newest is X".
	if got.Best == nil || got.Best.Version != "472" {
		t.Errorf("Best = %+v, want 472", got.Best)
	}

	// A name nobody lists at all leaves Best nil.
	if none := r.ResolveExact(context.Background(), "nobody", "1.0"); none.Best != nil {
		t.Errorf("Best = %+v, want nil for an unlisted name", none.Best)
	}
}

func TestResolveExact_OneSourceDownAndMiss_SourcesDegraded(t *testing.T) {
	up := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "472", SHA256: "s"}})
	down := deadServer(t)

	r := NewResolver(staticSources(up.URL, down.URL), time.Hour, nil, nil)
	got := r.ResolveExact(context.Background(), "mailer", "1.0")
	if got.Outcome != SourcesDegraded {
		t.Fatalf("outcome = %v, want SourcesDegraded", got.Outcome)
	}
}

func TestResolve_OneSourceDownButAnotherHits_Resolved(t *testing.T) {
	up := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "472", SHA256: "s"}})
	down := deadServer(t)

	r := NewResolver(staticSources(down.URL, up.URL), time.Hour, nil, nil)
	if got := r.ResolveExact(context.Background(), "mailer", "472"); got.Outcome != Resolved {
		t.Fatalf("ResolveExact outcome = %v, want Resolved (resolved beats degraded)", got.Outcome)
	}
	if got := r.ResolveSatisfying(context.Background(), "mailer", "400"); got.Outcome != Resolved {
		t.Fatalf("ResolveSatisfying outcome = %v, want Resolved", got.Outcome)
	}
}

func TestResolveSatisfying_PicksHighestSatisfying(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{"some-lib": {Version: "1.9", SHA256: "sha-19"}})
	b := richMetaServer(t, map[string]ucPlugin{"some-lib": {Version: "1.10", SHA256: "sha-110"}})
	c := richMetaServer(t, map[string]ucPlugin{"some-lib": {Version: "1.1", SHA256: "sha-11"}})

	r := NewResolver(staticSources(a.URL, b.URL, c.URL), time.Hour, nil, nil)
	got := r.ResolveSatisfying(context.Background(), "some-lib", "1.2")
	if got.Outcome != Resolved {
		t.Fatalf("outcome = %v, want Resolved", got.Outcome)
	}
	// 1.10 > 1.9 numerically; a lexical comparator would pick 1.9.
	if got.Meta.Version != "1.10" {
		t.Errorf("version = %q, want 1.10", got.Meta.Version)
	}
}

func TestResolveSatisfying_NoneSatisfy_NotListedWithBest(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{"old-thing": {Version: "3.1", SHA256: "s"}})
	r := NewResolver(staticSources(a.URL), time.Hour, nil, nil)

	got := r.ResolveSatisfying(context.Background(), "old-thing", "9.0")
	if got.Outcome != NotListed {
		t.Fatalf("outcome = %v, want NotListed", got.Outcome)
	}
	if got.Best == nil || got.Best.Version != "3.1" {
		t.Fatalf("Best = %+v, want 3.1", got.Best)
	}
}

func TestFetch_ParsesDependencyArrayAndRequiredCore(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{
		"varroa-mcp-tools": {
			Version:      "1.0.0",
			SHA256:       "s",
			RequiredCore: "2.492",
			Dependencies: []ucDep{
				{Name: "workflow-api", Version: "1384.vdc05a_48f535f"},
				{Name: "junit", Version: "1.0", Optional: true},
				{Name: "", Version: "9"}, // nameless entries are dropped
			},
		},
	})

	r := NewResolver(staticSources(a.URL), time.Hour, nil, nil)
	got := r.ResolveExact(context.Background(), "varroa-mcp-tools", "1.0.0")
	if got.Outcome != Resolved {
		t.Fatalf("outcome = %v, want Resolved", got.Outcome)
	}
	if got.Meta.RequiredCore != "2.492" {
		t.Errorf("requiredCore = %q, want 2.492", got.Meta.RequiredCore)
	}
	want := []Dep{
		{Name: "workflow-api", Version: "1384.vdc05a_48f535f"},
		{Name: "junit", Version: "1.0", Optional: true},
	}
	if len(got.Meta.Dependencies) != len(want) {
		t.Fatalf("dependencies = %+v, want %+v", got.Meta.Dependencies, want)
	}
	for i := range want {
		if got.Meta.Dependencies[i] != want[i] {
			t.Errorf("dependency[%d] = %+v, want %+v", i, got.Meta.Dependencies[i], want[i])
		}
	}
}

// TestResolveSHA256_Unchanged guards the §2.2 promise that the exact-match
// resolver's signature and behaviour did not move.
func TestResolveSHA256_Unchanged(t *testing.T) {
	a := richMetaServer(t, map[string]ucPlugin{"mailer": {Version: "472", SHA256: "sha-472"}})
	r := NewResolver(staticSources(a.URL), time.Hour, nil, nil)

	if sha, err := r.ResolveSHA256(context.Background(), "mailer", "472"); err != nil || sha != "sha-472" {
		t.Fatalf("got (%q,%v), want (sha-472,nil)", sha, err)
	}
	if _, err := r.ResolveSHA256(context.Background(), "mailer", "1.0"); err != ErrVersionUnavailable {
		t.Fatalf("err = %v, want ErrVersionUnavailable", err)
	}
}
