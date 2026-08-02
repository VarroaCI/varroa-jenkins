package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

const (
	// ArtifactTypePluginPack is the OCI artifact type for a Varroa plugin pack.
	ArtifactTypePluginPack = "application/vnd.varroa.pluginpack.v1"
	// MediaTypePackConfig is the media type for the plugin pack config blob.
	MediaTypePackConfig = "application/vnd.varroa.pluginpack.config.v1+json"
	// MediaTypePluginHPI is the media type for a Jenkins plugin HPI file.
	MediaTypePluginHPI = "application/vnd.varroa.plugin.hpi.v1"
)

// Layer annotation keys. These strings are a frozen cross-change interface —
// producers in internal/updatecenter and consumers in the catalog sync live
// outside this package, so all of them are exported and internal/oci owns the
// only encoder for the structured ones.
const (
	AnnPluginName         = "dev.varroa.plugin.name"
	AnnPluginVersion      = "dev.varroa.plugin.version"
	AnnPluginSHA256       = "dev.varroa.plugin.sha256"
	AnnPluginUpstreamURL  = "dev.varroa.plugin.upstreamUrl"
	AnnPluginDisplayName  = "dev.varroa.plugin.displayName"
	AnnPluginDescription  = "dev.varroa.plugin.description"
	AnnPluginTags         = "dev.varroa.plugin.tags"
	AnnPluginRequiredCore = "dev.varroa.plugin.requiredCore"
	AnnPluginDependencies = "dev.varroa.plugin.dependencies"
)

// annotation keys for manifest annotations
const (
	annPackProfile    = "dev.varroa.pack.profile"
	annPackJenkinsVer = "dev.varroa.pack.jenkinsVersion"
	annPackLockHash   = "dev.varroa.pack.lockHash"
)

// Pack kinds. `kind` is the sole discriminator between a bulk profile export
// and a standalone single-plugin pack; an empty Profile is deliberately NOT an
// implicit discriminator.
const (
	// PackKindProfile is a bulk export of a version profile's resolved plugin set.
	PackKindProfile = "profile"
	// PackKindAddon is a standalone single-plugin pack.
	PackKindAddon = "addon"
)

// PackConfig describes the configuration of a Varroa plugin pack.
//
// Per-kind field semantics:
//
//	field          | profile                       | addon
//	kind           | "profile"                     | "addon"
//	jenkinsVersion | the profile's Jenkins version  | the plugin's Jenkins-Version (may be empty)
//	profile        | profile name, required         | MUST be empty
//	lockHash       | LockHash(plugins)              | LockHash over the single entry
//	pluginCount    | len(plugins)                   | 1
//
// UploadedBy/UploadedAt are provenance for a pack written by an authenticated
// user upload. They are config-blob fields, deliberately NOT layer annotations:
// the annotation contract is frozen cross-change, and provenance is a property
// of the write, not of the plugin. Both are empty on packs written by any other
// producer (profile export, seed import, pull-through).
type PackConfig struct {
	Kind           string `json:"kind"` // "profile" | "addon"
	JenkinsVersion string `json:"jenkinsVersion"`
	Profile        string `json:"profile"`
	LockHash       string `json:"lockHash"`
	PluginCount    int    `json:"pluginCount"`
	CreatedAt      string `json:"createdAt"`            // RFC3339
	UploadedBy     string `json:"uploadedBy,omitempty"` // authenticated subject
	UploadedAt     string `json:"uploadedAt,omitempty"` // RFC3339
}

// ResolvedPlugin describes a single resolved plugin in a pack.
type ResolvedPlugin struct {
	Name        string
	Version     string
	SHA256      string // "sha256:<hex>" — pre-computed + pre-verified by the caller
	UpstreamURL string
	// DisplayName, RequiredCore, and Dependencies are derived from the plugin's
	// own HPI manifest (see ApplyHPIMetadata). Description and Tags are
	// operator-supplied and are addon-only. All five are optional on read.
	DisplayName  string
	Description  string
	Tags         []string
	RequiredCore string
	Dependencies []hpi.Dependency
	Content      io.Reader // already-verified .hpi bytes; nil when returned by ReadPluginPack
}

// packDependency is the frozen wire shape of dev.varroa.plugin.dependencies.
// Field names and order are a cross-change contract.
type packDependency struct {
	Name     string `json:"name"`
	Min      string `json:"min"`
	Optional bool   `json:"optional"`
}

// ErrHPIMetadata is the sentinel wrapped by ApplyHPIMetadata when a plugin's
// manifest cannot be read. Bulk producers treat it as non-fatal: they pack the
// plugin with the derived annotations omitted and log a warning. The addon
// producer treats it as fatal, because an addon's identity comes only from the
// manifest.
var ErrHPIMetadata = errors.New("oci: plugin HPI metadata unavailable")

// ApplyHPIMetadata populates DisplayName, RequiredCore, and Dependencies on p
// from the plugin's own .hpi bytes. It is the single implementation of the
// derived-annotation contract shared by every pack producer.
//
// On a manifest that will not parse it leaves p untouched and returns an error
// wrapping ErrHPIMetadata, which a bulk producer may treat as non-fatal.
func ApplyHPIMetadata(p *ResolvedPlugin, hpiBytes []byte) error {
	mf, err := hpi.ParseHPIBytes(hpiBytes)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrHPIMetadata, p.Name, err)
	}
	p.DisplayName = mf.LongName
	p.RequiredCore = mf.RequiredCore
	p.Dependencies = mf.Dependencies
	return nil
}

// validatePack checks every whole-pack invariant. It runs before any blob or
// manifest is pushed, so a rejected pack leaves the store untouched.
func validatePack(cfg PackConfig, plugins []ResolvedPlugin) error {
	// Reject empty versions before any push
	for _, p := range plugins {
		if p.Version == "" {
			return fmt.Errorf("plugin %q has empty version", p.Name)
		}
	}
	switch cfg.Kind {
	case PackKindProfile:
		if cfg.Profile == "" {
			return errors.New("pack kind \"profile\" requires a non-empty profile")
		}
	case PackKindAddon:
		if len(plugins) != 1 {
			return fmt.Errorf("pack kind \"addon\" requires exactly one plugin, got %d", len(plugins))
		}
		if cfg.Profile != "" {
			return fmt.Errorf("pack kind \"addon\" requires an empty profile, got %q", cfg.Profile)
		}
	case "":
		return errors.New("pack config has no kind")
	default:
		return fmt.Errorf("pack config has unknown kind %q", cfg.Kind)
	}
	if cfg.PluginCount != len(plugins) {
		return fmt.Errorf("pack config pluginCount %d does not match %d plugins", cfg.PluginCount, len(plugins))
	}
	return nil
}

// layerAnnotations builds the annotation map for one plugin layer. Empty
// strings and nil slices are OMITTED rather than written empty, so a pack never
// carries misleading empty metadata.
func layerAnnotations(p ResolvedPlugin, digest string) (map[string]string, error) {
	ann := map[string]string{
		AnnPluginName:    p.Name,
		AnnPluginVersion: p.Version,
		AnnPluginSHA256:  digest,
	}
	for k, v := range map[string]string{
		AnnPluginUpstreamURL:  p.UpstreamURL,
		AnnPluginDisplayName:  p.DisplayName,
		AnnPluginDescription:  p.Description,
		AnnPluginRequiredCore: p.RequiredCore,
	} {
		if v != "" {
			ann[k] = v
		}
	}
	if len(p.Tags) > 0 {
		b, err := json.Marshal(p.Tags)
		if err != nil {
			return nil, fmt.Errorf("marshal tags for plugin %q: %w", p.Name, err)
		}
		ann[AnnPluginTags] = string(b)
	}
	if len(p.Dependencies) > 0 {
		deps := make([]packDependency, len(p.Dependencies))
		for i, d := range p.Dependencies {
			deps[i] = packDependency{Name: d.Name, Min: d.Min, Optional: d.Optional}
		}
		b, err := json.Marshal(deps)
		if err != nil {
			return nil, fmt.Errorf("marshal dependencies for plugin %q: %w", p.Name, err)
		}
		ann[AnnPluginDependencies] = string(b)
	}
	return ann, nil
}

// PluginLayerAnnotations returns exactly the layer annotations BuildPluginPack
// would write for p, using p.SHA256 as the content digest. It exists so a
// producer can show what it is about to write (a --dry-run) without standing up
// a second encoder for the structured values.
func PluginLayerAnnotations(p ResolvedPlugin) (map[string]string, error) {
	return layerAnnotations(p, p.SHA256)
}

// BuildPluginPack pushes a complete plugin pack to the store.
// It validates the whole pack before pushing anything.
// If a layer blob's actual digest doesn't match the claimed SHA256, it fails
// closed (no config or manifest pushed).
func BuildPluginPack(ctx context.Context, store BlobStore, ref string, cfg PackConfig, plugins []ResolvedPlugin) error {
	if err := validatePack(cfg, plugins); err != nil {
		return err
	}

	// Push each plugin as a layer blob
	layerDescs := make([]Descriptor, 0, len(plugins))
	for _, p := range plugins {
		digest, size, err := store.PushBlob(ctx, MediaTypePluginHPI, p.Content)
		if err != nil {
			return fmt.Errorf("push plugin %q blob: %w", p.Name, err)
		}
		// Defensive check: the pushed digest must match the claimed SHA256
		if digest != p.SHA256 {
			return fmt.Errorf("plugin %q content digest mismatch: got %q, claimed %q", p.Name, digest, p.SHA256)
		}
		ann, err := layerAnnotations(p, digest)
		if err != nil {
			return err
		}
		layerDescs = append(layerDescs, Descriptor{
			MediaType:   MediaTypePluginHPI,
			Digest:      digest,
			Size:        size,
			Annotations: ann,
		})
	}

	// Push the config blob
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal pack config: %w", err)
	}
	configDigest, configSize, err := store.PushBlob(ctx, MediaTypePackConfig, bytes.NewReader(configBytes))
	if err != nil {
		return fmt.Errorf("push config blob: %w", err)
	}

	// Push the manifest
	manifest := Manifest{
		ArtifactType: ArtifactTypePluginPack,
		Config: Descriptor{
			MediaType: MediaTypePackConfig,
			Digest:    configDigest,
			Size:      configSize,
		},
		Layers: layerDescs,
		Annotations: map[string]string{
			annPackProfile:    cfg.Profile,
			annPackJenkinsVer: cfg.JenkinsVersion,
			annPackLockHash:   cfg.LockHash,
		},
	}

	return store.Push(ctx, ref, manifest)
}

// ReadPluginPack reads a plugin pack from the store and returns its config and plugin metadata.
// The returned ResolvedPlugin entries have nil Content.
func ReadPluginPack(ctx context.Context, store BlobStore, ref string) (PackConfig, []ResolvedPlugin, error) {
	manifest, err := store.Pull(ctx, ref)
	if err != nil {
		return PackConfig{}, nil, fmt.Errorf("pull manifest %q: %w", ref, err)
	}

	// Read the config blob
	configRC, err := store.FetchBlob(ctx, manifest.Config.Digest)
	if err != nil {
		return PackConfig{}, nil, fmt.Errorf("fetch config blob %q: %w", manifest.Config.Digest, err)
	}
	defer func() { _ = configRC.Close() }()

	configBytes, err := io.ReadAll(configRC)
	if err != nil {
		return PackConfig{}, nil, fmt.Errorf("read config blob: %w", err)
	}

	var cfg PackConfig
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return PackConfig{}, nil, fmt.Errorf("unmarshal config: %w", err)
	}
	switch cfg.Kind {
	case PackKindProfile, PackKindAddon:
	case "":
		return PackConfig{}, nil, fmt.Errorf("pack %q has no kind: it predates the pack-kind field and must be re-exported", ref)
	default:
		return PackConfig{}, nil, fmt.Errorf("pack %q has unknown kind %q", ref, cfg.Kind)
	}

	// Build plugin metadata from layer annotations. The five metadata
	// annotations are optional; a PRESENT but malformed structured value is an
	// error, because a corrupted dependency list must not read as "no
	// dependencies".
	plugins := make([]ResolvedPlugin, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		p := ResolvedPlugin{
			Name:         l.Annotations[AnnPluginName],
			Version:      l.Annotations[AnnPluginVersion],
			SHA256:       l.Annotations[AnnPluginSHA256],
			UpstreamURL:  l.Annotations[AnnPluginUpstreamURL],
			DisplayName:  l.Annotations[AnnPluginDisplayName],
			Description:  l.Annotations[AnnPluginDescription],
			RequiredCore: l.Annotations[AnnPluginRequiredCore],
			Content:      nil,
		}
		if raw, ok := l.Annotations[AnnPluginTags]; ok {
			if err := json.Unmarshal([]byte(raw), &p.Tags); err != nil {
				return PackConfig{}, nil, fmt.Errorf("plugin %q has malformed %s: %w", p.Name, AnnPluginTags, err)
			}
		}
		if raw, ok := l.Annotations[AnnPluginDependencies]; ok {
			var deps []packDependency
			if err := json.Unmarshal([]byte(raw), &deps); err != nil {
				return PackConfig{}, nil, fmt.Errorf("plugin %q has malformed %s: %w", p.Name, AnnPluginDependencies, err)
			}
			p.Dependencies = make([]hpi.Dependency, len(deps))
			for i, d := range deps {
				p.Dependencies[i] = hpi.Dependency{Name: d.Name, Min: d.Min, Optional: d.Optional}
			}
		}
		plugins = append(plugins, p)
	}

	return cfg, plugins, nil
}

// LockHash computes an order-independent hash of the plugin list.
// It sorts "name@version" lines and returns the sha256 hex of the sorted content.
func LockHash(plugins []ResolvedPlugin) string {
	lines := make([]string, len(plugins))
	for i, p := range plugins {
		lines[i] = p.Name + "@" + p.Version
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", h)
}
