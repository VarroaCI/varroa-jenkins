package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// recordingItemLookup implements ItemLookup keyed by namespace/name and records
// every lookup so "not consulted" assertions can be made.
type recordingItemLookup struct {
	items map[string]*v1alpha1.CatalogItem
	calls []string
}

func (r *recordingItemLookup) GetCatalogItemCRD(_ context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	r.calls = append(r.calls, namespace+"/"+name)
	it, ok := r.items[namespace+"/"+name]
	if !ok {
		return nil, nil
	}
	return it, nil
}

func jcascItem(content string) *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		Spec:   v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Content: content, ContentHash: "h-" + content, Valid: true},
	}
}

func explicitRef(name, namespace string) v1alpha1.ComposedInput {
	return v1alpha1.ComposedInput{
		ItemRef: &v1alpha1.ComposedItemRef{Name: name, Namespace: namespace},
	}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// Row 1: explicit ns resolves there and ignores same-named duplicates elsewhere.
func TestComposer_ExplicitNamespace_IgnoresDuplicates(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"team-a/theme": jcascItem("marker:\n  who: bundle-ns\n"),
		"op-ns/theme":  jcascItem("marker:\n  who: operator-ns\n"),
		"team-b/theme": jcascItem("marker:\n  who: team-b\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{explicitRef("theme", "team-b")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Errorf("expected no missing, got %v", result.Missing)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "who: team-b") {
		t.Errorf("expected team-b content, got: %s", result.Materialized.JenkinsYAML)
	}
	if hasCall(r.calls, "team-a/theme") || hasCall(r.calls, "op-ns/theme") {
		t.Errorf("bundle-ns/op-ns must not be consulted for an explicit ref; calls=%v", r.calls)
	}
}

// Row 2: explicit miss is not substituted by a same-named local item.
func TestComposer_ExplicitNamespace_MissNotSubstituted(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"team-a/theme": jcascItem("marker:\n  who: bundle-ns\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{explicitRef("theme", "team-b")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "team-b/theme" {
		t.Errorf("expected Missing [team-b/theme], got %v", result.Missing)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
	if result.Materialized != nil && strings.Contains(result.Materialized.JenkinsYAML, "who: bundle-ns") {
		t.Errorf("local item must NOT be substituted; got: %s", result.Materialized.JenkinsYAML)
	}
}

// Row 1-2 independence from fallback config: explicit ns resolves with empty operator ns.
func TestComposer_ExplicitNamespace_EmptyOperatorNS(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"team-b/theme": jcascItem("marker:\n  who: team-b\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "") // empty operator ns
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{explicitRef("theme", "team-b")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Errorf("expected no missing, got %v", result.Missing)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "who: team-b") {
		t.Errorf("expected team-b content, got: %s", result.Materialized.JenkinsYAML)
	}
}

// Row 3: unset, local hit, no op-ns duplicate → local used, no warning.
func TestComposer_Unset_LocalHit_NoShadow(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"team-a/theme": jcascItem("marker:\n  who: bundle-ns\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{itemRef("theme")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "who: bundle-ns") {
		t.Errorf("expected bundle-ns content, got: %s", result.Materialized.JenkinsYAML)
	}
}

// Row 4: unset, local hit, op-ns duplicate → local used AND a shadow warning naming both.
func TestComposer_Unset_LocalHit_ShadowWarning(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"team-a/theme": jcascItem("marker:\n  who: bundle-ns\n"),
		"op-ns/theme":  jcascItem("marker:\n  who: operator-ns\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{itemRef("theme")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "who: bundle-ns") {
		t.Errorf("expected bundle-ns content (local wins), got: %s", result.Materialized.JenkinsYAML)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected exactly one shadow warning, got %v", result.Warnings)
	}
	w := result.Warnings[0]
	if !strings.Contains(w, "team-a/theme") || !strings.Contains(w, "op-ns/theme") {
		t.Errorf("warning must name both namespaces, got: %s", w)
	}
}

// Row 4 variants: no warning when operator ns is empty or equal to bundle ns.
func TestComposer_Unset_LocalHit_NoWarningWhenOpNSEmptyOrEqual(t *testing.T) {
	items := map[string]*v1alpha1.CatalogItem{
		"team-a/theme": jcascItem("marker:\n  who: bundle-ns\n"),
	}
	// operator ns empty
	r1 := &recordingItemLookup{items: items}
	c1 := NewComposer(r1, nil, "", "", "", "", "")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{itemRef("theme")}}
	res1, err := c1.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(res1.Warnings) != 0 {
		t.Errorf("expected no warning with empty op ns, got %v", res1.Warnings)
	}
	// operator ns equal to bundle ns
	r2 := &recordingItemLookup{items: items}
	c2 := NewComposer(r2, nil, "", "", "", "", "team-a")
	res2, err := c2.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(res2.Warnings) != 0 {
		t.Errorf("expected no warning with op ns == bundle ns, got %v", res2.Warnings)
	}
}

// Row 5: unset, local miss, op-ns hit → op-ns used, no warning (shadow check must not fire).
func TestComposer_Unset_FallbackHit_NoWarning(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{
		"op-ns/theme": jcascItem("marker:\n  who: operator-ns\n"),
	}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{itemRef("theme")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("shadow check must NOT fire on a fallback hit, got %v", result.Warnings)
	}
	if !strings.Contains(result.Materialized.JenkinsYAML, "who: operator-ns") {
		t.Errorf("expected operator-ns content, got: %s", result.Materialized.JenkinsYAML)
	}
}

// Row 6: unset, both miss → Missing bare name, no warning.
func TestComposer_Unset_BothMiss(t *testing.T) {
	r := &recordingItemLookup{items: map[string]*v1alpha1.CatalogItem{}}
	c := NewComposer(r, nil, "", "", "", "", "op-ns")
	spec := &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{itemRef("theme")}}
	result, err := c.Compose(context.Background(), "team-a", spec, nil, nil)
	if err != nil {
		t.Fatalf("Compose error: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "theme" {
		t.Errorf("expected Missing [theme] (bare name), got %v", result.Missing)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
}
