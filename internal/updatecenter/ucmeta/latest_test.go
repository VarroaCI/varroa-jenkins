package ucmeta

import (
	"context"
	"testing"
)

func TestResolveAvailable_LatestResolvesToMetadataVersion(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	a := r.ResolveAvailable(context.Background(), testPlugin, "latest")
	if a.Outcome != Resolved || a.Via != ViaMetadata {
		t.Fatalf("got outcome %v via %q, want resolved via metadata", a.Outcome, a.Via)
	}
	if a.Version != "4360.vpinned" {
		t.Errorf("Version = %q, want 4360.vpinned", a.Version)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0: a latest request never consults the archive", *hits)
	}
}

func TestResolveAvailable_LatestCaseInsensitiveAndEmpty(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, _ := archiveServer(t, nil)
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	for _, v := range []string{"LATEST", "Latest", ""} {
		a := r.ResolveAvailable(context.Background(), testPlugin, v)
		if a.Outcome != Resolved || a.Version != "4360.vpinned" {
			t.Errorf("version %q: got outcome %v version %q, want resolved 4360.vpinned", v, a.Outcome, a.Version)
		}
	}
}

func TestResolveAvailable_LatestPicksHighestAcrossSources(t *testing.T) {
	older := gavMetaServer(t, "4360.vpinned", testGAV)
	newer := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, nil)
	r := NewResolver(staticSources(older.URL, newer.URL), 0, nil, nil)
	r.SetArchiveBaseURL(arch.URL)

	a := r.ResolveAvailable(context.Background(), testPlugin, "latest")
	if a.Outcome != Resolved || a.Version != "4362.vnewer" {
		t.Fatalf("got outcome %v version %q, want resolved 4362.vnewer", a.Outcome, a.Version)
	}
}

func TestResolveAvailable_LatestUnlisted_NotListed(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	a := r.ResolveAvailable(context.Background(), "no-such-plugin", "latest")
	if a.Outcome != NotListed {
		t.Fatalf("got outcome %v, want not-listed", a.Outcome)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0", *hits)
	}
}

func TestResolveAvailable_LatestDegraded(t *testing.T) {
	arch, hits := archiveServer(t, nil)
	r := newArchiveTestResolver(t, unreachableSource, arch.URL)

	a := r.ResolveAvailable(context.Background(), testPlugin, "latest")
	if a.Outcome != SourcesDegraded {
		t.Fatalf("got outcome %v, want sources-degraded", a.Outcome)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0", *hits)
	}
}

func TestResolveAvailable_ConcreteVersionEchoed(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, _ := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	if a := r.ResolveAvailable(context.Background(), testPlugin, "4360.vpinned"); a.Version != "4360.vpinned" {
		t.Errorf("metadata hit Version = %q, want 4360.vpinned", a.Version)
	}
	aged := gavMetaServer(t, "4362.vnewer", testGAV)
	ar := newArchiveTestResolver(t, aged.URL, arch.URL)
	if a := ar.ResolveAvailable(context.Background(), testPlugin, "4360.vpinned"); a.Via != ViaArchive || a.Version != "4360.vpinned" {
		t.Errorf("archive hit: via %q version %q, want archive 4360.vpinned", a.Via, a.Version)
	}
}
