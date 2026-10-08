package ucmeta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// unreachableSource is a metadata URL that always fails to connect.
const unreachableSource = "http://127.0.0.1:1/update-center.actual.json"

func TestResolveAvailable_MetadataHit(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != Resolved || a.Via != ViaMetadata {
		t.Fatalf("got outcome %v via %q, want resolved via metadata", a.Outcome, a.Via)
	}
	if a.Meta == nil || a.Meta.GAV != testGAV {
		t.Fatalf("Meta = %+v, want the metadata entry with its gav", a.Meta)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0", *hits)
	}
}

func TestResolveAvailable_AgedPinViaArchive(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != Resolved || a.Via != ViaArchive {
		t.Fatalf("got outcome %v via %q, want resolved via archive", a.Outcome, a.Via)
	}
	if a.Meta != nil {
		t.Errorf("Meta = %+v, want nil: the archive carries no dependency list", a.Meta)
	}
	if want := testDigestB64(t); a.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", a.SHA256, want)
	}
}

func TestResolveAvailable_ArchiveDisabled_NotListedFlagged(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	r := NewResolver(staticSources(meta.URL), time.Minute, http.DefaultClient, nil)
	r.SetArchiveBaseURL("")

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != NotListed || !a.ArchiveDisabled {
		t.Fatalf("got outcome %v archiveDisabled=%v, want not-listed with ArchiveDisabled", a.Outcome, a.ArchiveDisabled)
	}
}

// A sidecar 404 is a provable negative. A degraded metadata source elsewhere must not
// turn it into an unprovable one.
func TestResolveAvailable_Archive404_NotListedEvenWhenSourceDegraded(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, map[string]string{})
	r := NewResolver(staticSources(unreachableSource, meta.URL), time.Minute, http.DefaultClient, nil)
	r.SetArchiveBaseURL(arch.URL)

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != NotListed {
		t.Fatalf("outcome = %v, want not-listed", a.Outcome)
	}
}

func TestResolveAvailable_Archive5xx_SourcesDegraded(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(arch.Close)
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != SourcesDegraded {
		t.Fatalf("outcome = %v, want sources-degraded", a.Outcome)
	}
}

// Without a gav, the archive cannot be addressed. A healthy-but-silent metadata set is
// a provable negative; a degraded one is not.
func TestResolveAvailable_NoGAV_DegradedVsNotListed(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", "")
	r := newArchiveTestResolver(t, meta.URL, "http://127.0.0.1:1")
	if a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned"); a.Outcome != NotListed {
		t.Fatalf("healthy source, no gav: outcome = %v, want not-listed", a.Outcome)
	}

	r = NewResolver(staticSources(unreachableSource), time.Minute, http.DefaultClient, nil)
	if a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned"); a.Outcome != SourcesDegraded {
		t.Fatalf("degraded source: outcome = %v, want sources-degraded", a.Outcome)
	}
}

// A transient archive failure is remembered for archiveTransientTTL only, so a flaky
// repository recovers within a minute rather than six hours.
func TestArchive_TransientErrorCachedBriefly(t *testing.T) {
	var hits int32
	arch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(arch.Close)
	a := newArchiveResolver(arch.URL, http.DefaultClient)

	ctx := context.Background()
	if _, err := a.resolve(ctx, testGAV, testPlugin, "4360.vpinned"); err == nil {
		t.Fatal("first lookup: want an error")
	}
	if _, err := a.resolve(ctx, testGAV, testPlugin, "4360.vpinned"); err == nil {
		t.Fatal("second lookup: want the cached error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits within the transient window = %d, want 1", got)
	}

	// Age the entry past the transient window: the next lookup must refetch.
	key := verKey{name: testPlugin, version: "4360.vpinned"}
	a.mu.Lock()
	e := a.cache[key]
	e.at = e.at.Add(-archiveTransientTTL - time.Second)
	a.cache[key] = e
	a.mu.Unlock()

	_, _ = a.resolve(ctx, testGAV, testPlugin, "4360.vpinned")
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits after the transient window = %d, want 2", got)
	}
}

// A provable miss keeps the full TTL: a 404 is not retried inside the window.
func TestArchive_NotFoundCachedForArchiveTTL(t *testing.T) {
	e := archiveEntry{err: errArchiveNotFound, at: time.Now()}
	if e.ttl() != archiveTTL {
		t.Fatalf("ttl = %v, want %v", e.ttl(), archiveTTL)
	}
	transient := archiveEntry{err: errors.New("boom"), at: time.Now()}
	if transient.ttl() != archiveTransientTTL {
		t.Fatalf("transient ttl = %v, want %v", transient.ttl(), archiveTransientTTL)
	}
}

// ResolveSHA256 is a thin wrapper over ResolveAvailable: the two agree on every path.
func TestResolveSHA256_DelegatesToResolveAvailable(t *testing.T) {
	cases := []struct {
		name   string
		listed string
		arch   map[string]string
	}{
		{"metadata hit", "4360.vpinned", map[string]string{}},
		{"archive hit", "4362.vnewer", map[string]string{wantSidecarPath: testDigestHex}},
		{"miss", "4362.vnewer", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := gavMetaServer(t, tc.listed, testGAV)
			arch, _ := archiveServer(t, tc.arch)
			r := newArchiveTestResolver(t, meta.URL, arch.URL)
			ctx := context.Background()

			a := r.ResolveAvailable(ctx, "workflow-cps", "4360.vpinned")
			sha, err := r.ResolveSHA256(ctx, "workflow-cps", "4360.vpinned")
			if a.Outcome == Resolved {
				if err != nil || sha != a.SHA256 {
					t.Fatalf("ResolveSHA256 = (%q, %v), want (%q, nil)", sha, err, a.SHA256)
				}
				return
			}
			if !errors.Is(err, ErrVersionUnavailable) {
				t.Fatalf("ResolveSHA256 err = %v, want ErrVersionUnavailable", err)
			}
		})
	}
}

// With the archive disabled, a degraded source could still have listed the pin, so the
// negative is unprovable and must not be reported as archive-disabled.
func TestResolveAvailable_ArchiveDisabled_DegradedSourceIsUnprovable(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	r := NewResolver(staticSources(unreachableSource, meta.URL), time.Minute, http.DefaultClient, nil)
	r.SetArchiveBaseURL("")

	a := r.ResolveAvailable(context.Background(), "workflow-cps", "4360.vpinned")
	if a.Outcome != SourcesDegraded || a.ArchiveDisabled {
		t.Fatalf("got outcome %v archiveDisabled=%v, want sources-degraded without ArchiveDisabled", a.Outcome, a.ArchiveDisabled)
	}
}
