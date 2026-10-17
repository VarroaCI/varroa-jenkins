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

// TestCreateStatefulSet_StampsJenkinsUpstream asserts that CreateStatefulSet
// writes the upstream stamp on the object metadata, and that a later create
// with the other mode rewrites it.
func TestCreateStatefulSet_StampsJenkinsUpstream(t *testing.T) {
	ctx := context.Background()
	c, dyn := newFakeStsClient()
	spec := baseCascHashSpec("ci-upstream-stamp", "")
	read := func() string {
		t.Helper()
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "ci-upstream-stamp", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got.GetAnnotations()[jenkinsUpstreamAnnotation]
	}

	spec.JenkinsUpstreamOff = true
	if err := c.CreateStatefulSet(ctx, spec, noStatefulSetFence); err != nil {
		t.Fatalf("create off: %v", err)
	}
	if got := read(); got != "off" {
		t.Fatalf("stamp after off = %q, want off", got)
	}

	spec.JenkinsUpstreamOff = false
	if err := c.CreateStatefulSet(ctx, spec, noStatefulSetFence); err != nil {
		t.Fatalf("create on: %v", err)
	}
	if got := read(); got != "on" {
		t.Fatalf("stamp after on = %q, want on", got)
	}
}

// TestGetStatefulSetJenkinsUpstream asserts that the reader returns the
// upstream stamp and the jenkins JAVA_OPTS, and that found is false for a
// missing StatefulSet.
func TestGetStatefulSetJenkinsUpstream(t *testing.T) {
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
		_, _, found, err := c.GetStatefulSetJenkinsUpstream(ctx, "missing-sts", "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Error("found = true, want false for a missing StatefulSet")
		}
	})

	t.Run("stamp and JAVA_OPTS present", func(t *testing.T) {
		jenkins := simpleContainer("jenkins", "jenkins/jenkins:2.570.1")
		jenkins["env"] = []map[string]interface{}{
			{"name": "JAVA_OPTS", "value": "-Dhudson.model.UpdateCenter.never=true"},
		}
		existing := stsObject(map[string]string{jenkinsUpstreamAnnotation: "off"},
			[]map[string]interface{}{jenkins, simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		stamp, javaOpts, found, err := c.GetStatefulSetJenkinsUpstream(ctx, "test-sts", "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if stamp != "off" {
			t.Errorf("stamp = %q, want off", stamp)
		}
		if javaOpts != "-Dhudson.model.UpdateCenter.never=true" {
			t.Errorf("JAVA_OPTS = %q", javaOpts)
		}
	})

	t.Run("stamp absent", func(t *testing.T) {
		jenkins := simpleContainer("jenkins", "jenkins/jenkins:2.570.1")
		existing := stsObject(nil,
			[]map[string]interface{}{jenkins, simpleContainer("mite", "m:1")},
			nil,
		)
		c, dyn := setup()
		if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		stamp, _, found, err := c.GetStatefulSetJenkinsUpstream(ctx, "test-sts", "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if stamp != "" {
			t.Errorf("stamp = %q, want empty", stamp)
		}
	})
}
