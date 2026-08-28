package templates

import (
	"fmt"
	"sync"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// RestartPolicyEnum defines valid restart policy values.
var RestartPolicyEnum = map[string]bool{
	"Always":    true,
	"OnFailure": true,
	"Never":     true,
}

// Catalog is a thread-safe namespace-scoped in-memory template store.
type Catalog struct {
	mu        sync.RWMutex
	templates map[string]map[string]*v1alpha1.PodTemplate // namespace -> name -> template
}

// NewCatalog creates a new template Catalog.
func NewCatalog() *Catalog {
	return &Catalog{
		templates: make(map[string]map[string]*v1alpha1.PodTemplate),
	}
}

// Register adds a template to the catalog under a namespace.
func (c *Catalog) Register(namespace string, tmpl *v1alpha1.PodTemplate) error {
	if err := Validate(tmpl); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.templates[namespace] == nil {
		c.templates[namespace] = make(map[string]*v1alpha1.PodTemplate)
	}
	c.templates[namespace][tmpl.Name] = tmpl
	return nil
}

// Get retrieves a template by namespace and name.
func (c *Catalog) Get(namespace, name string) (*v1alpha1.PodTemplate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ns, ok := c.templates[namespace]
	if !ok {
		return nil, false
	}
	tmpl, ok := ns[name]
	if !ok {
		return nil, false
	}
	cp := *tmpl
	return &cp, true
}

// List returns all templates in a namespace.
func (c *Catalog) List(namespace string) []*v1alpha1.PodTemplate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []*v1alpha1.PodTemplate
	for ns, tmpls := range c.templates {
		if namespace != "" && ns != namespace {
			continue
		}
		for _, t := range tmpls {
			cp := *t
			result = append(result, &cp)
		}
	}
	return result
}

// Delete removes a template from the catalog.
func (c *Catalog) Delete(namespace, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ns, ok := c.templates[namespace]
	if !ok {
		return false
	}
	if _, ok := ns[name]; !ok {
		return false
	}
	delete(ns, name)
	return true
}

// Validate checks that a PodTemplate is valid.
func Validate(tmpl *v1alpha1.PodTemplate) error {
	if len(tmpl.Spec.Containers) == 0 {
		return fmt.Errorf("at least one container is required")
	}
	for i, ctr := range tmpl.Spec.Containers {
		if ctr.Name == "" {
			return fmt.Errorf("container[%d]: name is required", i)
		}
		if ctr.Image == "" {
			return fmt.Errorf("container[%d]: image is required", i)
		}
	}
	if tmpl.Spec.RestartPolicy != "" && !RestartPolicyEnum[tmpl.Spec.RestartPolicy] {
		return fmt.Errorf("invalid restartPolicy %q: must be one of Always, OnFailure, Never", tmpl.Spec.RestartPolicy)
	}
	return nil
}
