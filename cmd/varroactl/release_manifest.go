package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// releaseManifestFile is the file at the root of a release transfer bundle.
// Its SHA-256 is the digest an operator passes to import release.
const releaseManifestFile = "release-manifest.json"

// releaseManifestKind is the kind field every release manifest carries.
const releaseManifestKind = "varroa-release-bundle"

// releaseManifestSchema is the only schema version this build reads or writes.
const releaseManifestSchema = 1

// mediaTypeReleaseManifest is the layer media type for the manifest when the
// bundle is pushed to a registry.
const mediaTypeReleaseManifest = "application/vnd.varroa.release-manifest.v1+json"

// releaseManifestTag is the registry tag that carries the manifest.
const releaseManifestTag = "release-manifest"

// releaseUnseen names the image sources the export cannot read. Every bundle
// lists them, so an operator knows what to mirror by hand.
var releaseUnseen = []string{
	"Images in Jenkins pipeline scripts and in podTemplate steps.",
	"Pod templates of Tenant and CellPool objects that users create through the API.",
	"The CloudNativePG operator image when a Cluster sets no imageName.",
	"Images a Jenkins plugin pulls at run time.",
}

// releaseManifest is the index of a release transfer bundle. It records what
// the bundle holds and the SHA-256 of every blob, so an import can verify the
// bundle before it pushes anything. It never records the content of a values
// file: only a file name and its SHA-256.
type releaseManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	VarroaVersion string          `json:"varroaVersion"`
	CreatedAt     string          `json:"createdAt"`
	Chart         releaseChart    `json:"chart"`
	Images        []releaseImage  `json:"images"`
	PluginPacks   []releasePack   `json:"pluginPacks"`
	ValuesFiles   []releaseValues `json:"valuesFiles"`
	Blobs         []releaseBlob   `json:"blobs"`
	Unseen        []string        `json:"unseen"`
}

type releaseChart struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	LayoutTag string `json:"layoutTag"`
	Digest    string `json:"digest"`
}

type releaseImage struct {
	// Source is the image reference as the chart, a bundle, or a flag wrote it.
	Source    string `json:"source"`
	LayoutTag string `json:"layoutTag"`
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	// ValuesKeys names every chart value that holds this image. It is empty
	// for an image that a bundle or a flag supplies.
	ValuesKeys []string `json:"valuesKeys,omitempty"`
	// Origin says where the source came from: chart, profile:<name>, bundle,
	// or flag.
	Origin string `json:"origin"`
}

type releasePack struct {
	Profile     string `json:"profile"`
	LayoutTag   string `json:"layoutTag"`
	Digest      string `json:"digest"`
	PluginCount int    `json:"pluginCount"`
}

type releaseValues struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type releaseBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// marshal returns the manifest as indented JSON with a final newline. The
// bytes are the file's content, so the digest of the file is the digest of
// these bytes.
func (m releaseManifest) marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal release manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// writeReleaseManifest writes the manifest into dir and returns its digest.
func writeReleaseManifest(dir string, m releaseManifest) (string, error) {
	b, err := m.marshal()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, releaseManifestFile), b, 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", releaseManifestFile, err)
	}
	digest, _, err := oci.Sha256Digest(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	return digest, nil
}

// readReleaseManifest reads the manifest file in dir. It returns the raw bytes
// so the caller can check their digest before it trusts the content.
func readReleaseManifest(dir string) ([]byte, releaseManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, releaseManifestFile))
	if err != nil {
		return nil, releaseManifest{}, fmt.Errorf("read %s: %w", releaseManifestFile, err)
	}
	var m releaseManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, releaseManifest{}, fmt.Errorf("parse %s: %w", releaseManifestFile, err)
	}
	if m.Kind != releaseManifestKind {
		return nil, releaseManifest{}, fmt.Errorf("%s has kind %q, want %q", releaseManifestFile, m.Kind, releaseManifestKind)
	}
	if m.SchemaVersion != releaseManifestSchema {
		return nil, releaseManifest{}, fmt.Errorf("%s has schemaVersion %d, want %d", releaseManifestFile, m.SchemaVersion, releaseManifestSchema)
	}
	return b, m, nil
}
