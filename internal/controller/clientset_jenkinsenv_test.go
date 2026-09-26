package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestGetStatefulSetContainerEnv asserts that the method walks the named
// container's env array through the dynamic client. It reports found=false
// when the StatefulSet or the named container is absent. The cases use the
// jenkins container.
func TestGetStatefulSetContainerEnv(t *testing.T) {
	ctx := context.Background()
	setup := func(objs ...*unstructured.Unstructured) (*ClientsetClient, *dynamicfake.FakeDynamicClient) {
		scheme := runtime.NewScheme()
		stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
		scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})
		objects := make([]runtime.Object, len(objs))
		for i, o := range objs {
			objects[i] = o
		}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
			objects...,
		)
		return &ClientsetClient{dynamic: dyn}, dyn
	}

	t.Run("StatefulSet absent", func(t *testing.T) {
		c, _ := setup()
		env, found, err := c.GetStatefulSetContainerEnv(ctx, "missing-sts", "ns", "jenkins")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Error("found = true, want false for a missing StatefulSet")
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
	})

	t.Run("jenkins container absent", func(t *testing.T) {
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		env, found, err := c.GetStatefulSetContainerEnv(ctx, "test-sts", "ns", "jenkins")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Error("found = true, want false when the jenkins container is absent")
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
	})

	t.Run("VARROA_HEADLESS present", func(t *testing.T) {
		jenkins := simpleContainer("jenkins", "jenkins/jenkins:2.570.1")
		jenkins["env"] = []map[string]interface{}{
			{"name": "VARROA_HEADLESS", "value": "true"},
			{"name": "VARROA_CLAIM_GATE", "value": "0"},
		}
		existing := stsObject(nil,
			[]map[string]interface{}{jenkins, simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		env, found, err := c.GetStatefulSetContainerEnv(ctx, "test-sts", "ns", "jenkins")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if got := env["VARROA_HEADLESS"]; got != "true" {
			t.Errorf("VARROA_HEADLESS = %q, want %q", got, "true")
		}
	})

	t.Run("VARROA_HEADLESS absent", func(t *testing.T) {
		jenkins := simpleContainer("jenkins", "jenkins/jenkins:2.570.1")
		jenkins["env"] = []map[string]interface{}{
			{"name": "VARROA_CLAIM_GATE", "value": "0"},
		}
		existing := stsObject(nil,
			[]map[string]interface{}{jenkins, simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		env, found, err := c.GetStatefulSetContainerEnv(ctx, "test-sts", "ns", "jenkins")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if _, ok := env["VARROA_HEADLESS"]; ok {
			t.Error("VARROA_HEADLESS present, want absent")
		}
	})

	t.Run("valueFrom entry is skipped", func(t *testing.T) {
		jenkins := simpleContainer("jenkins", "jenkins/jenkins:2.570.1")
		jenkins["env"] = []map[string]interface{}{
			{"name": "VARROA_CELL_INSTANCE", "valueFrom": map[string]interface{}{
				"fieldRef": map[string]interface{}{"fieldPath": "metadata.uid"},
			}},
		}
		existing := stsObject(nil,
			[]map[string]interface{}{jenkins, simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		env, found, err := c.GetStatefulSetContainerEnv(ctx, "test-sts", "ns", "jenkins")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if _, ok := env["VARROA_CELL_INSTANCE"]; ok {
			t.Error("a valueFrom entry must not appear in the returned map")
		}
	})
}
