package oci

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

// hpiFixture builds a genuine (if minimal) .hpi holding the given manifest
// attributes, so ApplyHPIMetadata is exercised against a real archive rather
// than a stub.
func hpiFixture(t *testing.T, shortName, version, longName, jenkinsVersion, deps string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\nShort-Name: " + shortName + "\r\nPlugin-Version: " + version + "\r\n"
	if longName != "" {
		mf += "Long-Name: " + longName + "\r\n"
	}
	if jenkinsVersion != "" {
		mf += "Jenkins-Version: " + jenkinsVersion + "\r\n"
	}
	if deps != "" {
		mf += "Plugin-Dependencies: " + deps + "\r\n"
	}
	mf += "\r\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	if _, err := w.Write([]byte(mf)); err != nil {
		t.Fatalf("write manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestApplyHPIMetadata(t *testing.T) {
	data := hpiFixture(t, "demo", "1.2.3", "Demo Plugin", "2.516.3",
		"mailer:534.v1b_36f5864073,configuration-as-code:2082.vdb_db_4622e9fa_;resolution:=optional")

	var p ResolvedPlugin
	p.Name = "demo"
	if err := ApplyHPIMetadata(&p, data); err != nil {
		t.Fatalf("ApplyHPIMetadata: %v", err)
	}
	if p.DisplayName != "Demo Plugin" {
		t.Errorf("DisplayName = %q", p.DisplayName)
	}
	if p.RequiredCore != "2.516.3" {
		t.Errorf("RequiredCore = %q", p.RequiredCore)
	}
	if len(p.Dependencies) != 2 || p.Dependencies[0].Min != "534.v1b_36f5864073" || !p.Dependencies[1].Optional {
		t.Errorf("Dependencies = %+v", p.Dependencies)
	}
}

func TestApplyHPIMetadata_UnparseableIsSentinel(t *testing.T) {
	p := ResolvedPlugin{Name: "demo"}
	err := ApplyHPIMetadata(&p, []byte("not a zip"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrHPIMetadata) {
		t.Errorf("error does not wrap ErrHPIMetadata: %v", err)
	}
	if p.DisplayName != "" || p.RequiredCore != "" || p.Dependencies != nil {
		t.Errorf("a failed parse must leave the plugin untouched: %+v", p)
	}
	if !strings.Contains(err.Error(), "demo") {
		t.Errorf("error should name the plugin: %v", err)
	}
}

func TestBuildPluginPack_KindValidation(t *testing.T) {
	content := []byte("hpi-bytes")
	onePlugin := func() []ResolvedPlugin {
		return []ResolvedPlugin{{
			Name: "a", Version: "1.0",
			SHA256:  precomputeDigest(content),
			Content: bytes.NewReader(content),
		}}
	}

	tests := []struct {
		name    string
		cfg     PackConfig
		plugins []ResolvedPlugin
		wantErr string
	}{
		{
			name:    "missing kind",
			cfg:     PackConfig{Profile: "p", PluginCount: 1},
			plugins: onePlugin(),
			wantErr: "no kind",
		},
		{
			name:    "unknown kind",
			cfg:     PackConfig{Kind: "bundle", Profile: "p", PluginCount: 1},
			plugins: onePlugin(),
			wantErr: "unknown kind",
		},
		{
			name:    "profile with empty profile name",
			cfg:     PackConfig{Kind: PackKindProfile, PluginCount: 1},
			plugins: onePlugin(),
			wantErr: "requires a non-empty profile",
		},
		{
			name:    "addon with a profile name",
			cfg:     PackConfig{Kind: PackKindAddon, Profile: "p", PluginCount: 1},
			plugins: onePlugin(),
			wantErr: "requires an empty profile",
		},
		{
			name:    "addon with two plugins",
			cfg:     PackConfig{Kind: PackKindAddon, PluginCount: 2},
			plugins: append(onePlugin(), onePlugin()...),
			wantErr: "exactly one plugin",
		},
		{
			name:    "pluginCount mismatch",
			cfg:     PackConfig{Kind: PackKindProfile, Profile: "p", PluginCount: 7},
			plugins: onePlugin(),
			wantErr: "does not match",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewLayoutStore(dir)
			if err != nil {
				t.Fatalf("NewLayoutStore: %v", err)
			}
			err = BuildPluginPack(context.Background(), store, "ref:v1", tc.cfg, tc.plugins)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			// Nothing may have been pushed.
			if _, err := store.Resolve(context.Background(), "ref:v1"); err == nil {
				t.Error("a rejected pack must leave the store untouched")
			}
		})
	}
}

func TestBuildAndReadPluginPack_AddonRoundTripsTypedMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := NewLayoutStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	content := hpiFixture(t, "varroa-mcp-tools", "1.0.0", "Varroa MCP Tools", "2.533",
		"mcp-server:1.0,workflow-api:2.0;resolution:=optional")
	plugin := ResolvedPlugin{
		Name:         "varroa-mcp-tools",
		Version:      "1.0.0",
		SHA256:       precomputeDigest(content),
		DisplayName:  "Varroa MCP Tools",
		Description:  "First-party MCP endpoint tooling",
		Tags:         []string{"varroa", "mcp"},
		RequiredCore: "2.533",
		Dependencies: []hpi.Dependency{
			{Name: "mcp-server", Min: "1.0"},
			{Name: "workflow-api", Min: "2.0", Optional: true},
		},
		Content: bytes.NewReader(content),
	}
	plugins := []ResolvedPlugin{plugin}
	cfg := PackConfig{
		Kind:           PackKindAddon,
		JenkinsVersion: "2.533",
		LockHash:       LockHash(plugins),
		PluginCount:    1,
		CreatedAt:      "2026-07-25T00:00:00Z",
	}

	if err := BuildPluginPack(ctx, store, "addon:v1", cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}
	gotCfg, gotPlugins, err := ReadPluginPack(ctx, store, "addon:v1")
	if err != nil {
		t.Fatalf("ReadPluginPack: %v", err)
	}
	if gotCfg.Kind != PackKindAddon || gotCfg.Profile != "" || gotCfg.PluginCount != 1 {
		t.Errorf("cfg = %+v", gotCfg)
	}
	if len(gotPlugins) != 1 {
		t.Fatalf("got %d plugins", len(gotPlugins))
	}
	g := gotPlugins[0]
	if g.DisplayName != plugin.DisplayName || g.Description != plugin.Description || g.RequiredCore != plugin.RequiredCore {
		t.Errorf("metadata round trip failed: %+v", g)
	}
	if len(g.Tags) != 2 || g.Tags[0] != "varroa" || g.Tags[1] != "mcp" {
		t.Errorf("Tags = %v", g.Tags)
	}
	if len(g.Dependencies) != 2 {
		t.Fatalf("Dependencies = %+v", g.Dependencies)
	}
	for i := range plugin.Dependencies {
		if g.Dependencies[i] != plugin.Dependencies[i] {
			t.Errorf("dependency %d = %+v, want %+v", i, g.Dependencies[i], plugin.Dependencies[i])
		}
	}
	// Declared order must survive so the annotation is byte-stable.
	if g.Dependencies[0].Name != "mcp-server" {
		t.Errorf("declared order not preserved: %+v", g.Dependencies)
	}
}

func TestBuildPluginPack_OmitsEmptyAnnotations(t *testing.T) {
	ctx := context.Background()
	store, err := NewLayoutStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	content := []byte("bare-hpi")
	plugins := []ResolvedPlugin{{
		Name: "bare", Version: "1.0",
		SHA256:  precomputeDigest(content),
		Content: bytes.NewReader(content),
	}}
	cfg := PackConfig{Kind: PackKindProfile, Profile: "p", PluginCount: 1, LockHash: LockHash(plugins)}
	if err := BuildPluginPack(ctx, store, "bare:v1", cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}
	m, err := store.Pull(ctx, "bare:v1")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	ann := m.Layers[0].Annotations
	for _, k := range []string{
		AnnPluginUpstreamURL, AnnPluginDisplayName, AnnPluginDescription,
		AnnPluginTags, AnnPluginRequiredCore, AnnPluginDependencies,
	} {
		if _, present := ann[k]; present {
			t.Errorf("annotation %q must be omitted when empty, got %q", k, ann[k])
		}
	}
	for _, k := range []string{AnnPluginName, AnnPluginVersion, AnnPluginSHA256} {
		if ann[k] == "" {
			t.Errorf("annotation %q must always be written", k)
		}
	}
}

func TestReadPluginPack_RejectsLegacyAndMalformed(t *testing.T) {
	ctx := context.Background()

	t.Run("config without kind", func(t *testing.T) {
		store, err := NewLayoutStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewLayoutStore: %v", err)
		}
		// A pre-kind config blob, exactly as an older Varroa wrote it.
		legacy, _ := json.Marshal(map[string]any{
			"jenkinsVersion": "2.479.3", "profile": "2-555", "lockHash": "x", "pluginCount": 0,
		})
		d, size, err := store.PushBlob(ctx, MediaTypePackConfig, bytes.NewReader(legacy))
		if err != nil {
			t.Fatalf("PushBlob: %v", err)
		}
		if err := store.Push(ctx, "legacy:v1", Manifest{
			ArtifactType: ArtifactTypePluginPack,
			Config:       Descriptor{MediaType: MediaTypePackConfig, Digest: d, Size: size},
		}); err != nil {
			t.Fatalf("Push: %v", err)
		}
		_, _, err = ReadPluginPack(ctx, store, "legacy:v1")
		if err == nil || !strings.Contains(err.Error(), "no kind") {
			t.Fatalf("expected a no-kind rejection, got %v", err)
		}
	})

	for _, tc := range []struct{ name, key, value string }{
		{"malformed tags", AnnPluginTags, "[not json"},
		{"malformed dependencies", AnnPluginDependencies, "{\"name\":\"x\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewLayoutStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewLayoutStore: %v", err)
			}
			cfgBytes, _ := json.Marshal(PackConfig{Kind: PackKindAddon, PluginCount: 1})
			cd, csize, err := store.PushBlob(ctx, MediaTypePackConfig, bytes.NewReader(cfgBytes))
			if err != nil {
				t.Fatalf("PushBlob: %v", err)
			}
			ld, lsize, err := store.PushBlob(ctx, MediaTypePluginHPI, strings.NewReader("hpi"))
			if err != nil {
				t.Fatalf("PushBlob: %v", err)
			}
			if err := store.Push(ctx, "bad:v1", Manifest{
				ArtifactType: ArtifactTypePluginPack,
				Config:       Descriptor{MediaType: MediaTypePackConfig, Digest: cd, Size: csize},
				Layers: []Descriptor{{
					MediaType: MediaTypePluginHPI, Digest: ld, Size: lsize,
					Annotations: map[string]string{
						AnnPluginName: "x", AnnPluginVersion: "1.0", tc.key: tc.value,
					},
				}},
			}); err != nil {
				t.Fatalf("Push: %v", err)
			}
			// A corrupted structured value must NOT read as an empty one.
			_, _, err = ReadPluginPack(ctx, store, "bad:v1")
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("expected a malformed-annotation error, got %v", err)
			}
		})
	}
}
