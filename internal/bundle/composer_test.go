package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// fakeItemLookup implements ItemLookup with a static map namespace/name → item.
type fakeItemLookup struct {
	items map[string]*v1alpha1.CatalogItem
}

func (f *fakeItemLookup) GetCatalogItemCRD(_ context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	it, ok := f.items[namespace+"/"+name]
	if !ok {
		return nil, nil
	}
	return it, nil
}

// addItem stores an item keyed by (namespace, name).
func (f *fakeItemLookup) addItem(namespace, name string, item *v1alpha1.CatalogItem) {
	if f.items == nil {
		f.items = make(map[string]*v1alpha1.CatalogItem)
	}
	f.items[namespace+"/"+name] = item
}

func itemRef(name string) v1alpha1.ComposedInput {
	return v1alpha1.ComposedInput{
		ItemRef: &v1alpha1.ComposedItemRef{Name: name},
	}
}

func TestComposer_JcascMerge(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "security:\n  realm: oic\n", ContentHash: "a1", Valid: true}},
		"ns/jcasc-2": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "unclassified:\n  url: https://example.com\n", ContentHash: "a2", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			itemRef("jcasc-1"),
			itemRef("jcasc-2"),
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) > 0 {
		t.Errorf("unexpected missing items: %v", result.Missing)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "realm: oic") {
		t.Error("expected jenkinsYAML to contain realm: oic")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "url: https://example.com") {
		t.Error("expected jenkinsYAML to contain url: https://example.com")
	}
}

func TestComposer_MergeConflict(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "security:\n  realm: oic\n", ContentHash: "a1", Valid: true}},
		"ns/jcasc-2": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "security:\n  realm: saml\n", ContentHash: "a2", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			itemRef("jcasc-1"),
			itemRef("jcasc-2"),
		},
	}
	// Default strategy is errorOnConflict, so duplicate "security" key should fail.
	_, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err == nil {
		t.Error("expected merge conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("expected 'merge' in error, got: %v", err)
	}
}

func TestComposer_MergeOverride(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "security:\n  realm: oic\n", ContentHash: "a1", Valid: true}},
		"ns/jcasc-2": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "security:\n  realm: saml\n", ContentHash: "a2", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		JcascMergeStrategy: "override",
		Inputs: []v1alpha1.ComposedInput{
			itemRef("jcasc-1"),
			itemRef("jcasc-2"),
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error with override: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	// Last wins under override: should be saml, not oic.
	if !strings.Contains(result.Materialized.JenkinsYAML, "realm: saml") {
		t.Errorf("expected last-wins (saml), got: %s", result.Materialized.JenkinsYAML)
	}
	if strings.Contains(result.Materialized.JenkinsYAML, "realm: oic") {
		t.Errorf("expected oic to be overridden, got: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_PodTemplateWrap(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/pod-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: maven\n  image: maven:3.9\n", ContentHash: "p1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("pod-1")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose podtemplate error: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	// Pod templates must be wrapped under jenkins.clouds[].kubernetes.templates.
	if !strings.Contains(result.Materialized.JenkinsYAML, "jenkins:") {
		t.Error("expected jenkins key in output")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "clouds:") {
		t.Error("expected clouds key in output")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "kubernetes:") {
		t.Error("expected kubernetes key in output")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "templates:") {
		t.Error("expected templates key in output")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "name: maven") {
		t.Errorf("expected podtemplate content, got: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_MultiplePodTemplates(t *testing.T) {
	// Multiple podtemplate items must merge into a single
	// jenkins.clouds[].kubernetes.templates list rather than colliding on the
	// "clouds" key during JCasC merge.
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/pod-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: maven\n  image: maven:3.9\n", ContentHash: "p1", Valid: true}},
		"ns/pod-2": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: node\n  image: node:20\n", ContentHash: "p2", Valid: true}},
	}}
	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("pod-1"), itemRef("pod-2")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose with two podtemplates errored: %v", err)
	}
	// Both templates must survive in a single document.
	if !strings.Contains(result.Materialized.JenkinsYAML, "name: maven") {
		t.Errorf("expected first podtemplate, got: %s", result.Materialized.JenkinsYAML)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "name: node") {
		t.Errorf("expected second podtemplate, got: %s", result.Materialized.JenkinsYAML)
	}
	// There must be exactly one clouds key (single merged document).
	if n := strings.Count(result.Materialized.JenkinsYAML, "clouds:"); n != 1 {
		t.Errorf("expected exactly one clouds key, got %d:\n%s", n, result.Materialized.JenkinsYAML)
	}
}

func TestComposer_PodTemplateWithJCasCCloud(t *testing.T) {
	// Issue #221: a jcasc item defining a full kubernetes cloud (name/namespace/
	// jenkinsUrl) must survive when a podtemplate item is composed alongside it.
	// Previously the podtemplate wrapper's bare "clouds" list clobbered the
	// jcasc item's cloud config during merge.
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-cloud": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{
			Content:     "jenkins:\n  clouds:\n  - name: kubernetes\n    kubernetes:\n      namespace: jenkins\n      jenkinsUrl: http://jenkins.jenkins.svc:8080\n",
			ContentHash: "c1", Valid: true,
		}},
		"ns/pod-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: dind\n  image: docker:dind\n", ContentHash: "p1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("jcasc-cloud"), itemRef("pod-1")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	yaml := result.Materialized.JenkinsYAML
	if !strings.Contains(yaml, "name: kubernetes") {
		t.Errorf("expected cloud name to survive, got: %s", yaml)
	}
	if !strings.Contains(yaml, "namespace: jenkins") {
		t.Errorf("expected cloud namespace to survive, got: %s", yaml)
	}
	if !strings.Contains(yaml, "jenkinsUrl:") {
		t.Errorf("expected cloud jenkinsUrl to survive, got: %s", yaml)
	}
	if !strings.Contains(yaml, "name: dind") {
		t.Errorf("expected pod template to be injected, got: %s", yaml)
	}
	if n := strings.Count(yaml, "clouds:"); n != 1 {
		t.Errorf("expected exactly one clouds key, got %d:\n%s", n, yaml)
	}
}

func TestComposer_PodTemplateWithJCasCCloudAndExistingTemplates(t *testing.T) {
	// A jcasc-defined kubernetes cloud that already has a template must keep
	// it when a podtemplate catalog item injects another.
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-cloud": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{
			Content:     "jenkins:\n  clouds:\n  - name: kubernetes\n    kubernetes:\n      namespace: jenkins\n      templates:\n      - name: base\n        image: jenkins/inbound-agent\n",
			ContentHash: "c1", Valid: true,
		}},
		"ns/pod-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: dind\n  image: docker:dind\n", ContentHash: "p1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("jcasc-cloud"), itemRef("pod-1")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	yaml := result.Materialized.JenkinsYAML
	if !strings.Contains(yaml, "name: base") {
		t.Errorf("expected existing template to survive, got: %s", yaml)
	}
	if !strings.Contains(yaml, "name: dind") {
		t.Errorf("expected injected template, got: %s", yaml)
	}
}

func TestComposer_PodTemplateWithMultipleKubernetesClouds(t *testing.T) {
	// Pod-template catalog items carry no target-cloud reference, so when a
	// bundle defines more than one kubernetes cloud, the injected templates
	// must be added to all of them rather than silently dropped from extras.
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jcasc-clouds": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{
			Content:     "jenkins:\n  clouds:\n  - name: cloud-a\n    kubernetes:\n      namespace: ns-a\n  - name: cloud-b\n    kubernetes:\n      namespace: ns-b\n",
			ContentHash: "c1", Valid: true,
		}},
		"ns/pod-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPodTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "- name: dind\n  image: docker:dind\n", ContentHash: "p1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("jcasc-clouds"), itemRef("pod-1")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	yaml := result.Materialized.JenkinsYAML
	if !strings.Contains(yaml, "name: cloud-a") || !strings.Contains(yaml, "name: cloud-b") {
		t.Errorf("expected both cloud names to survive, got: %s", yaml)
	}
	if n := strings.Count(yaml, "name: dind"); n != 2 {
		t.Errorf("expected pod template injected into both clouds (2 occurrences), got %d:\n%s", n, yaml)
	}
}

func TestComposer_VariablePrecedence(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/item-1": {Spec: v1alpha1.CatalogItemSpec{
			Type: v1alpha1.CatalogItemJCasC,
			Variables: []v1alpha1.CatalogVariable{
				{Name: "namespace", Default: "default-val"},
				{Name: "replicas", Default: "1"},
			},
		}, Status: v1alpha1.CatalogItemStatus{Content: "namespace: ${namespace}\nreplicas: ${replicas}\n", ContentHash: "v1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Variables: map[string]string{"namespace": "spec-val"},
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "item-1", Variables: map[string]string{"replicas": "5"}}},
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	// Content is unresolved — ${var} placeholders remain.
	if !strings.Contains(result.Materialized.JenkinsYAML, "${namespace}") {
		t.Errorf("expected unresolved ${namespace}, got: %s", result.Materialized.JenkinsYAML)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "${replicas}") {
		t.Errorf("expected unresolved ${replicas}, got: %s", result.Materialized.JenkinsYAML)
	}
	// spec.Variables overrides item defaults → namespace=spec-val.
	if result.Materialized.Variables["namespace"] != "spec-val" {
		t.Errorf("expected namespace=spec-val, got: %s", result.Materialized.Variables["namespace"])
	}
	// ref.Variables overrides everything else → replicas=5.
	if result.Materialized.Variables["replicas"] != "5" {
		t.Errorf("expected replicas=5, got: %s", result.Materialized.Variables["replicas"])
	}
	// varroa_* variables are NOT set (injected at controller resolve time).
	if result.Materialized.Variables["varroa_controller_name"] != "" {
		t.Errorf("expected no varroa_controller_name in materialized output, got: %s", result.Materialized.Variables["varroa_controller_name"])
	}
}

// TestComposer_PipelineTemplateReachesItemsYAML is a regression test for the
// silent-drop bug found during explore: Compose's groups map is a hardcoded
// 5-key map, so a pipeline-template item's content would be silently dropped
// from every composed bundle without the groupKey alias in Compose.
func TestComposer_PipelineTemplateReachesItemsYAML(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/pt-1":   {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPipelineTemplate}, Status: v1alpha1.CatalogItemStatus{Content: "items:\n- name: templated-job\n  kind: pipeline\n", ContentHash: "pt1", Valid: true}},
		"ns/item-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemItem}, Status: v1alpha1.CatalogItemStatus{Content: "items:\n- name: plain-job\n", ContentHash: "i1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("pt-1"), itemRef("item-1")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	if !strings.Contains(result.Materialized.ItemsYAML, "templated-job") {
		t.Errorf("expected pipeline-template content to reach ItemsYAML (silent-drop regression), got: %s", result.Materialized.ItemsYAML)
	}
	if !strings.Contains(result.Materialized.ItemsYAML, "plain-job") {
		t.Errorf("expected plain item content to still reach ItemsYAML alongside it, got: %s", result.Materialized.ItemsYAML)
	}
}

// TestComposer_PipelineTemplateVariablePrecedence proves a pipeline-template
// item's declared variables participate in the normal item-default <
// spec-wide < per-ref precedence chain, identically to plain item-type
// content.
func TestComposer_PipelineTemplateVariablePrecedence(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/pt-1": {Spec: v1alpha1.CatalogItemSpec{
			Type: v1alpha1.CatalogItemPipelineTemplate,
			Variables: []v1alpha1.CatalogVariable{
				{Name: "environment", Default: "default-val"},
			},
		}, Status: v1alpha1.CatalogItemStatus{Content: "items:\n- name: templated-job\n  kind: pipeline\n  env: ${environment}\n", ContentHash: "pt1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Variables: map[string]string{"environment": "spec-val"},
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "pt-1", Variables: map[string]string{"environment": "ref-val"}}},
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	// ref.Variables overrides everything else → environment=ref-val.
	if result.Materialized.Variables["environment"] != "ref-val" {
		t.Errorf("expected environment=ref-val (per-ref precedence), got: %s", result.Materialized.Variables["environment"])
	}
}

func TestComposer_PluginDedup(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/plug-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin}, Status: v1alpha1.CatalogItemStatus{Content: "plugins:\n- artifactId: git\n  version: \"5.0\"\n- artifactId: workflow\n  version: \"1.0\"\n", ContentHash: "pl1", Valid: true}},
		"ns/plug-2": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin}, Status: v1alpha1.CatalogItemStatus{Content: "plugins:\n- artifactId: git\n  version: \"6.0\"\n", ContentHash: "pl2", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("plug-1"), itemRef("plug-2")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	// Last wins: git should be 6.0.
	if !strings.Contains(result.Materialized.PluginsYAML, "version: \"6.0\"") {
		t.Errorf("expected git version 6.0 (last wins), got: %s", result.Materialized.PluginsYAML)
	}
	// workflow should still be present.
	if !strings.Contains(result.Materialized.PluginsYAML, "workflow") {
		t.Errorf("expected workflow plugin, got: %s", result.Materialized.PluginsYAML)
	}
	// Version conflict warning.
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "git") && strings.Contains(w, "conflict") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected version conflict warning for git, got warnings: %v", result.Warnings)
	}
}

// A jcasc item may embed a top-level plugins: block (e.g. the varroa-theme
// item bundling simple-theme-plugin, #263). The plugin must land in the plugin
// set and must NOT leak into jenkins.yaml; the rest of the jcasc config stays.
func TestComposer_JcascEmbeddedPlugins(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/theme": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{
			Content:     "plugins:\n- artifactId: simple-theme-plugin\n  version: \"230.v8b\"\nappearance:\n  simpleTheme:\n    elements:\n    - cssUrl:\n        url: https://example.com/t.css\n",
			ContentHash: "th1", Valid: true,
		}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("theme")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	// Plugin routed to the plugin set.
	if !strings.Contains(result.Materialized.PluginsYAML, "simple-theme-plugin") {
		t.Errorf("expected simple-theme-plugin in plugins set, got: %s", result.Materialized.PluginsYAML)
	}
	// Appearance config preserved in jenkins.yaml.
	if !strings.Contains(result.Materialized.JenkinsYAML, "simpleTheme") {
		t.Errorf("expected simpleTheme config in jenkinsYAML, got: %s", result.Materialized.JenkinsYAML)
	}
	// The plugins key must NOT leak into jenkins.yaml.
	if strings.Contains(result.Materialized.JenkinsYAML, "simple-theme-plugin") {
		t.Errorf("plugins block leaked into jenkinsYAML: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_PinnedHashDrift(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/item-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "key: val\n", ContentHash: "abc123", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "item-1", PinnedContentHash: "def456"}},
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Drifted) != 1 || result.Drifted[0] != "item-1" {
		t.Errorf("expected item-1 to be drifted (pinned def456, actual abc123), got drifted=%v", result.Drifted)
	}
	// Content is still composed even when drifted.
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle even when drifted")
	}
}

func TestComposer_MissingItem(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("nonexistent")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "nonexistent" {
		t.Errorf("expected nonexistent in missing, got: %v", result.Missing)
	}
}

func TestComposer_ResolvedHash(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jc-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "key: val\n", ContentHash: "a1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("jc-1")},
	}
	result1, _ := c.Compose(context.Background(), "ns", spec, nil, nil)
	result2, _ := c.Compose(context.Background(), "ns", spec, nil, nil)

	// Same input → same hash.
	if result1.ResolvedHash != result2.ResolvedHash {
		t.Errorf("expected stable hash, got %s vs %s", result1.ResolvedHash, result2.ResolvedHash)
	}
	if result1.ResolvedHash == "" {
		t.Error("expected non-empty resolved hash")
	}
}

func TestComposer_AllFiveSections(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/jc-1":   {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC}, Status: v1alpha1.CatalogItemStatus{Content: "key: val\n", ContentHash: "j1", Valid: true}},
		"ns/plug-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin}, Status: v1alpha1.CatalogItemStatus{Content: "plugins:\n- artifactId: git\n  version: \"1.0\"\n", ContentHash: "p1", Valid: true}},
		"ns/item-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemItem}, Status: v1alpha1.CatalogItemStatus{Content: "items:\n- name: test-job\n", ContentHash: "i1", Valid: true}},
		"ns/rbac-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemRBAC}, Status: v1alpha1.CatalogItemStatus{Content: "roles:\n  admin:\n    permissions:\n    - Overall/Administer\n", ContentHash: "r1", Valid: true}},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			itemRef("jc-1"), itemRef("plug-1"), itemRef("item-1"), itemRef("rbac-1"),
		},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "key: val") {
		t.Error("missing jcasc content")
	}
	if !strings.Contains(result.Materialized.PluginsYAML, "git") {
		t.Error("missing plugins content")
	}
	if !strings.Contains(result.Materialized.ItemsYAML, "test-job") {
		t.Error("missing items content")
	}
	if !strings.Contains(result.Materialized.RbacYAML, "Overall/Administer") {
		t.Error("missing rbac content")
	}
	if result.BundleYAML == "" {
		t.Error("bundleYAML should not be empty")
	}
}

func TestComposer_UnionValidation(t *testing.T) {
	c := NewComposer(&fakeItemLookup{}, nil, "", "", "", "", "")

	// Neither set.
	_, err := c.Compose(context.Background(), "ns", &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{}},
	}, nil, nil)
	if err == nil {
		t.Error("expected error for input with neither itemRef nor gitSource")
	}

	// Both set.
	_, err = c.Compose(context.Background(), "ns", &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{
			ItemRef:   &v1alpha1.ComposedItemRef{Name: "x"},
			GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://example.com", Path: "."},
		}},
	}, nil, nil)
	if err == nil {
		t.Error("expected error for input with both itemRef and gitSource")
	}
}

func TestComposeItems_MalformedFragment(t *testing.T) {
	yamls := []string{
		"items:\n- name: good-job\n",
		"this is not valid yaml {{",
		"items:\n- name: another-job\n",
	}
	result, err := composeItems(yamls)
	if err == nil {
		t.Fatal("expected error for malformed fragment")
	}
	if !strings.Contains(err.Error(), "input 1") {
		t.Errorf("expected error to name input 1, got: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result on parse error, got: %s", result)
	}
}

func TestComposeItems_AllValid(t *testing.T) {
	yamls := []string{
		"items:\n- name: job-a\n",
		"items:\n- name: job-b\n",
	}
	result, err := composeItems(yamls)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "job-a") {
		t.Error("expected job-a in result")
	}
	if !strings.Contains(result, "job-b") {
		t.Error("expected job-b in result")
	}
}

func TestComposeRbac_MalformedFragment(t *testing.T) {
	yamls := []string{
		"roles:\n  admin:\n    permissions:\n    - Overall/Administer\n",
		"bad yaml {{{",
	}
	result, err := composeRbac(yamls)
	if err == nil {
		t.Fatal("expected error for malformed RBAC fragment")
	}
	if !strings.Contains(err.Error(), "input 1") {
		t.Errorf("expected error to name input 1, got: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result on parse error, got: %s", result)
	}
}

func TestComposeRbac_AllValid(t *testing.T) {
	yamls := []string{
		"roles:\n  admin:\n    permissions:\n    - Overall/Administer\n",
	}
	result, err := composeRbac(yamls)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Overall/Administer") {
		t.Error("expected admin role in result")
	}
}

func TestComposer_ComposeErrorsSurfaced(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/item-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemItem}, Status: v1alpha1.CatalogItemStatus{Content: "bad yaml {{{", ContentHash: "i1", Valid: true}},
		"ns/rbac-1": {Spec: v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemRBAC}, Status: v1alpha1.CatalogItemStatus{Content: "also bad {{{", ContentHash: "r1", Valid: true}},
	}}
	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("item-1"), itemRef("rbac-1")},
	}
	_, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err == nil {
		t.Fatal("expected Compose to return an error for malformed input")
	}
	if !strings.Contains(err.Error(), "compose items") {
		t.Errorf("expected error about items, got: %v", err)
	}
}

// TestComposer_GitInputMissingWorkDir guards the regression where a git-input
// compose hard-failed when the composer work dir did not yet exist (e.g. on an
// ephemeral /tmp after an operator restart). Compose must create the work dir
// rather than abort. The git clone itself fails softly (bogus repo) into
// result.Errors, so Compose returns no hard error.
func TestComposer_GitInputMissingWorkDir(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: workDir should not exist yet, stat err=%v", err)
	}

	c := NewComposer(&fakeItemLookup{}, NewResolver(t.TempDir()), workDir, "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{GitSource: &v1alpha1.GitBundleSource{
				RepoURL:  "file:///nonexistent-varroa-repo",
				Path:     ".",
				Revision: "main",
			}},
		},
	}

	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose must not hard-fail on a missing work dir: %v", err)
	}
	if _, statErr := os.Stat(workDir); statErr != nil {
		t.Fatalf("expected work dir to be created, stat err=%v", statErr)
	}
	if len(result.Errors) == 0 {
		t.Error("expected a soft git error for the bogus repo, got none")
	}
}

// --- ItemRef resolution fallback tests ---

func TestComposer_ItemRef_LocalHit(t *testing.T) {
	// (a) item only in bundle ns → resolved, operator ns not consulted.
	f := &fakeItemLookup{}
	f.addItem("bundle-ns", "my-item", &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: "key: local\n", ContentHash: "abc", Valid: true},
	})
	c := NewComposer(f, nil, "", "", "", "", "operator-ns")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("my-item")},
	}
	result, err := c.Compose(context.Background(), "bundle-ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) > 0 {
		t.Errorf("unexpected missing items: %v", result.Missing)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "local") {
		t.Errorf("expected local item content, got: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_ItemRef_OperatorFallback(t *testing.T) {
	// (b) item only in operator ns → resolved via fallback.
	f := &fakeItemLookup{}
	f.addItem("operator-ns", "platform-item", &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: "key: operator\n", ContentHash: "def", Valid: true},
	})
	// No item in bundle ns.
	c := NewComposer(f, nil, "", "", "", "", "operator-ns")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("platform-item")},
	}
	result, err := c.Compose(context.Background(), "bundle-ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) > 0 {
		t.Errorf("unexpected missing items: %v", result.Missing)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "operator") {
		t.Errorf("expected operator item content, got: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_ItemRef_LocalWinsOverOperator(t *testing.T) {
	// (c) same name in both → bundle-ns item wins.
	f := &fakeItemLookup{}
	f.addItem("bundle-ns", "shared-item", &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: "key: local\n", ContentHash: "local", Valid: true},
	})
	f.addItem("operator-ns", "shared-item", &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: "key: operator\n", ContentHash: "operator", Valid: true},
	})
	c := NewComposer(f, nil, "", "", "", "", "operator-ns")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("shared-item")},
	}
	result, err := c.Compose(context.Background(), "bundle-ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) > 0 {
		t.Errorf("unexpected missing items: %v", result.Missing)
	}
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle")
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "local") {
		t.Errorf("expected local item content (local wins), got: %s", result.Materialized.JenkinsYAML)
	}
}

func TestComposer_ItemRef_BothMiss(t *testing.T) {
	// (d) absent from both → name in result.Missing.
	f := &fakeItemLookup{}
	c := NewComposer(f, nil, "", "", "", "", "operator-ns")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("nowhere-item")},
	}
	result, err := c.Compose(context.Background(), "bundle-ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "nowhere-item" {
		t.Errorf("expected 'nowhere-item' in Missing, got: %v", result.Missing)
	}
}

func TestComposer_ItemRef_NoOperatorNamespace(t *testing.T) {
	// (e) operatorNamespace=="" + local miss → Missing, no fallback.
	f := &fakeItemLookup{}
	f.addItem("operator-ns", "only-op-item", &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: "key: op\n", ContentHash: "op", Valid: true},
	})
	c := NewComposer(f, nil, "", "", "", "", "") // empty operatorNamespace
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("only-op-item")},
	}
	result, err := c.Compose(context.Background(), "bundle-ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "only-op-item" {
		t.Errorf("expected 'only-op-item' in Missing (no fallback), got: %v", result.Missing)
	}
}

// TestComposer_RejectsGroovyItemRefAsInput verifies that a ComposedBundle
// input referencing a groovy-typed CatalogItem is added to result.Errors
// and its content does not reach any composed output group.
func TestComposer_RejectsGroovyItemRefAsInput(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/groovy-item": {
			Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
			Status: v1alpha1.CatalogItemStatus{Content: "println 'hello'", ContentHash: "g1", Valid: true},
		},
		"ns/jcasc-item": {
			Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
			Status: v1alpha1.CatalogItemStatus{Content: "key: val\n", ContentHash: "j1", Valid: true},
		},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("groovy-item"), itemRef("jcasc-item")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}

	// Must have an error describing the groovy rejection.
	var found bool
	for _, e := range result.Errors {
		if strings.Contains(e, "groovy-item") && strings.Contains(e, "brood-operation-only") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error about groovy-item being brood-operation-only, got errors: %v", result.Errors)
	}

	// The groovy item's content must NOT appear in any output.
	if result.Materialized == nil {
		t.Fatal("expected Materialized bundle, got nil")
	}
	if strings.Contains(result.Materialized.JenkinsYAML, "println") {
		t.Error("groovy content leaked into JenkinsYAML")
	}
	if strings.Contains(result.Materialized.ItemsYAML, "println") {
		t.Error("groovy content leaked into ItemsYAML")
	}
	if strings.Contains(result.Materialized.PluginsYAML, "println") {
		t.Error("groovy content leaked into PluginsYAML")
	}
	if strings.Contains(result.Materialized.RbacYAML, "println") {
		t.Error("groovy content leaked into RbacYAML")
	}

	// The jcasc item should still compose normally.
	if !strings.Contains(result.Materialized.JenkinsYAML, "key: val") {
		t.Error("expected jcasc content to still be present")
	}
}

// TestComposer_RejectsInvalidItemRefAsInput pins that an itemRef to an
// invalid catalog item fails the compose loudly instead of being silently
// omitted (invalid items store no content — without the gate the bundle
// would go Ready with the input missing).
func TestComposer_RejectsInvalidItemRefAsInput(t *testing.T) {
	f := &fakeItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"ns/bad-item": {
			Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
			Status: v1alpha1.CatalogItemStatus{Valid: false, Message: "items: yaml unmarshal error"},
		},
		"ns/good-item": {
			Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
			Status: v1alpha1.CatalogItemStatus{Content: "key: val\n", ContentHash: "g1", Valid: true},
		},
	}}

	c := NewComposer(f, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{itemRef("bad-item"), itemRef("good-item")},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}

	var found bool
	for _, e := range result.Errors {
		if strings.Contains(e, "bad-item") && strings.Contains(e, "is invalid and cannot be composed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error about bad-item being invalid, got errors: %v", result.Errors)
	}

	// The valid item still composes.
	if result.Materialized == nil || !strings.Contains(result.Materialized.JenkinsYAML, "key: val") {
		t.Errorf("good-item content missing from composed output")
	}
}
