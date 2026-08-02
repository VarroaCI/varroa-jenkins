package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// precomputeDigest computes the "sha256:<hex>" digest for the given content.
func precomputeDigest(content []byte) string {
	h := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", h)
}

func TestBuildAndReadPluginPack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	// Synthetic plugin content
	pluginAContent := []byte("plugin-a-hpi-data")
	pluginBContent := []byte("plugin-b-hpi-data")

	plugins := []ResolvedPlugin{
		{
			Name:        "plugin-a",
			Version:     "1.0.0",
			SHA256:      precomputeDigest(pluginAContent),
			UpstreamURL: "https://upstream.example.com/plugin-a.hpi",
			Content:     bytes.NewReader(pluginAContent),
		},
		{
			Name:        "plugin-b",
			Version:     "2.1.0",
			SHA256:      precomputeDigest(pluginBContent),
			UpstreamURL: "https://upstream.example.com/plugin-b.hpi",
			Content:     bytes.NewReader(pluginBContent),
		},
	}

	cfg := PackConfig{
		Kind:           PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test-profile",
		LockHash:       LockHash(plugins),
		PluginCount:    len(plugins),
		CreatedAt:      "2025-07-18T12:00:00Z",
	}

	ref := "plugin-pack:v1"
	if err := BuildPluginPack(ctx, store, ref, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	// Read it back
	gotCfg, gotPlugins, err := ReadPluginPack(ctx, store, ref)
	if err != nil {
		t.Fatalf("ReadPluginPack: %v", err)
	}

	// Verify PackConfig
	if gotCfg.JenkinsVersion != cfg.JenkinsVersion {
		t.Errorf("JenkinsVersion = %q, want %q", gotCfg.JenkinsVersion, cfg.JenkinsVersion)
	}
	if gotCfg.Profile != cfg.Profile {
		t.Errorf("Profile = %q, want %q", gotCfg.Profile, cfg.Profile)
	}
	if gotCfg.LockHash != cfg.LockHash {
		t.Errorf("LockHash = %q, want %q", gotCfg.LockHash, cfg.LockHash)
	}
	if gotCfg.PluginCount != cfg.PluginCount {
		t.Errorf("PluginCount = %d, want %d", gotCfg.PluginCount, cfg.PluginCount)
	}
	if gotCfg.CreatedAt != cfg.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", gotCfg.CreatedAt, cfg.CreatedAt)
	}

	// Verify plugins
	if len(gotPlugins) != len(plugins) {
		t.Fatalf("got %d plugins, want %d", len(gotPlugins), len(plugins))
	}
	for i, gp := range gotPlugins {
		if gp.Name != plugins[i].Name {
			t.Errorf("plugin[%d].Name = %q, want %q", i, gp.Name, plugins[i].Name)
		}
		if gp.Version != plugins[i].Version {
			t.Errorf("plugin[%d].Version = %q, want %q", i, gp.Version, plugins[i].Version)
		}
		if gp.SHA256 != plugins[i].SHA256 {
			t.Errorf("plugin[%d].SHA256 = %q, want %q", i, gp.SHA256, plugins[i].SHA256)
		}
		if gp.UpstreamURL != plugins[i].UpstreamURL {
			t.Errorf("plugin[%d].UpstreamURL = %q, want %q", i, gp.UpstreamURL, plugins[i].UpstreamURL)
		}
		if gp.Content != nil {
			t.Errorf("plugin[%d].Content should be nil after ReadPluginPack", i)
		}
	}
}

func TestBuildPluginPack_EmptyVersionRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	plugins := []ResolvedPlugin{
		{
			Name:    "plugin-good",
			Version: "1.0.0",
			SHA256:  precomputeDigest([]byte("good-data")),
			Content: bytes.NewReader([]byte("good-data")),
		},
		{
			Name:    "plugin-bad",
			Version: "", // empty — should be rejected
			SHA256:  precomputeDigest([]byte("bad-data")),
			Content: bytes.NewReader([]byte("bad-data")),
		},
	}

	cfg := PackConfig{
		Kind:           PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test",
		PluginCount:    2,
	}

	err = BuildPluginPack(ctx, store, "rejected:v1", cfg, plugins)
	if err == nil {
		t.Fatal("expected error for empty version, got nil")
	}
	if !strings.Contains(err.Error(), "empty version") {
		t.Errorf("error = %q, want 'empty version'", err)
	}

	// Assert no manifest was pushed
	descs, err := store.ListManifests(ctx)
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	if len(descs) != 0 {
		t.Errorf("expected no manifests, got %d", len(descs))
	}
}

func TestBuildPluginPack_DigestMismatchRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	// Content and claimed SHA256 don't match
	actualContent := []byte("actual-content")
	wrongDigest := precomputeDigest([]byte("different-content"))

	plugins := []ResolvedPlugin{
		{
			Name:    "plugin-mismatch",
			Version: "1.0.0",
			SHA256:  wrongDigest, // does NOT match actualContent
			Content: bytes.NewReader(actualContent),
		},
	}

	cfg := PackConfig{
		Kind:           PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test",
		PluginCount:    1,
	}

	err = BuildPluginPack(ctx, store, "mismatch:v1", cfg, plugins)
	if err == nil {
		t.Fatal("expected error for digest mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %q, want 'digest mismatch'", err)
	}

	// Assert no manifest was pushed
	descs, err := store.ListManifests(ctx)
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	if len(descs) != 0 {
		t.Errorf("expected no manifests, got %d", len(descs))
	}
}

func TestLockHash_Deterministic(t *testing.T) {
	plugins1 := []ResolvedPlugin{
		{Name: "plugin-b", Version: "2.0.0"},
		{Name: "plugin-a", Version: "1.0.0"},
		{Name: "plugin-c", Version: "3.0.0"},
	}

	plugins2 := []ResolvedPlugin{
		{Name: "plugin-c", Version: "3.0.0"},
		{Name: "plugin-a", Version: "1.0.0"},
		{Name: "plugin-b", Version: "2.0.0"},
	}

	h1 := LockHash(plugins1)
	h2 := LockHash(plugins2)

	if h1 != h2 {
		t.Errorf("LockHash order-dependent: %q != %q", h1, h2)
	}
}

func TestBuildPluginPack_Twice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	pluginContent := []byte("same-content-plugin")
	plugins := []ResolvedPlugin{
		{
			Name:        "plugin-x",
			Version:     "1.0.0",
			SHA256:      precomputeDigest(pluginContent),
			UpstreamURL: "https://example.com/plugin-x.hpi",
			Content:     bytes.NewReader(pluginContent),
		},
	}

	// Build the first pack
	cfg1 := PackConfig{
		Kind:           PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test",
		LockHash:       LockHash(plugins),
		PluginCount:    1,
		CreatedAt:      "2025-07-18T12:00:00Z",
	}

	if err := BuildPluginPack(ctx, store, "pack:v1", cfg1, plugins); err != nil {
		t.Fatalf("first BuildPluginPack: %v", err)
	}

	// Build again with different CreatedAt
	plugins[0].Content = bytes.NewReader(pluginContent) // rewind reader
	cfg2 := PackConfig{
		Kind:           PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test",
		LockHash:       LockHash(plugins),
		PluginCount:    1,
		CreatedAt:      "2025-07-18T14:00:00Z",
	}

	if err := BuildPluginPack(ctx, store, "pack:v2", cfg2, plugins); err != nil {
		t.Fatalf("second BuildPluginPack: %v", err)
	}

	// Verify both packs exist and have identical layer digests and LockHash
	_, p1, err := ReadPluginPack(ctx, store, "pack:v1")
	if err != nil {
		t.Fatalf("ReadPluginPack pack:v1: %v", err)
	}
	_, p2, err := ReadPluginPack(ctx, store, "pack:v2")
	if err != nil {
		t.Fatalf("ReadPluginPack pack:v2: %v", err)
	}

	if len(p1) != len(p2) {
		t.Fatalf("plugin counts differ: %d vs %d", len(p1), len(p2))
	}
	for i := range p1 {
		if p1[i].SHA256 != p2[i].SHA256 {
			t.Errorf("plugin[%d] SHA256: %q vs %q (should be identical)", i, p1[i].SHA256, p2[i].SHA256)
		}
	}

	if cfg1.LockHash != cfg2.LockHash {
		t.Errorf("LockHash should be identical: %q vs %q", cfg1.LockHash, cfg2.LockHash)
	}
}
