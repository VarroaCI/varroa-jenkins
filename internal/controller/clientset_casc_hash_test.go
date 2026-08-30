package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func newFakeStsClient() (*ClientsetClient, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
	)
	return &ClientsetClient{dynamic: dyn}, dyn
}

func baseCascHashSpec(name, cascHash string) StatefulSetSpec {
	return StatefulSetSpec{
		Name:            name,
		Namespace:       "ns",
		ControllerName:  "test",
		JenkinsImage:    "jenkins:lts",
		MiteImage:       "mite:latest",
		StorageSize:     "10Gi",
		OIDCIssuer:      "https://oidc.example.com",
		VarroaLoginURL:  "https://login.example.com",
		CascContentHash: cascHash,
	}
}

func podTemplateAnnotation(t *testing.T, got *unstructured.Unstructured, key string) (string, bool) {
	t.Helper()
	anns, _, err := unstructured.NestedStringMap(got.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("read pod template annotations: %v", err)
	}
	v, ok := anns[key]
	return v, ok
}

// TestCreateStatefulSet_CascHashAnnotation_Deterministic verifies that
// applying the same CascContentHash twice (create, then an update
// CreateOrUpdate pass) produces an identical pod-template annotation both
// times.
func TestCreateStatefulSet_CascHashAnnotation_Deterministic(t *testing.T) {
	c, dyn := newFakeStsClient()
	spec := baseCascHashSpec("test-casc-det", "deadbeef")

	if err := c.CreateStatefulSet(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "test-casc-det", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	first, ok := podTemplateAnnotation(t, got, cascContentHashAnnotation)
	if !ok || first != "deadbeef" {
		t.Fatalf("first annotation = %q, ok=%v, want deadbeef", first, ok)
	}

	// Re-apply the same spec (CreateOrUpdate semantics: Create returns
	// AlreadyExists and CreateStatefulSet falls back to Update).
	if err := c.CreateStatefulSet(context.Background(), spec); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "test-casc-det", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	second, ok := podTemplateAnnotation(t, got, cascContentHashAnnotation)
	if !ok || second != first {
		t.Fatalf("second annotation = %q, ok=%v, want %q (identical content must produce an identical annotation)", second, ok, first)
	}
}

// TestCreateStatefulSet_CascHashAnnotation_ChangesWithContent verifies that
// a changed CascContentHash produces a changed pod-template annotation on
// the update path, so a StatefulSet controller sees a template diff and
// rolls the pod.
func TestCreateStatefulSet_CascHashAnnotation_ChangesWithContent(t *testing.T) {
	c, dyn := newFakeStsClient()
	name := "test-casc-change"

	if err := c.CreateStatefulSet(context.Background(), baseCascHashSpec(name, "hash-v1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.CreateStatefulSet(context.Background(), baseCascHashSpec(name, "hash-v2")); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	v, ok := podTemplateAnnotation(t, got, cascContentHashAnnotation)
	if !ok || v != "hash-v2" {
		t.Fatalf("annotation = %q, ok=%v, want hash-v2 (changed content must produce a changed annotation)", v, ok)
	}
}

// TestCreateStatefulSet_CascHashAnnotation_SurvivesOverlays verifies that
// the CASC content hash annotation is present after PodOverrides and
// ResourceOverlay are merged, and that it does not clobber a pod-template
// annotation the overlay itself sets.
func TestCreateStatefulSet_CascHashAnnotation_SurvivesOverlays(t *testing.T) {
	c, dyn := newFakeStsClient()
	spec := baseCascHashSpec("test-casc-overlay", "overlay-hash")
	spec.PodOverrides = &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "MY_OVERLAY_VAR", Value: "overlay-val"},
		},
	}
	spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
		StatefulSet: `
spec:
  template:
    metadata:
      annotations:
        user.example.com/custom: "kept"
`,
	}

	if err := c.CreateStatefulSet(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "test-casc-overlay", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	hashVal, ok := podTemplateAnnotation(t, got, cascContentHashAnnotation)
	if !ok || hashVal != "overlay-hash" {
		t.Fatalf("casc-content-hash annotation = %q, ok=%v, want overlay-hash even with PodOverrides/ResourceOverlay applied", hashVal, ok)
	}
	userVal, ok := podTemplateAnnotation(t, got, "user.example.com/custom")
	if !ok || userVal != "kept" {
		t.Fatalf("overlay-set annotation = %q, ok=%v, want kept (must not be clobbered by the operator's own stamp)", userVal, ok)
	}
}
