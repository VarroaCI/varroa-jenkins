package ucmeta

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// coreMetaServer serves one metadata document that lists name at version, with the
// given requiredCore. An empty requiredCore is omitted from the document.
func coreMetaServer(t *testing.T, version, requiredCore string) *httptest.Server {
	t.Helper()
	body := fmt.Sprintf(`{"plugins":{"foo":{"version":%q,"sha256":"c2hhLWZha2U=","requiredCore":%q}}}`,
		version, requiredCore)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/update-center.actual.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// coreTestResolver builds a resolver over the given sources, with the archive off.
func coreTestResolver(urls ...string) *Resolver {
	srcs := make([]string, len(urls))
	for i, u := range urls {
		srcs[i] = u + "/update-center.actual.json"
	}
	r := NewResolver(staticSources(srcs...), 0, nil, nil)
	r.SetArchiveBaseURL("")
	return r
}

func TestResolveLatestForCore_SkipsTooNewSource(t *testing.T) {
	weekly := coreMetaServer(t, "2.0", "2.570")
	lts := coreMetaServer(t, "1.9", "2.555.3")
	r := coreTestResolver(weekly.URL, lts.URL)

	a := r.ResolveLatestForCore(context.Background(), "foo", "2.555.3")
	if a.Outcome != Resolved || a.Version != "1.9" {
		t.Fatalf("got outcome %v version %q, want resolved 1.9", a.Outcome, a.Version)
	}
	if a.Via != ViaMetadata || a.Meta == nil || a.Meta.RequiredCore != "2.555.3" {
		t.Errorf("metadata = %+v via %q, want the LTS entry with requiredCore 2.555.3", a.Meta, a.Via)
	}
}

func TestResolveLatestForCore_EmptyCoreTakesHighest(t *testing.T) {
	weekly := coreMetaServer(t, "2.0", "2.570")
	lts := coreMetaServer(t, "1.9", "2.555.3")
	r := coreTestResolver(weekly.URL, lts.URL)

	a := r.ResolveLatestForCore(context.Background(), "foo", "")
	if a.Outcome != Resolved || a.Version != "2.0" {
		t.Fatalf("got outcome %v version %q, want resolved 2.0", a.Outcome, a.Version)
	}
}

func TestResolveLatestForCore_NoneCompatible_NotListed(t *testing.T) {
	weekly := coreMetaServer(t, "2.0", "2.570")
	lts := coreMetaServer(t, "1.9", "2.555.3")
	r := coreTestResolver(weekly.URL, lts.URL)

	a := r.ResolveLatestForCore(context.Background(), "foo", "2.400")
	if a.Outcome != NotListed {
		t.Fatalf("got outcome %v, want not-listed", a.Outcome)
	}
}

func TestResolveLatestForCore_Degraded(t *testing.T) {
	lts := coreMetaServer(t, "1.9", "2.555.3")
	r := coreTestResolver(unreachableSource, lts.URL)

	a := r.ResolveLatestForCore(context.Background(), "foo", "2.400")
	if a.Outcome != SourcesDegraded {
		t.Fatalf("got outcome %v, want sources-degraded", a.Outcome)
	}
}

func TestResolveLatestForCore_EmptyRequiredCoreAccepted(t *testing.T) {
	weekly := coreMetaServer(t, "2.0", "")
	r := coreTestResolver(weekly.URL)

	a := r.ResolveLatestForCore(context.Background(), "foo", "2.400")
	if a.Outcome != Resolved || a.Version != "2.0" {
		t.Fatalf("got outcome %v version %q, want resolved 2.0", a.Outcome, a.Version)
	}
}

func TestCoreCompatible(t *testing.T) {
	cases := []struct {
		name         string
		requiredCore string
		core         string
		want         bool
	}{
		{"empty core accepts any", "2.570", "", true},
		{"empty required accepts any", "", "2.400", true},
		{"equal core runs", "2.555.3", "2.555.3", true},
		{"newer core runs", "2.555.3", "2.570", true},
		{"older core rejected", "2.570", "2.555.3", false},
		{"numeric not lexical", "2.1000", "2.999", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CoreCompatible(c.requiredCore, c.core); got != c.want {
				t.Errorf("CoreCompatible(%q, %q) = %v, want %v", c.requiredCore, c.core, got, c.want)
			}
		})
	}
}
