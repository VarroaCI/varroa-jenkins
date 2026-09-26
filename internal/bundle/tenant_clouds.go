package bundle

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// TenantCloudContainer is one container of a tenant cloud's pod template.
type TenantCloudContainer struct {
	Name                  string
	Image                 string
	ResourceRequestCPU    string
	ResourceRequestMemory string
	ResourceLimitCPU      string
	ResourceLimitMemory   string
	WorkingDir            string
}

// TenantCloudTemplate is one pod template of a tenant cloud.
type TenantCloudTemplate struct {
	Name           string
	Namespace      string
	ServiceAccount string
	Labels         string
	Containers     []TenantCloudContainer
	// Raw holds the pass-through keys of a pod template lifted from a
	// tenant bundle. The renderer writes Raw first, and the typed keys
	// then win, so a tenant key never replaces a stamped key.
	Raw map[string]any
}

// TenantCloud is one kubernetes cloud entry appended to jenkins.clouds for
// a placed tenant.
type TenantCloud struct {
	Name            string
	Namespace       string
	JenkinsURL      string
	CredentialsID   string
	UsageRestricted bool
	WebSocket       bool
	// GarbageCollectionTimeout is the orphan-pod timeout of the kubernetes
	// plugin, in seconds. A value above zero turns on the plugin's own
	// garbage collection for this cloud. Zero leaves it off.
	GarbageCollectionTimeout int
	Templates                []TenantCloudTemplate
}

// InjectTenantClouds appends one kubernetes cloud per tenant to
// jenkins.clouds. It never replaces an existing cloud, and it rejects a
// name collision, whether against an existing cloud or between two tenant
// clouds in the same call. mergeMaps replaces a list rather than
// deep-merging it, so a map merge here would delete the bundle's own
// clouds; this injector appends into the parsed document instead, mirroring
// injectPodTemplatesIntoJCasC.
func InjectTenantClouds(jcascYAML string, clouds []TenantCloud) (string, error) {
	if len(clouds) == 0 {
		return jcascYAML, nil
	}

	var doc map[string]any
	if strings.TrimSpace(jcascYAML) != "" {
		if err := yaml.Unmarshal([]byte(jcascYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for tenant cloud injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	var jenkinsSection map[string]any
	if raw, exists := doc["jenkins"]; exists {
		m, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins\" key is not a map (got %T)", raw)
		}
		jenkinsSection = m
	}
	if jenkinsSection == nil {
		jenkinsSection = map[string]any{}
		doc["jenkins"] = jenkinsSection
	}

	var existing []any
	if raw, exists := jenkinsSection["clouds"]; exists {
		c, ok := raw.([]any)
		if !ok {
			return "", fmt.Errorf("jcasc: \"jenkins.clouds\" is not a list (got %T)", raw)
		}
		existing = c
	}

	seen := make(map[string]bool, len(existing)+len(clouds))
	for _, cloud := range existing {
		cloudMap, ok := cloud.(map[string]any)
		if !ok {
			continue
		}
		k8s, ok := cloudMap["kubernetes"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := k8s["name"].(string); ok {
			seen[name] = true
		}
	}

	for _, tc := range clouds {
		if seen[tc.Name] {
			return "", fmt.Errorf("tenant cloud %q collides with an existing cloud name", tc.Name)
		}
		seen[tc.Name] = true
		existing = append(existing, map[string]any{"kubernetes": tenantCloudMap(tc)})
	}

	jenkinsSection["clouds"] = existing

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after tenant cloud injection: %w", err)
	}
	return string(out), nil
}

// tenantCloudMap renders a TenantCloud into the JCasC kubernetes-cloud
// shape. serverUrl is always empty: every tenant cloud reaches the
// in-cluster API server, never an external one.
func tenantCloudMap(tc TenantCloud) map[string]any {
	m := map[string]any{
		"name":            tc.Name,
		"serverUrl":       "",
		"namespace":       tc.Namespace,
		"jenkinsUrl":      tc.JenkinsURL,
		"credentialsId":   tc.CredentialsID,
		"usageRestricted": tc.UsageRestricted,
		"webSocket":       tc.WebSocket,
	}
	// The block carries no namespaces key: the plugin then scans the
	// cloud's own namespace only, which is the tenant agent namespace.
	if tc.GarbageCollectionTimeout > 0 {
		m["garbageCollection"] = map[string]any{"timeout": tc.GarbageCollectionTimeout}
	}
	if len(tc.Templates) > 0 {
		templates := make([]any, 0, len(tc.Templates))
		for _, t := range tc.Templates {
			templates = append(templates, tenantCloudTemplateMap(t))
		}
		m["templates"] = templates
	}
	return m
}

// tenantCloudTemplateMap renders one pod template. It starts from a
// shallow copy of t.Raw and then writes the typed keys, so a typed key
// always wins over the same key in Raw. It writes containers from the
// typed field only when that field is not empty.
func tenantCloudTemplateMap(t TenantCloudTemplate) map[string]any {
	m := make(map[string]any, len(t.Raw)+5)
	for k, v := range t.Raw {
		m[k] = v
	}
	m["name"] = t.Name
	m["namespace"] = t.Namespace
	m["serviceAccount"] = t.ServiceAccount
	// The kubernetes plugin's PodTemplate attribute is the singular
	// "label"; a "labels" key fails the whole JCasC reload.
	m["label"] = t.Labels
	if len(t.Containers) > 0 {
		containers := make([]any, 0, len(t.Containers))
		for _, c := range t.Containers {
			containers = append(containers, map[string]any{
				"name":                  c.Name,
				"image":                 c.Image,
				"resourceRequestCpu":    c.ResourceRequestCPU,
				"resourceRequestMemory": c.ResourceRequestMemory,
				"resourceLimitCpu":      c.ResourceLimitCPU,
				"resourceLimitMemory":   c.ResourceLimitMemory,
				"workingDir":            c.WorkingDir,
			})
		}
		m["containers"] = containers
	}
	return m
}
