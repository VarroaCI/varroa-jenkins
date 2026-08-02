package profileview

import (
	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

// PluginLinesFromYAML extracts pinned plugin lines from a materialized plugins.yaml.
// Entries preserve ConfigMap order and format as artifactId[@version].
func PluginLinesFromYAML(data string) ([]string, error) {
	var set struct {
		Plugins []pluginlock.PluginEntry `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(data), &set); err != nil {
		return nil, err
	}
	if len(set.Plugins) == 0 {
		return nil, nil
	}
	lines := make([]string, 0, len(set.Plugins))
	for _, pe := range set.Plugins {
		if pe.Version != "" {
			lines = append(lines, pe.ArtifactID+"@"+pe.Version)
		} else {
			lines = append(lines, pe.ArtifactID)
		}
	}
	return lines, nil
}
