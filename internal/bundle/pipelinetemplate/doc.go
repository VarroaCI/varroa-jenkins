// Package pipelinetemplate implements the PipelineTemplate document: its
// structure, its JSON Schema subset, its uiSchema subset, its version
// resolution, and the marker file that selects it.
package pipelinetemplate

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// APIVersion is the required apiVersion of a PipelineTemplate document.
const APIVersion = "varroa.dev/v1"

// Kind is the required kind of a PipelineTemplate document.
const Kind = "PipelineTemplate"

// MaxDocumentSize is the maximum size, in bytes, of one PipelineTemplate
// document.
const MaxDocumentSize = 256 * 1024

// MaxJenkinsfileSize is the maximum size, in bytes, of spec.jenkinsfile.
const MaxJenkinsfileSize = 64 * 1024

// Document is the content of a pipeline-template CatalogItem.
type Document struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata names a PipelineTemplate document. Name is the name a marker
// uses. It is independent of the CatalogItem resource name.
type Metadata struct {
	Name        string `yaml:"name"`
	DisplayName string `yaml:"displayName,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// Spec holds the template's parameter schema and its body. Exactly one of
// Jenkinsfile and SCMDefinition is set.
type Spec struct {
	Version       string         `yaml:"version"`
	Parameters    map[string]any `yaml:"parameters"`
	UISchema      map[string]any `yaml:"uiSchema,omitempty"`
	Jenkinsfile   string         `yaml:"jenkinsfile,omitempty"`
	SCMDefinition *SCMDefinition `yaml:"scmDefinition,omitempty"`
}

// SCMDefinition names a script in the template repository. It never names a
// script in a consumer repository.
type SCMDefinition struct {
	Remote        string `yaml:"remote"`
	CredentialsID string `yaml:"credentialsId,omitempty"`
	ScriptPath    string `yaml:"scriptPath"`
	Revision      string `yaml:"revision,omitempty"`
}

// ParseDocument decodes raw as one PipelineTemplate YAML document. It
// rejects an unknown key at any level of the document and rejects more than
// one YAML document in raw.
func ParseDocument(raw []byte) (*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode pipeline template document: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("decode pipeline template document: more than one YAML document")
	}
	return &doc, nil
}
