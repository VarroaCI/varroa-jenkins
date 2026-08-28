package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestComposer_OCISource_Merge verifies that an OCISource input with a nil
// resolver still gets past the union guard (the resolver nil check is a
// separate case). We test the union guard and error paths here since full
// OCI materialization is verified in resolver_oci_test.go.
func TestComposer_OCISource_UnionGuard(t *testing.T) {
	f := &fakeItemLookup{}
	c := NewComposer(f, nil, "", "", "", "", "")

	// A single OCISource input should pass the union guard.
	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{OCISource: &v1alpha1.OCIBundleSource{Ref: "myregistry.io/bundle:v1", Path: "."}},
		},
	}
	_, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	// It should fail with resolver=nil (not union guard).
	if err == nil {
		t.Fatal("expected error for OCISource with nil resolver")
	}
	if !strings.Contains(err.Error(), "resolver") {
		t.Errorf("expected resolver-related error, got: %v", err)
	}
}

// TestComposer_OCISource_NeedsResolver guards that a resolver is required.
func TestComposer_OCISource_NeedsResolver(t *testing.T) {
	f := &fakeItemLookup{}
	c := NewComposer(f, nil, "", "", "", "", "")

	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{OCISource: &v1alpha1.OCIBundleSource{Ref: "registry.io/bundle:v1", Path: "."}},
		},
	}
	_, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "OCI source requires a resolver") {
		t.Errorf("expected 'requires a resolver', got: %v", err)
	}
}

// TestComposer_Union_ThreeWayGuard_NeitherSet ensures the three-way guard
// catches an input with none of the three fields set.
func TestComposer_Union_ThreeWayGuard_NeitherSet(t *testing.T) {
	c := NewComposer(&fakeItemLookup{}, nil, "", "", "", "", "")

	_, err := c.Compose(context.Background(), "ns", &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{}},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for input with none of itemRef/gitSource/ociSource set")
	}
	if !strings.Contains(err.Error(), "none set") {
		t.Errorf("expected 'none set' in error, got: %v", err)
	}
}

// TestComposer_Union_ThreeWayGuard_MultipleSet ensures the three-way guard
// catches an input with multiple fields set.
func TestComposer_Union_ThreeWayGuard_MultipleSet(t *testing.T) {
	c := NewComposer(&fakeItemLookup{}, nil, "", "", "", "", "")

	_, err := c.Compose(context.Background(), "ns", &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{
			ItemRef:   &v1alpha1.ComposedItemRef{Name: "x"},
			GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://example.com", Path: "."},
			OCISource: &v1alpha1.OCIBundleSource{Ref: "registry.io/bundle:v1"},
		}},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for input with multiple fields set")
	}
	if !strings.Contains(err.Error(), "multiple set") {
		t.Errorf("expected 'multiple set' in error, got: %v", err)
	}

	// Also test GitSource+OCISource (two fields, not three).
	_, err = c.Compose(context.Background(), "ns", &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{
			GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://example.com", Path: "."},
			OCISource: &v1alpha1.OCIBundleSource{Ref: "registry.io/bundle:v1"},
		}},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for input with both gitSource and ociSource set")
	}
}

// TestComposer_OCISource_MissingAuth guards that a secretRef with missing
// resolvedOCIAuth errors (same as GitSource's missing auth pattern).
func TestComposer_OCISource_MissingAuth(t *testing.T) {
	r := NewResolver(t.TempDir())
	c := NewComposer(&fakeItemLookup{}, r, t.TempDir(), "", "", "", "")

	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{OCISource: &v1alpha1.OCIBundleSource{Ref: "registry.io/bundle:v1", Path: ".", SecretRef: "my-pull-secret"}},
		},
	}
	// Pass nil resolvedOCIAuth — the secretRef means auth is required but
	// the caller didn't resolve it. Compose returns a hard error.
	_, err := c.Compose(context.Background(), "ns", spec, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing OCI auth, got nil")
	}
	if !strings.Contains(err.Error(), "secretRef") {
		t.Errorf("expected error mentioning secretRef, got: %v", err)
	}
}

// TestComposer_OCISource_WithResolverAndAuth verifies that with a real resolver
// and auth, the OCI source path flows through without hard error. The actual
// OCI pull will fail (no registry), but it should error softly into result.Errors.
func TestComposer_OCISource_WithResolverAndAuth(t *testing.T) {
	r := NewResolver(t.TempDir())
	c := NewComposer(&fakeItemLookup{}, r, t.TempDir(), "", "", "", "")

	spec := &v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{OCISource: &v1alpha1.OCIBundleSource{Ref: "localhost:9999/nonexistent:bogus", Path: "."}},
		},
	}
	resolvedOCIAuth := map[int]*OCIAuth{
		0: {Username: "test", Password: "test", Registry: "localhost:9999"},
	}
	result, err := c.Compose(context.Background(), "ns", spec, nil, resolvedOCIAuth)
	if err != nil {
		t.Fatalf("Compose must not hard-fail on OCI pull error: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Error("expected a soft error for OCI pull to nonexistent registry, got none")
	}
}
