package plugininv

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// ==========================================================================
// §1.5 Golden-vector tests for Hash()
// ==========================================================================

func TestHash_GoldenVectors(t *testing.T) {
	tests := []struct {
		name string
		inv  Inventory
		want string
	}{
		{
			name: "empty inventory",
			inv:  Inventory{},
			want: "v1:a7557e0d0f57232fbf20c15bbe97af2410293d11fa7265114b654d3fed73224a",
		},
		{
			name: "single plugin, no deps",
			inv: Inventory{Records: []Record{
				{Name: "simple-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			}},
			want: "v1:53593a79c471d1eb0f2e0e0e3bc445e6ade283c3323ddd5a3b33b37c8249a0e8",
		},
		{
			name: "empty version",
			inv: Inventory{Records: []Record{
				{Name: "no-version", Version: "", Enabled: TriFalse, Detached: TriFalse, Bundled: TriFalse},
			}},
			want: "v1:68e8122e10c4966bb95d51e8b476bdf9085e64c8eac5dd432c56ed026f742607",
		},
		{
			name: "non-ASCII name",
			inv: Inventory{Records: []Record{
				{Name: "naïve-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			}},
			want: "v1:7899bc75b133432bef3468a661c49eb663ead97777ebc7ec7a3b53056c9d5487",
		},
		{
			name: "HTML-significant name",
			inv: Inventory{Records: []Record{
				{Name: "plugin<script>alert(1)</script>", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			}},
			want: "v1:80d6807fcbdc14a845f031d5a152a2fe5c2b893050c8e5577be6c9941d7d0c5b",
		},
		{
			name: "all unknown flags",
			inv: Inventory{Records: []Record{
				{Name: "filesystem-plugin", Version: "2.0", Enabled: TriUnknown, Detached: TriUnknown, Bundled: TriUnknown},
			}},
			want: "v1:e981ca7065b7cb1f2ee618144dd86316598a86f86d56736166cecf9c23944d66",
		},
		{
			name: "bundled true, detached false",
			inv: Inventory{Records: []Record{
				{Name: "bundled-plugin", Version: "1.5", Enabled: TriTrue, Detached: TriFalse, Bundled: TriTrue},
			}},
			want: "v1:01d38614a019648b2c3aa024e52a9d80bac8f1a5ada06c2f8b1163cc17ddbe64",
		},
		{
			name: "duplicate plugin name",
			inv: Inventory{Records: []Record{
				{Name: "dup", Version: "2.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
				{Name: "dup", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			}},
			want: "v1:e0c87a379f23904c550f5ae31ffecfd3b2829575ae32cbd6ecbbdb96ba5eee49",
		},
		{
			name: "identical and near-identical dep triples",
			inv: Inventory{Records: []Record{
				{Name: "with-deps", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
					Deps: []Dep{
						{Name: "alpha", Min: "1.0", Optional: false},
						{Name: "alpha", Min: "1.0", Optional: false}, // identical → dropped
						{Name: "alpha", Min: "1.0", Optional: true},  // near-identical
						{Name: "alpha", Min: "2.0", Optional: false}, // near-identical
						{Name: "beta", Min: "1.0", Optional: false},
					},
				},
			}},
			want: "v1:c71d3b626d2c7abdd280958b30b4eb385bdceec2434899ae327733f9ef1d6f90",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.inv.Hash()
			if got != tc.want {
				t.Errorf("Hash() = %s, want %s", got, tc.want)
			}
		})
	}
}

// ==========================================================================
// §1.6 Order-independent hash tests
// ==========================================================================

func TestHash_InputOrderDoesNotChangeHash(t *testing.T) {
	a := Inventory{Records: []Record{
		{Name: "b-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		{Name: "a-plugin", Version: "2.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
	}}
	b := Inventory{Records: []Record{
		{Name: "a-plugin", Version: "2.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		{Name: "b-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
	}}
	if a.Hash() != b.Hash() {
		t.Errorf("hash differs when input order changes")
	}
}

func TestHash_UnknownFlagsHashDifferentlyFromObservedFalse(t *testing.T) {
	// Same plugin set, one from API (observed false), one from FS (unknown).
	api := Inventory{Records: []Record{
		{Name: "p", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
	}}
	fs := Inventory{Records: []Record{
		{Name: "p", Version: "1.0", Enabled: TriUnknown, Detached: TriUnknown, Bundled: TriUnknown},
	}}
	if api.Hash() == fs.Hash() {
		t.Errorf("API and FS hashes of same plugin set must differ")
	}
}

func TestHash_NormalizeIsIdempotent(t *testing.T) {
	inv := Inventory{Records: []Record{
		{Name: "dup", Version: "2.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		{Name: "dup", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		{Name: "a-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
			Deps: []Dep{
				{Name: "z", Min: "1.0", Optional: false},
				{Name: "a", Min: "1.0", Optional: true},
				{Name: "z", Min: "1.0", Optional: false}, // duplicate
			},
		},
	}}
	h1 := inv.Hash()
	inv.Normalize()
	h2 := inv.Hash()
	if h1 != h2 {
		t.Errorf("Normalize not idempotent: %s vs %s", h1, h2)
	}
}

// ==========================================================================
// §1.4 hashRecognised tests
// ==========================================================================

func TestHashRecognised(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"v1:0000000000000000000000000000000000000000000000000000000000000000", true},
		{"v1:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
		{"", false},
		{"v1:", false},
		{"v1:abc", false},
		{"v1:000000000000000000000000000000000000000000000000000000000000000", false},   // 63 chars
		{"v1:00000000000000000000000000000000000000000000000000000000000000000", false}, // 65 chars
		{"V1:0000000000000000000000000000000000000000000000000000000000000000", false},  // uppercase V
		{"v1:000000000000000000000000000000000000000000000000000000000000000G", false},  // non-hex
		{"v2:0000000000000000000000000000000000000000000000000000000000000000", false},  // wrong version
	}
	for _, tc := range tests {
		if got := hashRecognised(tc.s); got != tc.want {
			t.Errorf("hashRecognised(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// ==========================================================================
// §2.6 Collection tests
// ==========================================================================

// stubJenkinsAPI implements jenkinsPluginAPI for tests.
type stubJenkinsAPI struct {
	plugins []jenkins.APIPlugin
	err     error
}

func (s *stubJenkinsAPI) GetPluginManager(ctx context.Context) ([]jenkins.APIPlugin, error) {
	return s.plugins, s.err
}

func TestCollectAPI_Depth2Accepted(t *testing.T) {
	api := &stubJenkinsAPI{plugins: []jenkins.APIPlugin{
		{
			ShortName: "test-plugin",
			Version:   "1.0",
			Enabled:   true,
			Detached:  false,
			Bundled:   false,
			Dependencies: []json.RawMessage{
				json.RawMessage(`{"name":"dep1","version":"1.5","optional":false}`),
				json.RawMessage(`{"name":"dep2","version":"2.0","optional":true}`),
			},
		},
	}}
	inv, err := CollectAPI(context.Background(), api)
	if err != nil {
		t.Fatalf("CollectAPI: %v", err)
	}
	if inv.Source != SourceJenkinsAPI {
		t.Errorf("Source = %q, want %q", inv.Source, SourceJenkinsAPI)
	}
	if len(inv.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(inv.Records))
	}
	r := inv.Records[0]
	if r.Name != "test-plugin" {
		t.Errorf("Name = %q", r.Name)
	}
	if r.Enabled != TriTrue || r.Detached != TriFalse || r.Bundled != TriFalse {
		t.Errorf("flags: enabled=%v detached=%v bundled=%v", r.Enabled, r.Detached, r.Bundled)
	}
	if len(r.Deps) != 2 {
		t.Fatalf("got %d deps, want 2", len(r.Deps))
	}
	if r.Deps[1].Optional != true {
		t.Errorf("dep2 optional = %v, want true", r.Deps[1].Optional)
	}
}

func TestCollectAPI_Depth1Rejected(t *testing.T) {
	api := &stubJenkinsAPI{plugins: []jenkins.APIPlugin{
		{
			ShortName:    "bad-plugin",
			Version:      "1.0",
			Enabled:      true,
			Dependencies: []json.RawMessage{json.RawMessage(`"bare-string-dep"`)},
		},
	}}
	_, err := CollectAPI(context.Background(), api)
	if err == nil {
		t.Fatal("expected error for depth=1 shaped response")
	}
	if !strings.Contains(err.Error(), "depth=1") {
		t.Errorf("error should mention depth=1: %v", err)
	}
}

func TestCollectAPI_Error(t *testing.T) {
	api := &stubJenkinsAPI{err: errors.New("connection refused")}
	_, err := CollectAPI(context.Background(), api)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCollectFS_Parity(t *testing.T) {
	// Build a minimal HPI zip with manifest.
	manifest := "Short-Name: test-plugin\nPlugin-Version: 1.0\nPlugin-Dependencies: dep1:1.5,dep2:2.0;resolution:=optional\n"
	hpiBytes := buildMinimalHPI(t, manifest)

	fsys := fstest.MapFS{
		"test-plugin.hpi": &fstest.MapFile{Data: hpiBytes},
		"README.txt":      &fstest.MapFile{Data: []byte("not a plugin")},
		"some-dir":        &fstest.MapFile{Mode: fs.ModeDir},
	}

	inv, err := CollectFS(fsys)
	if err != nil {
		t.Fatalf("CollectFS: %v", err)
	}
	if inv.Source != SourceFilesystem {
		t.Errorf("Source = %q, want %q", inv.Source, SourceFilesystem)
	}
	if len(inv.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(inv.Records))
	}
	r := inv.Records[0]
	if r.Name != "test-plugin" {
		t.Errorf("Name = %q", r.Name)
	}
	if r.Enabled != TriUnknown || r.Detached != TriUnknown || r.Bundled != TriUnknown {
		t.Errorf("all flags should be TriUnknown, got enabled=%v detached=%v bundled=%v",
			r.Enabled, r.Detached, r.Bundled)
	}
	if len(r.Deps) != 2 {
		t.Fatalf("got %d deps, want 2", len(r.Deps))
	}
	if !r.Deps[1].Optional {
		t.Errorf("dep2 should be optional")
	}
}

func TestCollectSelection_BothFail(t *testing.T) {
	api := &stubJenkinsAPI{err: errors.New("api down")}
	// Use a nonexistent directory to force FS failure.
	dir := t.TempDir()
	_ = os.Remove(dir)
	fsys := os.DirFS(dir)
	inv := CollectSelection(context.Background(), api, fsys)
	if !inv.CollectionFailed {
		t.Fatal("expected CollectionFailed=true")
	}
	if inv.CollectionError == "" {
		t.Error("CollectionError should not be empty")
	}
	if len(inv.Records) != 0 {
		t.Error("should not have records when both fail")
	}
}

func TestCollectSelection_APIFirst(t *testing.T) {
	api := &stubJenkinsAPI{plugins: []jenkins.APIPlugin{
		{ShortName: "api-plugin", Version: "1.0", Enabled: true},
	}}
	inv := CollectSelection(context.Background(), api, fstest.MapFS{})
	if inv.Source != SourceJenkinsAPI {
		t.Errorf("Source = %q, want %q", inv.Source, SourceJenkinsAPI)
	}
	if inv.CollectionFailed {
		t.Error("should not have failed")
	}
}

func TestCollectSelection_FallbackToFS(t *testing.T) {
	api := &stubJenkinsAPI{err: errors.New("api down")}
	manifest := "Short-Name: fs-plugin\nPlugin-Version: 2.0\n"
	hpiBytes := buildMinimalHPI(t, manifest)
	fsys := fstest.MapFS{
		"fs-plugin.hpi": &fstest.MapFile{Data: hpiBytes},
	}
	inv := CollectSelection(context.Background(), api, fsys)
	if inv.Source != SourceFilesystem {
		t.Errorf("Source = %q, want %q", inv.Source, SourceFilesystem)
	}
	if inv.CollectionFailed {
		t.Error("should not have failed")
	}
}

// ==========================================================================
// buildMinimalHPI creates a minimal zip with the given manifest content.
// ==========================================================================

func buildZipWithManifest(manifest string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("META-INF/MANIFEST.MF")
	_, _ = w.Write([]byte(manifest))
	// Add a second entry so the manifest isn't the only member.
	w2, _ := zw.Create("WEB-INF/lib/demo.jar")
	_, _ = w2.Write([]byte("not a real jar"))
	_ = zw.Close()
	return buf.Bytes()
}

func buildMinimalHPI(t *testing.T, manifest string) []byte {
	t.Helper()
	return buildZipWithManifest(manifest)
}

// ==========================================================================
// §3.7 Table tests for classification
// ==========================================================================

func TestClassify_BootstrapRoot(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "varroa-mite-auth", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared:         nil,
		BootstrapRoot:    "varroa-mite-auth",
		BootstrapMatched: true,
	}
	c := Classify(in)
	if c.Total != 1 {
		t.Fatalf("total = %d, want 1", c.Total)
	}
	if c.Plugins[0].Class != ClassBootstrap {
		t.Errorf("class = %q, want %q", c.Plugins[0].Class, ClassBootstrap)
	}
	if c.Counts[ClassBootstrap] != 1 {
		t.Errorf("counts[bootstrap] = %d, want 1", c.Counts[ClassBootstrap])
	}
}

func TestClassify_Declared(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "my-plugin", Version: "2.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "my-plugin", Version: "2.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassDeclared {
		t.Errorf("class = %q, want %q", c.Plugins[0].Class, ClassDeclared)
	}
	if c.Plugins[0].DeclaredBy != DeclaredByCore {
		t.Errorf("declaredBy = %q, want %q", c.Plugins[0].DeclaredBy, DeclaredByCore)
	}
}

func TestClassify_JenkinsSupplied(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "detached-plugin", Version: "1.0", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse},
		}},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassJenkinsSupplied {
		t.Errorf("class = %q, want %q", c.Plugins[0].Class, ClassJenkinsSupplied)
	}
}

func TestClassify_Dependency(t *testing.T) {
	// A declared plugin with a mandatory dependency.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "parent", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "child", Min: "1.0", Optional: false}},
			},
			{Name: "child", Version: "1.5", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "parent", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)
	if c.Total != 2 {
		t.Fatalf("total = %d, want 2", c.Total)
	}
	// Find child.
	var child *ClassifiedPlugin
	for i := range c.Plugins {
		if c.Plugins[i].Name == "child" {
			child = &c.Plugins[i]
			break
		}
	}
	if child == nil {
		t.Fatal("child not found")
	}
	if child.Class != ClassDependency {
		t.Errorf("child class = %q, want %q", child.Class, ClassDependency)
	}
	if len(child.ImpliedBy) != 1 || child.ImpliedBy[0] != "parent" {
		t.Errorf("impliedBy = %v, want [parent]", child.ImpliedBy)
	}
}

func TestClassify_OptionalDependency(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "parent", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "opt-child", Min: "1.0", Optional: true}},
			},
			{Name: "opt-child", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "parent", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)
	var opt *ClassifiedPlugin
	for i := range c.Plugins {
		if c.Plugins[i].Name == "opt-child" {
			opt = &c.Plugins[i]
			break
		}
	}
	if opt == nil {
		t.Fatal("opt-child not found")
	}
	if opt.Class != ClassOptionalDependency {
		t.Errorf("class = %q, want %q", opt.Class, ClassOptionalDependency)
	}
}

func TestClassify_Unmanaged(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "rogue", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassUnmanaged {
		t.Errorf("class = %q, want %q", c.Plugins[0].Class, ClassUnmanaged)
	}
}

func TestClassify_UnmanagedDepsStayUnmanaged(t *testing.T) {
	// An unmanaged plugin has its own dependencies → they are also unmanaged.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "rogue", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "rogue-dep", Min: "1.0", Optional: false}},
			},
			{Name: "rogue-dep", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
	}
	c := Classify(in)
	for _, p := range c.Plugins {
		if p.Class != ClassUnmanaged {
			t.Errorf("plugin %q class = %q, want %q (unmanaged deps must not be laundered)", p.Name, p.Class, ClassUnmanaged)
		}
	}
}

func TestClassify_OptionalBehindMandatoryIntermediate(t *testing.T) {
	// parent (declared) → mandatory → intermediate → optional → leaf
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "parent", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "intermediate", Min: "1.0", Optional: false}},
			},
			{Name: "intermediate", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "leaf", Min: "1.0", Optional: true}},
			},
			{Name: "leaf", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "parent", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)

	// intermediate → dependency (mandatory reachable from declared root)
	var inter, leaf *ClassifiedPlugin
	for i := range c.Plugins {
		switch c.Plugins[i].Name {
		case "intermediate":
			inter = &c.Plugins[i]
		case "leaf":
			leaf = &c.Plugins[i]
		}
	}
	if inter == nil || leaf == nil {
		t.Fatal("missing plugins")
	}
	if inter.Class != ClassDependency {
		t.Errorf("intermediate class = %q, want %q", inter.Class, ClassDependency)
	}
	if leaf.Class != ClassOptionalDependency {
		t.Errorf("leaf class = %q, want %q (must be optional-dependency, not unmanaged)", leaf.Class, ClassOptionalDependency)
	}
}

func TestClassify_UnsatisfiedMinimumAdvisory(t *testing.T) {
	// parent declares dep min=2.0, but child is installed at 1.0
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "parent", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{{Name: "child", Min: "2.0", Optional: false}},
			},
			{Name: "child", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "parent", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)

	// child should still be class 4 (dependency), never unmanaged.
	var child *ClassifiedPlugin
	for i := range c.Plugins {
		if c.Plugins[i].Name == "child" {
			child = &c.Plugins[i]
			break
		}
	}
	if child == nil {
		t.Fatal("child not found")
	}
	if child.Class != ClassDependency {
		t.Errorf("child class = %q, want %q (minimum is a floor, not a gate)", child.Class, ClassDependency)
	}

	// Advisory should be present.
	if len(c.Advisories) != 1 {
		t.Fatalf("advisories = %d, want 1", len(c.Advisories))
	}
	adv := c.Advisories[0]
	if adv.Code != "dependencyMinimumUnsatisfied" {
		t.Errorf("advisory code = %q", adv.Code)
	}
	if adv.Plugin != "parent" || adv.Dependency != "child" {
		t.Errorf("advisory: plugin=%q dep=%q", adv.Plugin, adv.Dependency)
	}
}

// ==========================================================================
// §3.7b Precedence tests
// ==========================================================================

func TestClassify_DeclaredBeforeJenkinsSupplied(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "mailer", Version: "1.0", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "mailer", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassDeclared {
		t.Errorf("class = %q, want %q (declared must win over jenkins-supplied)", c.Plugins[0].Class, ClassDeclared)
	}
}

func TestClassify_UndeclaredDetachedClassifiesJenkinsSupplied(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "detached-only", Version: "1.0", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse},
		}},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassJenkinsSupplied {
		t.Errorf("class = %q, want %q", c.Plugins[0].Class, ClassJenkinsSupplied)
	}
}

func TestClassify_BundledTrueDetachedFalse(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "bundled-only", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriTrue},
		}},
	}
	c := Classify(in)
	if c.Plugins[0].Class != ClassJenkinsSupplied {
		t.Errorf("class = %q, want %q (bundled alone is jenkins-supplied)", c.Plugins[0].Class, ClassJenkinsSupplied)
	}
}

// ==========================================================================
// §3.7c Profile-path bootstrap test
// ==========================================================================

func TestClassify_ProfilePathBootstrap(t *testing.T) {
	// When BootstrapMatched is false (profile path), the root still
	// classifies as bootstrap, its members classify declared/dependency,
	// none classifies unmanaged, and BootstrapApproximate is set.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "varroa-mite-auth", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			{Name: "mailer", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "mailer", Version: "1.0", Tier: DeclaredByCore},
		},
		BootstrapRoot:    "varroa-mite-auth",
		BootstrapMembers: []string{"mailer"},
		BootstrapMatched: false, // profile path
	}
	c := Classify(in)

	// Root must be bootstrap.
	var root, mailer *ClassifiedPlugin
	for i := range c.Plugins {
		switch c.Plugins[i].Name {
		case "varroa-mite-auth":
			root = &c.Plugins[i]
		case "mailer":
			mailer = &c.Plugins[i]
		}
	}
	if root.Class != ClassBootstrap {
		t.Errorf("root class = %q, want %q", root.Class, ClassBootstrap)
	}
	if mailer.Class != ClassDeclared {
		t.Errorf("mailer class = %q, want %q (should be declared)", mailer.Class, ClassDeclared)
	}
	if !c.BootstrapApproximate {
		t.Error("BootstrapApproximate should be true when matched=false")
	}
}

// ==========================================================================
// §3.7c Cross-check test
// ==========================================================================

func TestClassify_BootstrapCrossCheck(t *testing.T) {
	// A recorded non-root member that is not reachable from the root
	// in the observed graph should be class 1 and set BootstrapApproximate.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "varroa-mite-auth", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			{Name: "orphan-member", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared:         nil,
		BootstrapRoot:    "varroa-mite-auth",
		BootstrapMembers: []string{"orphan-member"},
		BootstrapMatched: true,
	}
	c := Classify(in)

	var orphan *ClassifiedPlugin
	for i := range c.Plugins {
		if c.Plugins[i].Name == "orphan-member" {
			orphan = &c.Plugins[i]
			break
		}
	}
	if orphan == nil {
		t.Fatal("orphan-member not found")
	}
	if orphan.Class != ClassBootstrap {
		t.Errorf("class = %q, want %q (cross-check must rescue orphan member)", orphan.Class, ClassBootstrap)
	}
	if !c.BootstrapApproximate {
		t.Error("BootstrapApproximate should be true after cross-check rescue")
	}
}

// ==========================================================================
// §3.6 Version drift tests
// ==========================================================================

func TestVersionVerdict(t *testing.T) {
	if got := versionVerdict("2.0", "1.0"); got != VerdictAhead {
		t.Errorf("2.0 vs 1.0 = %q, want %q", got, VerdictAhead)
	}
	if got := versionVerdict("1.0", "2.0"); got != VerdictBehind {
		t.Errorf("1.0 vs 2.0 = %q, want %q", got, VerdictBehind)
	}
	if got := versionVerdict("1.0", "1.0"); got != VerdictMatch {
		t.Errorf("1.0 vs 1.0 = %q, want %q", got, VerdictMatch)
	}
	if got := versionVerdict("", "1.0"); got != VerdictMissing {
		t.Errorf("empty vs 1.0 = %q, want %q", got, VerdictMissing)
	}
	if got := versionVerdict("1.0", ""); got != VerdictMatch {
		t.Errorf("1.0 vs empty = %q, want %q", got, VerdictMatch)
	}
}

// ==========================================================================
// §3.8 Smoke-mcp fixture test
// ==========================================================================

func TestClassify_SmokeMCPFixture(t *testing.T) {
	// Captured from the live `smoke-mcp` controller on cluster `core` on
	// 2026-07-26, after deploying this branch. Plugin names, versions, detached
	// flags and dependency edges below are verbatim from that controller's
	// /pluginManager/api/json?depth=2, and the declared tiers from the BFF's
	// own classification response. The graph is trimmed to the plugins that
	// exercise every class — the full 85-plugin capture classifies identically.
	//
	// Live ground truth (verified against Controller.status.pluginInventory and
	// GET /clusters/core/controllers/varroa/smoke-mcp/plugins):
	//
	//   total 85 — bootstrap 1, declared 76, jenkins-supplied 2, unmanaged 6
	//
	// NOTE ON THE ORIGINAL SPEC NUMBERS. proposal.md predicted "84 installed,
	// 8 naive, closure-aware 4, actionable 2" with javax-mail-api landing in
	// optional-dependency. The live controller does not reproduce that, and the
	// implementation is right:
	//
	//   - javax-mail-api reports detached=true, so step 3 (jenkins-supplied)
	//     claims it before the closure passes ever run. A detached plugin can
	//     never reach class 5. jdk-tool, hand-installed during the 13.3 smoke,
	//     lands the same way — which is why jenkins-supplied is 2, not 1.
	//   - varroa-mcp-tools plus its dependency closure were hand-installed on
	//     2026-07-18, after the proposal froze, taking unmanaged from 2 to 6.
	//
	// So the funnel in the proposal describes an earlier state of this
	// controller, not a classification defect. This fixture pins what the code
	// actually produces from real input.
	in := Inputs{
		BootstrapRoot: "varroa-mite-auth",
		Inventory: Inventory{
			Source: SourceJenkinsAPI,
			Records: []Record{
				// class 1 — bootstrap root (the varroa auth plugin itself).
				{Name: "varroa-mite-auth", Version: "1.0-SNAPSHOT", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
					Deps: []Dep{
						{Name: "mailer", Min: "534.v1b_36f5864073"},
						{Name: "configuration-as-code", Min: "2082.vdb_db_4622e9fa_", Optional: true},
					}},

				// class 2 — declared, core-lock tier, version matches.
				{Name: "mailer", Version: "534.v1b_36f5864073", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse,
					Deps: []Dep{{Name: "jakarta-mail-api", Min: "2.1.3-2"}}},
				{Name: "configuration-as-code", Version: "2100.vb_fd699d2a_09c", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
				{Name: "jakarta-mail-api", Version: "2.1.5-1", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},

				// class 2 — declared, bundle tier, installed AHEAD of declared.
				{Name: "git", Version: "5.10.1", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
					Deps: []Dep{{Name: "git-client", Min: "6.5.0"}}},
				{Name: "git-client", Version: "6.6.1", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},

				// class 3 — jenkins-supplied via detached, NOT declared.
				// This is the one the proposal expected to be class 5.
				{Name: "javax-mail-api", Version: "1.6.2-11", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse,
					Deps: []Dep{{Name: "javax-activation-api", Min: "1.2.0-7"}}},
				// class 3 — hand-installed during smoke 13.3, also detached.
				{Name: "jdk-tool", Version: "83.v417146707a_3d", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse},

				// Also detached, so also class 3 — see the no-class-4 note below.
				{Name: "javax-activation-api", Version: "1.2.0-8", Enabled: TriTrue, Detached: TriTrue, Bundled: TriFalse},

				// class 6 — hand-installed root and its closure. Because the
				// root is itself unmanaged it is not an expected root, so its
				// dependencies stay unmanaged rather than becoming class 4.
				{Name: "varroa-mcp-tools", Version: "1.0.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
					Deps: []Dep{
						{Name: "pipeline-graph-analysis", Min: "254.v0f63a_a_447dca_"},
					}},
				{Name: "pipeline-graph-analysis", Version: "254.v0f63a_a_447dca_", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			},
		},
		Declared: []DeclaredPlugin{
			{Name: "mailer", Version: "534.v1b_36f5864073", Tier: DeclaredByCore},
			{Name: "configuration-as-code", Version: "2100.vb_fd699d2a_09c", Tier: DeclaredByCore},
			{Name: "jakarta-mail-api", Version: "2.1.5-1", Tier: DeclaredByCore},
			{Name: "git-client", Version: "6.6.1", Tier: DeclaredByCore},
			// Declared BEHIND what is installed — verdict "ahead".
			{Name: "git", Version: "5.9.0", Tier: DeclaredByBundle},
		},
	}

	c := Classify(in)
	byName := map[string]ClassifiedPlugin{}
	for _, p := range c.Plugins {
		byName[p.Name] = p
	}

	if c.Total != len(in.Inventory.Records) {
		t.Fatalf("total = %d, want %d", c.Total, len(in.Inventory.Records))
	}

	// Every class the live controller exhibits, pinned by real plugin name.
	wantClass := map[string]string{
		"varroa-mite-auth":        ClassBootstrap,
		"mailer":                  ClassDeclared,
		"configuration-as-code":   ClassDeclared,
		"jakarta-mail-api":        ClassDeclared,
		"git":                     ClassDeclared,
		"git-client":              ClassDeclared,
		"javax-mail-api":          ClassJenkinsSupplied,
		"jdk-tool":                ClassJenkinsSupplied,
		"javax-activation-api":    ClassJenkinsSupplied,
		"varroa-mcp-tools":        ClassUnmanaged,
		"pipeline-graph-analysis": ClassUnmanaged,
	}
	for name, want := range wantClass {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from classification", name)
		}
		if got.Class != want {
			t.Errorf("%s class = %q, want %q", name, got.Class, want)
		}
	}

	// A detached plugin is claimed by step 3 before the closure runs. Pinned
	// because the proposal predicted class 5 here; if precedence is ever
	// reordered this is the test that should fail and force the discussion.
	if byName["javax-mail-api"].Class == ClassOptionalDependency {
		t.Error("javax-mail-api classified optional-dependency: jenkins-supplied " +
			"precedence over the closure passes was lost")
	}

	// The live controller produces NO class-4 and NO class-5 rows at all:
	// 1 bootstrap + 76 declared + 2 jenkins-supplied + 6 unmanaged = 85. The
	// core-lock declared set is complete enough that every mandatory dependency
	// is itself declared, and everything else is detached. Recorded because it
	// means this fixture cannot exercise the closure passes — those are covered
	// by TestClassify_Dependency, TestClassify_OptionalDependency and
	// TestClassify_ImpliedByReportsRootsNotParents against synthetic graphs.
	for _, p := range c.Plugins {
		if p.Class == ClassDependency || p.Class == ClassOptionalDependency {
			t.Errorf("unexpected %s row %q: the captured smoke-mcp graph has none",
				p.Class, p.Name)
		}
	}

	// Dependencies of an UNMANAGED root stay unmanaged and carry no impliedBy:
	// an unmanaged plugin is not an expected root, so it seeds no closure.
	if ib := byName["pipeline-graph-analysis"].ImpliedBy; len(ib) != 0 {
		t.Errorf("pipeline-graph-analysis impliedBy = %v, want empty "+
			"(its only requirer is itself unmanaged)", ib)
	}

	// Version drift on a declared plugin installed ahead of its pin.
	if v := byName["git"].VersionVerdict; v != VerdictAhead {
		t.Errorf("git versionVerdict = %q, want %q", v, VerdictAhead)
	}
}

// ==========================================================================
// §3.9 Shared-semantic test against closure.go
// ==========================================================================

func TestClassify_AgreementWithClosureSemantics(t *testing.T) {
	// This test asserts agreement with internal/updatecenter/closure.go on:
	// 1. Optional dependencies are recorded but never followed for mandatory closure.
	// 2. Dependency versions are minimums (floors), not pins — an unsatisfied
	//    minimum doesn't change the class.

	// Set up: declared parent has an optional dep and a mandatory dep with
	// an unsatisfied minimum.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "parent", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse,
				Deps: []Dep{
					{Name: "mandatory-child", Min: "10.0", Optional: false}, // very high minimum
					{Name: "optional-child", Min: "1.0", Optional: true},
				},
			},
			{Name: "mandatory-child", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			{Name: "optional-child", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "parent", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)

	// mandatory-child: minimum 10.0 is unsatisfied, but it's still dependency (class 4).
	for _, p := range c.Plugins {
		switch p.Name {
		case "mandatory-child":
			if p.Class != ClassDependency {
				t.Errorf("mandatory-child class = %q, want %q (minimums are floors, not gates)",
					p.Class, ClassDependency)
			}
		case "optional-child":
			if p.Class != ClassOptionalDependency {
				t.Errorf("optional-child class = %q, want %q (optional deps are recorded, not followed)",
					p.Class, ClassOptionalDependency)
			}
		}
	}

	// There should be an unsatisfied-minimum advisory for mandatory-child.
	found := false
	for _, adv := range c.Advisories {
		if adv.Dependency == "mandatory-child" {
			found = true
			if pluginver.AtLeast("1.0", "10.0") {
				t.Error("AtLeast(1.0, 10.0) should be false")
			}
			break
		}
	}
	if !found {
		t.Error("expected unsatisfied-minimum advisory for mandatory-child")
	}
}

// ==========================================================================
// R15 test: attribution absence degrades only contributors
// ==========================================================================

func TestClassify_R15_AttributionAbsence(t *testing.T) {
	withAttr := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "p1", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			{Name: "p2", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "p1", Version: "1.0", Tier: DeclaredByBundle, Contributors: []string{"input-a", "input-b"}},
			{Name: "p2", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	withoutAttr := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "p1", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
			{Name: "p2", Version: "1.0", Enabled: TriTrue, Detached: TriFalse, Bundled: TriFalse},
		}},
		Declared: []DeclaredPlugin{
			{Name: "p1", Version: "1.0", Tier: DeclaredByBundle}, // no contributors
			{Name: "p2", Version: "1.0", Tier: DeclaredByCore},
		},
	}

	c1 := Classify(withAttr)
	c2 := Classify(withoutAttr)

	// Classes, counts, drift set, condition must be identical.
	if c1.Total != c2.Total {
		t.Errorf("total differs: %d vs %d", c1.Total, c2.Total)
	}
	for k, v := range c1.Counts {
		if c2.Counts[k] != v {
			t.Errorf("counts[%s] differs: %d vs %d", k, v, c2.Counts[k])
		}
	}
	for k := range c2.Counts {
		if _, ok := c1.Counts[k]; !ok {
			t.Errorf("counts[%s] missing from with-attr", k)
		}
	}
	if c1.BootstrapApproximate != c2.BootstrapApproximate {
		t.Error("BootstrapApproximate differs")
	}
	// Only contributors differ.
	for i := range c1.Plugins {
		if c1.Plugins[i].Name != c2.Plugins[i].Name {
			t.Errorf("plugin order differs at %d", i)
			continue
		}
		if c1.Plugins[i].Class != c2.Plugins[i].Class {
			t.Errorf("plugin %q class differs: %s vs %s",
				c1.Plugins[i].Name, c1.Plugins[i].Class, c2.Plugins[i].Class)
		}
	}
}

// TestClassify_ImpliedByReportsRootsNotParents pins the published impliedBy
// contract: it names the EXPECTED ROOTS whose closure reaches a plugin, not the
// immediate predecessor that declares the edge.
//
// Graph: declared roots "a" and "x" both depend on "b"; "b" depends on "c".
// "c" must report [a x] — both roots, sorted — not [b].
func TestClassify_ImpliedByReportsRootsNotParents(t *testing.T) {
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "a", Version: "1.0", Deps: []Dep{{Name: "b", Min: "1.0"}}},
			{Name: "x", Version: "1.0", Deps: []Dep{{Name: "b", Min: "1.0"}}},
			{Name: "b", Version: "1.0", Deps: []Dep{{Name: "c", Min: "1.0"}}},
			{Name: "c", Version: "1.0"},
		}},
		Declared: []DeclaredPlugin{
			{Name: "a", Version: "1.0", Tier: DeclaredByCore},
			{Name: "x", Version: "1.0", Tier: DeclaredByCore},
		},
	}
	c := Classify(in)

	byName := map[string]*ClassifiedPlugin{}
	for i := range c.Plugins {
		byName[c.Plugins[i].Name] = &c.Plugins[i]
	}

	// The transitive dependency names both roots, not its parent "b".
	got := byName["c"]
	if got == nil {
		t.Fatal("c not classified")
	}
	if got.Class != ClassDependency {
		t.Errorf("c class = %q, want %q", got.Class, ClassDependency)
	}
	if len(got.ImpliedBy) != 2 || got.ImpliedBy[0] != "a" || got.ImpliedBy[1] != "x" {
		t.Errorf("c impliedBy = %v, want [a x] (roots, sorted — not the parent b)", got.ImpliedBy)
	}

	// The direct dependency records both roots that reach it, not just the
	// first one the traversal happened to visit.
	if b := byName["b"]; b == nil {
		t.Fatal("b not classified")
	} else if len(b.ImpliedBy) != 2 || b.ImpliedBy[0] != "a" || b.ImpliedBy[1] != "x" {
		t.Errorf("b impliedBy = %v, want [a x]", b.ImpliedBy)
	}
}

// TestClassify_AdvisoriesDeterministicOrder pins advisory ordering. Advisories
// are accumulated by ranging a map, so a dependency with two declaring parents
// emitted them in randomized order. The operator compares advisories
// positionally, so an unstable order made an unchanged classification compare
// unequal every reconcile and rewrite the read model forever.
func TestClassify_AdvisoriesDeterministicOrder(t *testing.T) {
	// Two declared roots both require "shared" above the installed version, so
	// both raise an unsatisfied-minimum advisory against the same dependency.
	in := Inputs{
		Inventory: Inventory{Records: []Record{
			{Name: "alpha", Version: "1.0", Deps: []Dep{{Name: "shared", Min: "9.0"}}},
			{Name: "zulu", Version: "1.0", Deps: []Dep{{Name: "shared", Min: "8.0"}}},
			{Name: "shared", Version: "1.0"},
		}},
		Declared: []DeclaredPlugin{
			{Name: "alpha", Version: "1.0", Tier: DeclaredByCore},
			{Name: "zulu", Version: "1.0", Tier: DeclaredByCore},
		},
	}

	first := Classify(in).Advisories
	if len(first) < 2 {
		t.Fatalf("expected >=2 advisories from two declaring parents, got %d", len(first))
	}

	// Repeat: Go randomizes map iteration per range, so an unsorted
	// implementation diverges across runs well within this many attempts.
	for i := 0; i < 50; i++ {
		got := Classify(in).Advisories
		if len(got) != len(first) {
			t.Fatalf("run %d: advisory count %d, want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: advisory[%d] = %+v, want %+v (order is not deterministic)",
					i, j, got[j], first[j])
			}
		}
	}
}
