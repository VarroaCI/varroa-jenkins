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

// stsGVR is the GroupVersionResource for StatefulSets (apps/v1).
var stsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}

// TestCreateStatefulSetWithOverlays verifies that PodOverrides and
// ResourceOverlay.StatefulSet are merged onto the base StatefulSet before
// creation, and that the merged result is persisted.
func TestCreateStatefulSetWithOverlays(t *testing.T) {
	scheme := runtime.NewScheme()
	// Register unstructured types for the fake dynamic client.
	stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
	)
	c := &ClientsetClient{dynamic: dyn}

	spec := StatefulSetSpec{
		Name:           "test-ov",
		Namespace:      "ns",
		ControllerName: "test",
		JenkinsImage:   "jenkins:lts",
		MiteImage:      "mite:latest",
		StorageSize:    "10Gi",
		Resources:      nil,
		OIDCIssuer:     "https://oidc.example.com",
		VarroaLoginURL: "https://login.example.com",
		PodOverrides: &v1alpha1.PodOverrides{
			Env: []corev1.EnvVar{
				{Name: "MY_OVERLAY_VAR", Value: "overlay-val"},
			},
		},
		ResourceOverlay: &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      volumes:
        - name: user-custom-vol
          emptyDir: {}
`,
		},
	}

	t.Logf("PodOverrides.Env = %+v", spec.PodOverrides.Env)
	t.Logf("ResourceOverlay.StatefulSet = %q", spec.ResourceOverlay.StatefulSet)

	err := c.CreateStatefulSet(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateStatefulSet failed: %v", err)
	}

	// Fetch the created StatefulSet back from the fake dynamic client.
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "test-ov", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created statefulset: %v", err)
	}

	// Verify the jenkins container has the user env var AND original ones.
	containers, found, err := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		t.Fatalf("get containers: found=%v err=%v", found, err)
	}

	var jenkins map[string]interface{}
	var mite map[string]interface{}
	for _, c := range containers {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		switch cm["name"] {
		case "jenkins":
			jenkins = cm
		case "mite":
			mite = cm
		}
	}
	if jenkins == nil {
		t.Fatal("jenkins container not found in created STS")
	}
	if mite == nil {
		t.Fatal("mite container not found in created STS")
	}

	// Verify user env var merged onto jenkins container.
	envList, _, _ := unstructured.NestedSlice(jenkins, "env")
	envByName := make(map[string]string)
	for _, e := range envList {
		em, _ := e.(map[string]interface{})
		if em == nil {
			continue
		}
		name, _ := em["name"].(string)
		value, _ := em["value"].(string)
		envByName[name] = value
	}
	if v := envByName["MY_OVERLAY_VAR"]; v != "overlay-val" {
		t.Errorf("MY_OVERLAY_VAR = %q, want overlay-val", v)
	}
	// Original env var preserved.
	if v := envByName["JAVA_OPTS"]; v == "" {
		t.Error("JAVA_OPTS missing from merged jenkins env")
	}

	// Verify the user volume is present alongside original volumes.
	volumes, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "volumes")
	volByName := make(map[string]bool)
	for _, v := range volumes {
		vm, _ := v.(map[string]interface{})
		if vm == nil {
			continue
		}
		name, _ := vm["name"].(string)
		volByName[name] = true
	}
	if !volByName["user-custom-vol"] {
		t.Error("user-custom-vol volume missing from merged STS")
	}
	// Original volume preserved.
	if !volByName["jenkins-home"] {
		// jenkins-home is from volumeClaimTemplates, not spec.volumes
		// Check for a real spec.volume like init-scripts
		if !volByName["init-scripts"] && !volByName["bootstrap"] {
			t.Error("expected at least one original spec.volume (init-scripts or bootstrap) preserved")
		}
	}
}
