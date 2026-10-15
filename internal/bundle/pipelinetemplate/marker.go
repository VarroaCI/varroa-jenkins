package pipelinetemplate

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarkerPath is the fixed location of the marker file in a consumer
// repository. No factory, item, controller, or chart value changes it.
const MarkerPath = ".varroa/pipeline.yaml"

// MarkerAPIVersion is the required apiVersion of a marker.
const MarkerAPIVersion = "varroa.dev/v1"

// MarkerKind is the required kind of a marker.
const MarkerKind = "Pipeline"

// MaxMarkerSize is the maximum size, in bytes, of a marker file.
const MaxMarkerSize = 64 * 1024

// maxMarkerRead is the most ParseMarkerReader ever reads: one byte past
// MaxMarkerSize, enough to tell an oversized marker from one that exactly
// fills the limit, without loading an unbounded file into memory.
const maxMarkerRead = MaxMarkerSize + 1

// Marker is the parsed content of a .varroa/pipeline.yaml file.
type Marker struct {
	APIVersion      string         `yaml:"apiVersion"`
	Kind            string         `yaml:"kind"`
	Template        string         `yaml:"template"`
	TemplateVersion string         `yaml:"templateVersion,omitempty"`
	Values          map[string]any `yaml:"values,omitempty"`
}

// ParseMarker decodes raw as one marker YAML document. It rejects a file
// over 64 KiB, more than one YAML document, an unknown top-level key, a
// wrong apiVersion or kind, and a missing template.
func ParseMarker(raw []byte) (*Marker, error) {
	if len(raw) > MaxMarkerSize {
		return nil, fmt.Errorf("marker exceeds %d bytes", MaxMarkerSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var m Marker
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode marker: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("decode marker: more than one YAML document")
	}
	if m.APIVersion != MarkerAPIVersion {
		return nil, fmt.Errorf("marker apiVersion must be %q, got %q", MarkerAPIVersion, m.APIVersion)
	}
	if m.Kind != MarkerKind {
		return nil, fmt.Errorf("marker kind must be %q, got %q", MarkerKind, m.Kind)
	}
	if m.Template == "" {
		return nil, fmt.Errorf("marker template is required")
	}
	return &m, nil
}

// ParseMarkerReader reads r and parses the result as one marker, the same
// as ParseMarker. It never reads more than one byte past MaxMarkerSize:
// an oversized marker is rejected without the whole file ever reaching
// memory. Use this over ParseMarker for a caller that streams the marker
// from disk or a network source rather than holding it already decoded.
func ParseMarkerReader(r io.Reader) (*Marker, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxMarkerRead))
	if err != nil {
		return nil, fmt.Errorf("read marker: %w", err)
	}
	return ParseMarker(raw)
}

// Resolve finds, in delivered, the document Resolve's marker names. It is
// ResolveTemplate with the marker's own template name and version
// constraint.
func (m *Marker) Resolve(delivered []Document) (*Document, error) {
	return ResolveTemplate(delivered, m.Template, m.TemplateVersion)
}

// ResolveTemplate finds, in delivered, the PipelineTemplate document named
// name whose version satisfies constraint, selecting the highest matching
// version. It never fetches a template outside delivered: a cell and the
// composer both pass only the set a bundle's pipeline-template inputs
// carry.
func ResolveTemplate(delivered []Document, name, constraint string) (*Document, error) {
	c, err := ParseConstraint(constraint)
	if err != nil {
		return nil, fmt.Errorf("parse version constraint %q: %w", constraint, err)
	}

	var best *Document
	var bestVersion Version
	var deliveredVersions []string
	for i := range delivered {
		d := &delivered[i]
		if d.Metadata.Name != name {
			continue
		}
		v, err := ParseVersion(d.Spec.Version)
		if err != nil {
			continue
		}
		deliveredVersions = append(deliveredVersions, d.Spec.Version)
		if !c.Matches(v) {
			continue
		}
		if best == nil || CompareVersions(v, bestVersion) > 0 {
			best = d
			bestVersion = v
		}
	}
	if len(deliveredVersions) == 0 {
		return nil, fmt.Errorf("template %q is not delivered", name)
	}
	if best == nil {
		return nil, fmt.Errorf("no delivered version of %q matches %q, delivered: %s", name, c.String(), strings.Join(deliveredVersions, ", "))
	}
	return best, nil
}
