package bundle

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// InjectUpdateSites sets jenkins.updateCenter.sites in the Jenkins YAML content
// to a single site with the id "default" and the given url. The operator owns
// the jenkins.updateCenter key, so any bundle-provided value is replaced.
// An empty url writes an empty sites list.
// Returns the modified YAML, whether an existing different value was overridden, and any error.
func InjectUpdateSites(jenkinsYAML, url string) (out string, overrode bool, err error) {
	data := make(map[string]interface{})
	if jenkinsYAML != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &data); err != nil {
			return "", false, fmt.Errorf("unmarshal jenkins yaml: %w", err)
		}
	}

	jenkins, ok := data["jenkins"].(map[string]interface{})
	if !ok {
		jenkins = make(map[string]interface{})
		data["jenkins"] = jenkins
	}

	sites := []interface{}{}
	if url != "" {
		sites = append(sites, map[string]interface{}{"id": "default", "url": url})
	}
	updateCenter := map[string]interface{}{"sites": sites}

	if existing, ok := jenkins["updateCenter"]; ok && !reflect.DeepEqual(existing, updateCenter) {
		overrode = true
	}
	jenkins["updateCenter"] = updateCenter

	raw, err := yaml.Marshal(data)
	if err != nil {
		return "", false, fmt.Errorf("marshal jenkins yaml: %w", err)
	}
	return string(raw), overrode, nil
}
