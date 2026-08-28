package bundle

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// InjectLocationURL sets unclassified.location.url in the Jenkins YAML content.
// Returns the modified YAML, whether an existing different value was overridden, and any error.
func InjectLocationURL(jenkinsYAML, url string) (out string, overrode bool, err error) {
	data := make(map[string]interface{})
	if jenkinsYAML != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &data); err != nil {
			return "", false, fmt.Errorf("unmarshal jenkins yaml: %w", err)
		}
	}

	unclassified, ok := data["unclassified"].(map[string]interface{})
	if !ok {
		unclassified = make(map[string]interface{})
		data["unclassified"] = unclassified
	}

	location, ok := unclassified["location"].(map[string]interface{})
	if !ok {
		location = make(map[string]interface{})
		unclassified["location"] = location
	}

	if existing, ok := location["url"].(string); ok && existing != url {
		overrode = true
	}
	location["url"] = url

	raw, err := yaml.Marshal(data)
	if err != nil {
		return "", false, fmt.Errorf("marshal jenkins yaml: %w", err)
	}
	return string(raw), overrode, nil
}
