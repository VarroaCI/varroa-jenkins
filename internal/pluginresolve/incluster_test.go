package pluginresolve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// fakeBlobStore is a minimal in-memory oci.BlobStore for InClusterSource
// tests. It stores whatever oci.BuildPluginPack pushes to it, computing a
// real content-derived manifest digest so a tie-break test's outcome does not
// depend on push order. A test may inject a Pull or FetchBlob failure for one
// ref to simulate an unreadable manifest.
type fakeBlobStore struct {
	manifests map[string]oci.Manifest
	digests   map[string]string // ref -> manifest digest
	blobs     map[string][]byte
	order     []string

	failPull  map[string]error // ref -> forced Pull error
	failFetch map[string]error // blob digest -> forced FetchBlob error
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{
		manifests: map[string]oci.Manifest{},
		digests:   map[string]string{},
		blobs:     map[string][]byte{},
		failPull:  map[string]error{},
		failFetch: map[string]error{},
	}
}

func (s *fakeBlobStore) Push(_ context.Context, ref string, manifest oci.Manifest) error {
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	s.manifests[ref] = manifest
	s.digests[ref] = "sha256:" + hex.EncodeToString(sum[:])
	s.order = append(s.order, ref)
	return nil
}

func (s *fakeBlobStore) PushBlob(_ context.Context, _ string, content io.Reader) (string, int64, error) {
	b, err := io.ReadAll(content)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(b)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	s.blobs[digest] = b
	return digest, int64(len(b)), nil
}

func (s *fakeBlobStore) Pull(_ context.Context, ref string) (oci.Manifest, error) {
	if err, ok := s.failPull[ref]; ok {
		return oci.Manifest{}, err
	}
	m, ok := s.manifests[ref]
	if !ok {
		return oci.Manifest{}, fmt.Errorf("fakeBlobStore: no manifest for ref %q", ref)
	}
	return m, nil
}

func (s *fakeBlobStore) Resolve(_ context.Context, ref string) (oci.Descriptor, error) {
	d, ok := s.digests[ref]
	if !ok {
		return oci.Descriptor{}, fmt.Errorf("fakeBlobStore: no digest for ref %q", ref)
	}
	return oci.Descriptor{Digest: d}, nil
}

func (s *fakeBlobStore) FetchBlob(_ context.Context, digest string) (io.ReadCloser, error) {
	if err, ok := s.failFetch[digest]; ok {
		return nil, err
	}
	b, ok := s.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("fakeBlobStore: no blob for digest %q", digest)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *fakeBlobStore) ListManifests(_ context.Context) ([]oci.Descriptor, error) {
	descs := make([]oci.Descriptor, len(s.order))
	for i, ref := range s.order {
		descs[i] = oci.Descriptor{
			Digest:      s.digests[ref],
			Annotations: map[string]string{"org.opencontainers.image.ref.name": ref},
		}
	}
	return descs, nil
}

// pushFixturePack pushes a one-or-more-plugin profile pack under ref.
func pushFixturePack(t *testing.T, store *fakeBlobStore, ref string, plugins []oci.ResolvedPlugin) {
	t.Helper()
	cfg := oci.PackConfig{Kind: oci.PackKindProfile, Profile: ref, PluginCount: len(plugins)}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack(%s): %v", ref, err)
	}
}

// fixturePlugin builds a ResolvedPlugin whose SHA256 is the real digest of
// content, so oci.BuildPluginPack's digest-match check passes.
func fixturePlugin(name, version, requiredCore string, deps []hpi.Dependency, content string) oci.ResolvedPlugin {
	b := []byte(content)
	sum := sha256.Sum256(b)
	return oci.ResolvedPlugin{
		Name:         name,
		Version:      version,
		SHA256:       "sha256:" + hex.EncodeToString(sum[:]),
		RequiredCore: requiredCore,
		Dependencies: deps,
		Content:      bytes.NewReader(b),
	}
}

func TestInClusterSource_ResolvesNewestSatisfying(t *testing.T) {
	store := newFakeBlobStore()
	pushFixturePack(t, store, "profile-a", []oci.ResolvedPlugin{
		fixturePlugin("mailer", "1.0", "2.400", []hpi.Dependency{{Name: "jakarta-mail-api", Min: "2.1"}}, "mailer-1.0"),
	})

	src := InClusterSource{Store: store}
	res := src.Resolve(context.Background(), "mailer", "0.5")
	if res.Outcome != ucmeta.Resolved {
		t.Fatalf("Outcome = %v, want Resolved", res.Outcome)
	}
	if res.Meta.Version != "1.0" || res.Meta.RequiredCore != "2.400" {
		t.Errorf("Meta = %+v, want Version 1.0, RequiredCore 2.400", res.Meta)
	}
	if len(res.Meta.Dependencies) != 1 || res.Meta.Dependencies[0].Name != "jakarta-mail-api" || res.Meta.Dependencies[0].Version != "2.1" {
		t.Errorf("Meta.Dependencies = %+v", res.Meta.Dependencies)
	}
}

func TestInClusterSource_CompletenessTieBreak(t *testing.T) {
	// Same version, same SHA256-irrelevant distinct content: "a" carries no
	// derived metadata (as if ApplyHPIMetadata failed at pack-build time),
	// "b" carries both RequiredCore and Dependencies. "b" must win regardless
	// of which pack is listed first.
	build := func(firstRef, secondRef string) *ucmeta.Resolution {
		store := newFakeBlobStore()
		pushFixturePack(t, store, firstRef, []oci.ResolvedPlugin{
			fixturePlugin("foo", "1.0", "", nil, firstRef+"-content"),
		})
		pushFixturePack(t, store, secondRef, []oci.ResolvedPlugin{
			fixturePlugin("foo", "1.0", "2.400", []hpi.Dependency{{Name: "bar", Min: "1.0"}}, secondRef+"-content"),
		})
		res := (InClusterSource{Store: store}).Resolve(context.Background(), "foo", "0")
		return &res
	}

	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		res := build(order[0], order[1])
		if res.Outcome != ucmeta.Resolved {
			t.Fatalf("order %v: Outcome = %v, want Resolved", order, res.Outcome)
		}
		if res.Meta.RequiredCore != "2.400" {
			t.Errorf("order %v: Meta.RequiredCore = %q, want the more complete candidate's 2.400", order, res.Meta.RequiredCore)
		}
	}
}

func TestInClusterSource_IdenticalTieIsOrderIndependent(t *testing.T) {
	// Two packs offer byte-identical plugin content — same SHA256, same
	// completeness — so only the manifest-digest tie-break distinguishes
	// them. The winning Meta must be identical regardless of which pack
	// ListManifests returns first.
	plugin := func() oci.ResolvedPlugin {
		return fixturePlugin("foo", "1.0", "2.400", []hpi.Dependency{{Name: "bar", Min: "1.0"}}, "shared-content")
	}

	forward := newFakeBlobStore()
	pushFixturePack(t, forward, "a", []oci.ResolvedPlugin{plugin()})
	pushFixturePack(t, forward, "b", []oci.ResolvedPlugin{plugin()})

	backward := newFakeBlobStore()
	pushFixturePack(t, backward, "b", []oci.ResolvedPlugin{plugin()})
	pushFixturePack(t, backward, "a", []oci.ResolvedPlugin{plugin()})

	resForward := (InClusterSource{Store: forward}).Resolve(context.Background(), "foo", "0")
	resBackward := (InClusterSource{Store: backward}).Resolve(context.Background(), "foo", "0")

	if resForward.Outcome != ucmeta.Resolved || resBackward.Outcome != ucmeta.Resolved {
		t.Fatalf("Outcomes = %v / %v, want both Resolved", resForward.Outcome, resBackward.Outcome)
	}
	if fmt.Sprintf("%+v", resForward.Meta) != fmt.Sprintf("%+v", resBackward.Meta) {
		t.Errorf("winner depends on ListManifests order: forward %+v, backward %+v", resForward.Meta, resBackward.Meta)
	}
}

func TestInClusterSource_UnreadableManifestStillResolves(t *testing.T) {
	t.Run("pull failure", func(t *testing.T) {
		store := newFakeBlobStore()
		pushFixturePack(t, store, "good", []oci.ResolvedPlugin{fixturePlugin("foo", "1.0", "", nil, "good-content")})
		pushFixturePack(t, store, "bad", []oci.ResolvedPlugin{fixturePlugin("other", "1.0", "", nil, "bad-content")})
		store.failPull["bad"] = errors.New("simulated pull failure")

		res := (InClusterSource{Store: store}).Resolve(context.Background(), "foo", "0")
		if res.Outcome != ucmeta.Resolved {
			t.Fatalf("Outcome = %v, want Resolved despite an unrelated manifest's Pull failure", res.Outcome)
		}
	})

	t.Run("read pack failure", func(t *testing.T) {
		store := newFakeBlobStore()
		pushFixturePack(t, store, "good", []oci.ResolvedPlugin{fixturePlugin("foo", "1.0", "", nil, "good-content")})
		pushFixturePack(t, store, "bad", []oci.ResolvedPlugin{fixturePlugin("other", "1.0", "", nil, "bad-content")})
		store.failFetch[store.manifests["bad"].Config.Digest] = errors.New("simulated config read failure")

		res := (InClusterSource{Store: store}).Resolve(context.Background(), "foo", "0")
		if res.Outcome != ucmeta.Resolved {
			t.Fatalf("Outcome = %v, want Resolved despite an unrelated manifest's ReadPluginPack failure", res.Outcome)
		}
	})
}

func TestInClusterSource_AllManifestsUnreadableDegrades(t *testing.T) {
	store := newFakeBlobStore()
	pushFixturePack(t, store, "a", []oci.ResolvedPlugin{fixturePlugin("foo", "1.0", "", nil, "a-content")})
	pushFixturePack(t, store, "b", []oci.ResolvedPlugin{fixturePlugin("foo", "2.0", "", nil, "b-content")})
	store.failPull["a"] = errors.New("simulated pull failure")
	store.failPull["b"] = errors.New("simulated pull failure")

	res := (InClusterSource{Store: store}).Resolve(context.Background(), "foo", "0")
	if res.Outcome != ucmeta.SourcesDegraded {
		t.Fatalf("Outcome = %v, want SourcesDegraded when every manifest is unreadable", res.Outcome)
	}
}
