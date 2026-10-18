package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// directoryConventionFiles is a catalog with no catalog.yaml. The index comes
// from the directory convention.
var directoryConventionFiles = map[string]string{
	"items/a.yaml":         "a",
	"items/b.yaml":         "b",
	"pod-templates/p.yaml": "p",
	"README.txt":           "not indexed",
}

func requireIndexTypes(t *testing.T, index *CatalogIndex, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for _, item := range index.Items {
		got[item.Type]++
	}
	total := 0
	for typ, n := range want {
		total += n
		if got[typ] != n {
			t.Errorf("type %q: got %d items, want %d (index %+v)", typ, got[typ], n, index.Items)
		}
	}
	if len(index.Items) != total {
		t.Errorf("got %d items, want %d", len(index.Items), total)
	}
}

func TestFetchCatalog_GitDirectoryConvention(t *testing.T) {
	bareURL, _ := newBareFixture(t)
	head := commitWorkTree(t, bareURL, func(work string) {
		for name, content := range directoryConventionFiles {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755))
			must(t, os.WriteFile(filepath.Join(work, name), []byte(content), 0o644))
		}
	})

	dir := t.TempDir()
	rev, err := FetchCatalog(context.Background(), CatalogFetch{
		RepoURL: bareURL,
		Cloner:  newLocalCloner(),
	}, dir)
	must(t, err)
	if rev != head {
		t.Errorf("rev = %q, want HEAD %q", rev, head)
	}
	if _, err := os.Stat(filepath.Join(dir, "bundle.yaml")); !os.IsNotExist(err) {
		t.Errorf("bundle.yaml must not be required or created, stat err = %v", err)
	}

	root, err := CatalogRoot(dir, "")
	must(t, err)
	index, err := ParseCatalogIndex(root)
	must(t, err)
	requireIndexTypes(t, index, map[string]int{"item": 2, "podtemplate": 1})
}

func TestFetchCatalog_GitCloneCache(t *testing.T) {
	bareURL, _ := newBareFixture(t)
	head := commitWorkTree(t, bareURL, func(work string) {
		must(t, os.MkdirAll(filepath.Join(work, "items"), 0o755))
		must(t, os.WriteFile(filepath.Join(work, "items", "a.yaml"), []byte("a"), 0o644))
		must(t, os.WriteFile(filepath.Join(work, "items", "b.yaml"), []byte("b"), 0o644))
		must(t, os.MkdirAll(filepath.Join(work, "pod-templates"), 0o755))
		must(t, os.WriteFile(filepath.Join(work, "pod-templates", "p.yaml"), []byte("p"), 0o644))
	})

	cache := newCacheForTest(t, t.TempDir(), 10, 1024)
	dir := t.TempDir()
	rev, err := FetchCatalog(context.Background(), CatalogFetch{
		RepoURL:    bareURL,
		CloneCache: cache,
	}, dir)
	must(t, err)
	if rev != head {
		t.Errorf("rev = %q, want HEAD %q", rev, head)
	}

	root, err := CatalogRoot(dir, "")
	must(t, err)
	index, err := ParseCatalogIndex(root)
	must(t, err)
	requireIndexTypes(t, index, map[string]int{"item": 2, "podtemplate": 1})
}

func TestFetchCatalog_NoSource(t *testing.T) {
	_, err := FetchCatalog(context.Background(), CatalogFetch{}, t.TempDir())
	if err == nil {
		t.Fatal("expected error for a fetch with no repoURL or ociRef")
	}
}

// tarGzFiles returns a gzipped tar stream that holds files.
func tarGzFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		must(t, err)
	}
	must(t, tw.Close())
	must(t, gw.Close())
	return buf.Bytes()
}

type testLayer struct {
	mediaType string
	data      []byte
}

// pushTestArtifact pushes an OCI artifact with the given layers under ref.
func pushTestArtifact(t *testing.T, store oci.BlobStore, ref string, layers ...testLayer) {
	t.Helper()
	ctx := context.Background()
	descs := make([]oci.Descriptor, 0, len(layers))
	for _, l := range layers {
		digest, size, err := store.PushBlob(ctx, l.mediaType, bytes.NewReader(l.data))
		must(t, err)
		descs = append(descs, oci.Descriptor{MediaType: l.mediaType, Digest: digest, Size: size})
	}
	cfgDigest, _, err := store.PushBlob(ctx, "application/vnd.varroa.bundle.config.v1+json", strings.NewReader("{}"))
	must(t, err)
	must(t, store.Push(ctx, ref, oci.Manifest{
		ArtifactType: CatalogArtifactType,
		Config: oci.Descriptor{
			MediaType: "application/vnd.varroa.bundle.config.v1+json",
			Digest:    cfgDigest,
		},
		Layers: descs,
	}))
}

func TestFetchCatalogLayer_LayoutStore(t *testing.T) {
	ctx := context.Background()
	store, err := oci.NewLayoutStore(t.TempDir())
	must(t, err)
	const ref = "catalog-src"
	pushTestArtifact(t, store, ref, testLayer{
		mediaType: CatalogLayerMediaType,
		data: tarGzFiles(t, map[string]string{
			"items/a.yaml":         "a",
			"items/b.yaml":         "b",
			"pod-templates/p.yaml": "p",
		}),
	})

	dir := t.TempDir()
	digest, err := FetchCatalogLayer(ctx, store, ref, dir)
	must(t, err)
	want, err := store.Resolve(ctx, ref)
	must(t, err)
	if digest != want.Digest {
		t.Errorf("digest = %q, want %q", digest, want.Digest)
	}

	root, err := CatalogRoot(dir, "")
	must(t, err)
	index, err := ParseCatalogIndex(root)
	must(t, err)
	requireIndexTypes(t, index, map[string]int{"item": 2, "podtemplate": 1})
}

func TestFetchCatalogLayer_SelectsCatalogLayer(t *testing.T) {
	ctx := context.Background()
	store, err := oci.NewLayoutStore(t.TempDir())
	must(t, err)
	const ref = "multi-layer"
	pushTestArtifact(t, store, ref,
		testLayer{mediaType: "application/octet-stream", data: []byte("not a tar")},
		testLayer{mediaType: CatalogLayerMediaType, data: tarGzFiles(t, map[string]string{
			"items/a.yaml": "a",
		})},
	)

	dir := t.TempDir()
	_, err = FetchCatalogLayer(ctx, store, ref, dir)
	must(t, err)
	index, err := ParseCatalogIndex(dir)
	must(t, err)
	requireIndexTypes(t, index, map[string]int{"item": 1})
}

func TestFetchCatalogLayer_NoCatalogLayer(t *testing.T) {
	ctx := context.Background()
	store, err := oci.NewLayoutStore(t.TempDir())
	must(t, err)
	const ref = "no-catalog-layer"
	pushTestArtifact(t, store, ref, testLayer{mediaType: "application/octet-stream", data: []byte("x")})

	_, err = FetchCatalogLayer(ctx, store, ref, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no layer with media type") {
		t.Fatalf("err = %v, want a missing-layer error", err)
	}
}

func TestCatalogRoot(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))

	got, err := CatalogRoot(dir, "")
	must(t, err)
	if got != dir {
		t.Errorf("empty path: got %q, want %q", got, dir)
	}

	got, err = CatalogRoot(dir, "sub")
	must(t, err)
	if filepath.Base(got) != "sub" {
		t.Errorf("sub path: got %q, want it to end in sub", got)
	}

	_, err = CatalogRoot(dir, "../x")
	if err == nil || !strings.Contains(err.Error(), "catalog path") {
		t.Fatalf("escaping path: err = %v, want a catalog path error", err)
	}
}
