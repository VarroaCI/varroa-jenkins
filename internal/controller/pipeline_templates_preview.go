package controller

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// pipelineTemplateSummaries lists the templates a composed bundle
// delivers, in input order, for the preview and validate replies. It
// reads the composer's own "templates" document; an empty or unreadable
// document lists nothing, because the composer already reported any
// error on the bundle itself.
func pipelineTemplateSummaries(templatesYAML string) []bus.PipelineTemplateSummary {
	out := []bus.PipelineTemplateSummary{}
	if strings.TrimSpace(templatesYAML) == "" {
		return out
	}
	var doc struct {
		Templates []pipelinetemplate.Document `yaml:"templates"`
	}
	if err := yaml.Unmarshal([]byte(templatesYAML), &doc); err != nil {
		return out
	}
	for _, d := range doc.Templates {
		out = append(out, bus.PipelineTemplateSummary{
			Name:        d.Metadata.Name,
			Version:     d.Spec.Version,
			DisplayName: d.Metadata.DisplayName,
			Description: d.Metadata.Description,
		})
	}
	return out
}
