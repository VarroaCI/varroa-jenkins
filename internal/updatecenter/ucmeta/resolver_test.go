package ucmeta

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// metaServer serves an update-center.actual.json for the given plugins and counts hits.
func metaServer(t *testing.T, plugins map[string]ucPlugin) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		body := `{"plugins":{`
		first := true
		for name, p := range plugins {
			if !first {
				body += ","
			}
			first = false
			body += fmt.Sprintf(`%q:{"version":%q,"sha256":%q}`, name, p.Version, p.SHA256)
		}
		body += `}}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func staticSources(urls ...string) func() []Source {
	return func() []Source {
		out := make([]Source, len(urls))
		for i, u := range urls {
			out[i] = Source{URL: u}
		}
		return out
	}
}

func TestResolve_NameVersionIndexing_CoexistAcrossSources(t *testing.T) {
	weekly, _ := metaServer(t, map[string]ucPlugin{"role-strategy": {Version: "878", SHA256: "sha-878"}})
	lts, _ := metaServer(t, map[string]ucPlugin{"role-strategy": {Version: "867", SHA256: "sha-867"}})

	r := NewResolver(staticSources(weekly.URL, lts.URL), time.Hour, nil, nil)
	ctx := context.Background()

	if sha, err := r.ResolveSHA256(ctx, "role-strategy", "878"); err != nil || sha != "sha-878" {
		t.Fatalf("weekly version: got (%q,%v), want (sha-878,nil)", sha, err)
	}
	if sha, err := r.ResolveSHA256(ctx, "role-strategy", "867"); err != nil || sha != "sha-867" {
		t.Fatalf("LTS version: got (%q,%v), want (sha-867,nil)", sha, err)
	}
}

func TestResolve_WeeklyFirstPrecedence(t *testing.T) {
	// Same (name,version) in both sources but different sha; weekly (first) must win.
	weekly, _ := metaServer(t, map[string]ucPlugin{"git": {Version: "5.0", SHA256: "weekly-sha"}})
	lts, _ := metaServer(t, map[string]ucPlugin{"git": {Version: "5.0", SHA256: "lts-sha"}})

	r := NewResolver(staticSources(weekly.URL, lts.URL), time.Hour, nil, nil)
	if sha, err := r.ResolveSHA256(context.Background(), "git", "5.0"); err != nil || sha != "weekly-sha" {
		t.Fatalf("got (%q,%v), want (weekly-sha,nil)", sha, err)
	}
}

func TestResolve_TTLCacheHitThenRefetch(t *testing.T) {
	srv, hits := metaServer(t, map[string]ucPlugin{"p": {Version: "1", SHA256: "s1"}})
	r := NewResolver(staticSources(srv.URL), time.Hour, nil, nil)

	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }

	ctx := context.Background()
	_, _ = r.ResolveSHA256(ctx, "p", "1")
	_, _ = r.ResolveSHA256(ctx, "p", "1")
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("within TTL: %d fetches, want 1", got)
	}

	// Advance past the TTL -> one more fetch.
	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	_, _ = r.ResolveSHA256(ctx, "p", "1")
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("after TTL: %d fetches, want 2", got)
	}
}

func TestResolve_FailedSourceSkipped_OthersResolve(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	good, _ := metaServer(t, map[string]ucPlugin{"p": {Version: "1", SHA256: "s1"}})

	r := NewResolver(staticSources(dead.URL, good.URL), time.Hour, nil, nil)
	if sha, err := r.ResolveSHA256(context.Background(), "p", "1"); err != nil || sha != "s1" {
		t.Fatalf("got (%q,%v), want (s1,nil) despite a dead source", sha, err)
	}
}

func TestResolve_FailedSourceNotRetriedWithinTTL(t *testing.T) {
	var hits int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	r := NewResolver(staticSources(dead.URL), time.Hour, nil, nil)
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }

	ctx := context.Background()
	_, _ = r.ResolveSHA256(ctx, "p", "1")
	_, _ = r.ResolveSHA256(ctx, "p", "1")
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("failed source retried within TTL: %d fetches, want 1", got)
	}
}

func TestResolve_VersionUnavailable(t *testing.T) {
	srv, _ := metaServer(t, map[string]ucPlugin{"p": {Version: "1", SHA256: "s1"}})
	r := NewResolver(staticSources(srv.URL), time.Hour, nil, nil)

	if _, err := r.ResolveSHA256(context.Background(), "p", "2"); err != ErrVersionUnavailable {
		t.Fatalf("got %v, want ErrVersionUnavailable", err)
	}
	if _, err := r.ResolveSHA256(context.Background(), "absent", "1"); err != ErrVersionUnavailable {
		t.Fatalf("got %v, want ErrVersionUnavailable", err)
	}
}

func TestResolve_Concurrent_SingleFetchPerSource(t *testing.T) {
	srv, hits := metaServer(t, map[string]ucPlugin{"p": {Version: "1", SHA256: "s1"}})
	r := NewResolver(staticSources(srv.URL), time.Hour, nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sha, err := r.ResolveSHA256(context.Background(), "p", "1"); err != nil || sha != "s1" {
				t.Errorf("got (%q,%v)", sha, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("concurrent resolution triggered %d fetches, want 1", got)
	}
}

func TestResolve_SourcesListChangePickedUp(t *testing.T) {
	weekly, _ := metaServer(t, map[string]ucPlugin{"p": {Version: "1", SHA256: "weekly"}})
	lts, _ := metaServer(t, map[string]ucPlugin{"p": {Version: "2", SHA256: "lts"}})

	var mu sync.Mutex
	urls := make([]string, 0, 2)
	urls = append(urls, weekly.URL)
	sources := func() []Source {
		mu.Lock()
		defer mu.Unlock()
		out := make([]Source, len(urls))
		for i, u := range urls {
			out[i] = Source{URL: u}
		}
		return out
	}
	r := NewResolver(sources, time.Hour, nil, nil)
	ctx := context.Background()

	// v2 not resolvable until the LTS source is added.
	if _, err := r.ResolveSHA256(ctx, "p", "2"); err != ErrVersionUnavailable {
		t.Fatalf("before add: got %v, want ErrVersionUnavailable", err)
	}
	mu.Lock()
	urls = append(urls, lts.URL)
	mu.Unlock()
	if sha, err := r.ResolveSHA256(ctx, "p", "2"); err != nil || sha != "lts" {
		t.Fatalf("after add: got (%q,%v), want (lts,nil)", sha, err)
	}

	// Remove the weekly source; v1 no longer resolvable.
	mu.Lock()
	urls = []string{lts.URL}
	mu.Unlock()
	if _, err := r.ResolveSHA256(ctx, "p", "1"); err != ErrVersionUnavailable {
		t.Fatalf("after remove: got %v, want ErrVersionUnavailable", err)
	}
}
