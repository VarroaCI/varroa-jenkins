package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// stsGVR is declared in clientset_overlay_test.go.

// stsObject builds an unstructured StatefulSet with the given name, namespace,
// annotations, containers, and initContainers for use with the dynamic fake.
// containers and initContainers are slices of {"name":...,"image":...,...} maps.
func stsObject(annotations map[string]string, containers, initContainers []map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{
		"name":      "test-sts",
		"namespace": "ns",
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	obj := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{},
			},
		},
	}
	podSpec := obj["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	if len(containers) > 0 {
		podSpec["containers"] = containers
	}
	if len(initContainers) > 0 {
		podSpec["initContainers"] = initContainers
	}
	// Normalize to JSON-compatible types ([]interface{}, float64) so the fake
	// dynamic client's deep-copy accepts the object.
	raw, _ := json.Marshal(obj)
	var norm map[string]interface{}
	_ = json.Unmarshal(raw, &norm)
	return &unstructured.Unstructured{Object: norm}
}

// simpleContainer returns a container map with name and image (and optionally
// imagePullPolicy).
func simpleContainer(name, image string, pullPolicy ...string) map[string]interface{} {
	c := map[string]interface{}{
		"name":  name,
		"image": image,
	}
	if len(pullPolicy) > 0 && pullPolicy[0] != "" {
		c["imagePullPolicy"] = pullPolicy[0]
	}
	return c
}

// addComputedStamp sets the computedImagesAnnotation on an object's metadata.
func addComputedStamp(obj *unstructured.Unstructured, stamp map[string]string) {
	b, _ := json.Marshal(stamp)
	anns, _, _ := unstructured.NestedStringMap(obj.Object, "metadata", "annotations")
	if anns == nil {
		anns = map[string]string{}
	}
	anns[computedImagesAnnotation] = string(b)
	_ = unstructured.SetNestedStringMap(obj.Object, anns, "metadata", "annotations")
}

func TestCreateStatefulSetImageUpdate(t *testing.T) {
	ctx := context.Background()
	setup := func() (*ClientsetClient, *dynamicfake.FakeDynamicClient) {
		scheme := runtime.NewScheme()
		stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
		scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
		)
		return &ClientsetClient{dynamic: dyn}, dyn
	}

	makeSpec := func(jenkinsImage, miteImage string) StatefulSetSpec {
		return StatefulSetSpec{
			Name:                "test-sts",
			Namespace:           "ns",
			ControllerName:      "test",
			JenkinsImage:        jenkinsImage,
			MiteImage:           miteImage,
			MiteImagePullPolicy: "IfNotPresent",
			StorageSize:         "10Gi",
			Resources:           nil,
			OIDCIssuer:          "https://oidc.example.com",
			VarroaLoginURL:      "https://login.example.com",
		}
	}

	t.Run("operator-owned", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "jenkins/jenkins:2.570.2" {
			t.Errorf("jenkins image = %q, want %q", img, "jenkins/jenkins:2.570.2")
		}
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing")
		}
		if stamp["jenkins"] != "jenkins/jenkins:2.570.2" {
			t.Errorf("stamp jenkins = %q, want %q", stamp["jenkins"], "jenkins/jenkins:2.570.2")
		}
	})

	t.Run("out-of-band override preserved when desired is unchanged", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "custom/jenkins:override", "Never"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// Desired (spec) image for jenkins is unchanged from the previous
		// stamp (2.570.1 -> 2.570.1): the out-of-band edit stays preserved.
		spec := makeSpec("jenkins/jenkins:2.570.1", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "custom/jenkins:override" {
			t.Errorf("jenkins image = %q, want %q", img, "custom/jenkins:override")
		}
		if pol, _, _ := unstructured.NestedString(jenkins, "imagePullPolicy"); pol != "Never" {
			t.Errorf("jenkins imagePullPolicy = %q, want %q", pol, "Never")
		}
		// The stamp records the operator's own desired-value baseline for this
		// reconcile (2.570.1), NOT the preserved out-of-band template reality
		// (custom/jenkins:override) — it must keep meaning "what the operator
		// last computed as desired" across ticks, or the preservation check
		// above (which compares this tick's desired against the previous
		// stamp) breaks on the very next reconcile: re-deriving the stamp from
		// the post-preservation template would make prev == the preserved
		// value, which no longer equals an unchanged desired value, and the
		// override would get stomped back to desired on the following tick.
		// The live template (asserted above) is the
		// source of truth for "what's actually applied"; the stamp is the
		// source of truth for "what did the operator last want."
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing")
		}
		if stamp["jenkins"] != "jenkins/jenkins:2.570.1" {
			t.Errorf("stamp jenkins = %q, want %q (operator's desired baseline, unchanged)", stamp["jenkins"], "jenkins/jenkins:2.570.1")
		}

		// Second tick, desired still unchanged: the override must stay
		// preserved indefinitely, not just for one reconcile. A
		// re-stamp-from-applied approach breaks this: it would turn the stamp
		// into "custom/jenkins:override" after tick 1, making tick 2's
		// prev==override != this tick's want==2.570.1, incorrectly treating
		// the still-unchanged desired value as a delta and stomping the
		// override.
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet (tick 2): %v", err)
		}
		got2, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers2, _, _ := unstructured.NestedSlice(got2.Object, "spec", "template", "spec", "containers")
		jenkins2 := findContainer(containers2, "jenkins")
		if jenkins2 == nil {
			t.Fatal("jenkins container not found (tick 2)")
		}
		if img, _, _ := unstructured.NestedString(jenkins2, "image"); img != "custom/jenkins:override" {
			t.Errorf("tick 2 jenkins image = %q, want %q (still preserved)", img, "custom/jenkins:override")
		}
		stamp2 := parseComputedImagesAnnotation(got2)
		if stamp2 == nil {
			t.Fatal("computed images annotation missing (tick 2)")
		}
		if stamp2["jenkins"] != "jenkins/jenkins:2.570.1" {
			t.Errorf("tick 2 stamp jenkins = %q, want %q (desired baseline still unchanged)", stamp2["jenkins"], "jenkins/jenkins:2.570.1")
		}
	})

	t.Run("out-of-band override superseded when desired changes (fleet scenario)", func(t *testing.T) {
		// New spec/overlay intent must always win over a stale out-of-band
		// image hotfix: the preservation rule must not re-adopt the stale
		// live image once the operator's desired value has changed.
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "custom/jenkins:override", "Never"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// Desired (spec) image for jenkins CHANGES from the previous stamp
		// (2.570.1 -> 2.570.2): new intent must override the stale hotfix.
		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "jenkins/jenkins:2.570.2" {
			t.Errorf("jenkins image = %q, want %q (new intent must win over stale hotfix)", img, "jenkins/jenkins:2.570.2")
		}
		// Stamp must match the applied template (2.570.2), not the dropped override.
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing")
		}
		if stamp["jenkins"] != "jenkins/jenkins:2.570.2" {
			t.Errorf("stamp jenkins = %q, want %q", stamp["jenkins"], "jenkins/jenkins:2.570.2")
		}
	})

	t.Run("no annotation on existing (adoption)", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "custom/jenkins:override"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		// No computedImagesAnnotation at all.
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		// Adopted: stamp is absent so live image is NOT preserved.
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "jenkins/jenkins:2.570.2" {
			t.Errorf("jenkins image = %q, want %q", img, "jenkins/jenkins:2.570.2")
		}
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing after update")
		}
		if stamp["jenkins"] != "jenkins/jenkins:2.570.2" {
			t.Errorf("stamp jenkins = %q, want %q", stamp["jenkins"], "jenkins/jenkins:2.570.2")
		}
	})

	t.Run("unparseable annotation JSON", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(
			map[string]string{computedImagesAnnotation: "{not json"},
			[]map[string]interface{}{simpleContainer("jenkins", "custom/jenkins:override"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		// Adopted: unparseable stamp treated as absent.
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "jenkins/jenkins:2.570.2" {
			t.Errorf("jenkins image = %q, want %q", img, "jenkins/jenkins:2.570.2")
		}
	})

	t.Run("mite out-of-band override superseded when class-resolved mite image changes (fleet scenario)", func(t *testing.T) {
		// The mite container was manually STS-patched to a hotfix image at a
		// moment when the operator's desired mite image (via class-resolved
		// effectiveDesiredMiteImage) was "m:1". A later class-level mite
		// image change to "m:2" must win over the stale hotfix, and the
		// stamp must advance to that new desired baseline.
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "hotfix/mite:6fb785d")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "hotfix/mite:6fb785d")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		spec := makeSpec("jenkins/jenkins:2.570.1", "m:2")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		mite := findContainer(containers, "mite")
		if mite == nil {
			t.Fatal("mite container not found")
		}
		if img, _, _ := unstructured.NestedString(mite, "image"); img != "m:2" {
			t.Errorf("mite image = %q, want %q (new spec intent must win over stale hotfix)", img, "m:2")
		}

		initContainers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "initContainers")
		initGroovy := findContainer(initContainers, "init-groovy")
		if initGroovy == nil {
			t.Fatal("init-groovy container not found")
		}
		if img, _, _ := unstructured.NestedString(initGroovy, "image"); img != "m:2" {
			t.Errorf("init-groovy image = %q, want %q (shares the mite image; also superseded)", img, "m:2")
		}

		// The stamp records the desired baseline for every container. Here
		// desired intent changed, so preservation does not win and the
		// baseline and the applied template agree.
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing")
		}
		if stamp["mite"] != "m:2" {
			t.Errorf("stamp mite = %q, want %q", stamp["mite"], "m:2")
		}
		if stamp["init-groovy"] != "m:2" {
			t.Errorf("stamp init-groovy = %q, want %q", stamp["init-groovy"], "m:2")
		}
	})

	// The image-preservation predicate must not also preserve the mite
	// container's imagePullPolicy: the class-resolved mite imagePullPolicy
	// has its own independent drift check, so once a controller has a
	// preserved out-of-band mite image override, a genuine desired pull-policy
	// change must still win — piggybacking pull-policy preservation onto the
	// image predicate would silently revert it to the stale live value on
	// every Provisioning pass, an unconvergeable Connected->Provisioning loop.
	t.Run("mite imagePullPolicy is not preserved alongside a preserved out-of-band image", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "hotfix/mite:6fb785d", "Never")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "hotfix/mite:6fb785d")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// Desired mite image is unchanged (m:1 -> m:1: the hotfix stays
		// preserved), but desired imagePullPolicy is a genuine change
		// (spec now wants "Always", live/stale is "Never").
		spec := makeSpec("jenkins/jenkins:2.570.1", "m:1")
		spec.MiteImagePullPolicy = "Always"
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		mite := findContainer(containers, "mite")
		if mite == nil {
			t.Fatal("mite container not found")
		}
		// Image still preserved (out-of-band, desired unchanged).
		if img, _, _ := unstructured.NestedString(mite, "image"); img != "hotfix/mite:6fb785d" {
			t.Errorf("mite image = %q, want %q (still preserved)", img, "hotfix/mite:6fb785d")
		}
		// But the pull-policy change must win, not get silently swallowed.
		if pol, _, _ := unstructured.NestedString(mite, "imagePullPolicy"); pol != "Always" {
			t.Errorf("mite imagePullPolicy = %q, want %q (desired change must win over stale live value)", pol, "Always")
		}
	})

	t.Run("init containers by name with reordering", func(t *testing.T) {
		c, dyn := setup()
		// Existing initContainers ordered [init-groovy, plugins-init] (reversed).
		// plugins-init has an override image different from stamp.
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{
				simpleContainer("init-groovy", "m:1"),
				simpleContainer("plugins-init", "custom/plugins:override"),
			},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// spec MiteImage="m:2" => init-groovy should update (operator-owned, stamp "m:1", delta).
		// JenkinsImage is left UNCHANGED (2.570.1) so plugins-init's own desired
		// value is unchanged from its stamp, keeping its out-of-band override
		// preserved and isolating this test to reordering behavior alone.
		spec := makeSpec("jenkins/jenkins:2.570.1", "m:2")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		initContainers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "initContainers")
		pluginsInit := findContainer(initContainers, "plugins-init")
		if pluginsInit == nil {
			t.Fatal("plugins-init container not found")
		}
		if img, _, _ := unstructured.NestedString(pluginsInit, "image"); img != "custom/plugins:override" {
			t.Errorf("plugins-init image = %q, want %q (override preserved)", img, "custom/plugins:override")
		}

		initGroovy := findContainer(initContainers, "init-groovy")
		if initGroovy == nil {
			t.Fatal("init-groovy container not found")
		}
		// init-groovy is operator-owned (stamp matches live), so it should update to new desired.
		if img, _, _ := unstructured.NestedString(initGroovy, "image"); img != "m:2" {
			t.Errorf("init-groovy image = %q, want %q", img, "m:2")
		}
	})

	t.Run("overlay-declared jenkins image", func(t *testing.T) {
		c, dyn := setup()
		existing := stsObject(nil,
			[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "m:1")},
			[]map[string]interface{}{simpleContainer("plugins-init", "jenkins/jenkins:2.570.1"), simpleContainer("init-groovy", "m:1")},
		)
		addComputedStamp(existing, map[string]string{
			"jenkins":      "jenkins/jenkins:2.570.1",
			"mite":         "m:1",
			"plugins-init": "jenkins/jenkins:2.570.1",
			"init-groovy":  "m:1",
		})
		_, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: jenkins\n        image: my-reg/custom:9.9\n",
		}
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		jenkins := findContainer(containers, "jenkins")
		if jenkins == nil {
			t.Fatal("jenkins container not found")
		}
		if img, _, _ := unstructured.NestedString(jenkins, "image"); img != "my-reg/custom:9.9" {
			t.Errorf("jenkins image = %q, want %q (overlay)", img, "my-reg/custom:9.9")
		}
		// Stamp records the actual image that was written (overlay value).
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing")
		}
		if stamp["jenkins"] != "my-reg/custom:9.9" {
			t.Errorf("stamp jenkins = %q, want %q", stamp["jenkins"], "my-reg/custom:9.9")
		}
	})

	t.Run("create path (no existing STS)", func(t *testing.T) {
		c, dyn := setup()
		spec := makeSpec("jenkins/jenkins:2.570.2", "m:1")
		if err := c.CreateStatefulSet(ctx, spec); err != nil {
			t.Fatalf("CreateStatefulSet: %v", err)
		}
		got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		stamp := parseComputedImagesAnnotation(got)
		if stamp == nil {
			t.Fatal("computed images annotation missing on create")
		}
		if stamp["jenkins"] != "jenkins/jenkins:2.570.2" {
			t.Errorf("stamp jenkins = %q, want %q", stamp["jenkins"], "jenkins/jenkins:2.570.2")
		}
	})
}

// findContainer returns the first container map in a slice whose "name" matches.
func findContainer(list []interface{}, name string) map[string]interface{} {
	for _, c := range list {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		n, _ := cm["name"].(string)
		if n == name {
			return cm
		}
	}
	return nil
}

// TestCreateStatefulSetPreservesVolumeClaimTemplates pins the immutability
// guard: volumeClaimTemplates cannot be updated on a StatefulSet, so the
// update path must carry the live value verbatim — otherwise any rendered
// difference (persistence.size edited post-creation, or a pre-epic CR whose
// old storage value now renders the default) makes the API server reject the
// whole update and wedges Provisioning forever.
func TestCreateStatefulSetPreservesVolumeClaimTemplates(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
	)
	c := &ClientsetClient{dynamic: dyn}

	existing := stsObject(nil,
		[]map[string]interface{}{simpleContainer("jenkins", "jenkins/jenkins:2.570.1"), simpleContainer("mite", "m:1")},
		nil,
	)
	liveVCT := []interface{}{map[string]interface{}{
		"metadata": map[string]interface{}{"name": "jenkins-home"},
		"spec": map[string]interface{}{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": "5Gi"}},
		},
	}}
	if err := unstructured.SetNestedSlice(existing.Object, liveVCT, "spec", "volumeClaimTemplates"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(existing.Object, "OrderedReady", "spec", "podManagementPolicy"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(stsGVR).Namespace("ns").Create(ctx, existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	spec := StatefulSetSpec{
		Name:           "test-sts",
		Namespace:      "ns",
		ControllerName: "test",
		JenkinsImage:   "jenkins/jenkins:2.570.1",
		MiteImage:      "m:1",
		StorageSize:    "20Gi", // renders a different VCT than the live 5Gi
		OIDCIssuer:     "https://oidc.example.com",
		VarroaLoginURL: "https://login.example.com",
	}
	if err := c.CreateStatefulSet(ctx, spec); err != nil {
		t.Fatalf("CreateStatefulSet update: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "test-sts", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vct, found, _ := unstructured.NestedSlice(got.Object, "spec", "volumeClaimTemplates")
	if !found {
		t.Fatal("volumeClaimTemplates missing, want the live template preserved")
	}
	if !reflect.DeepEqual(vct, liveVCT) {
		t.Errorf("volumeClaimTemplates = %v, want live template preserved verbatim %v", vct, liveVCT)
	}
	if pmp, _, _ := unstructured.NestedString(got.Object, "spec", "podManagementPolicy"); pmp != "OrderedReady" {
		t.Errorf("podManagementPolicy = %q, want live OrderedReady preserved", pmp)
	}

	// The create path must still render persistence fresh — preservation is
	// update-only.
	fresh := spec
	fresh.Name = "fresh-sts"
	if err := c.CreateStatefulSet(ctx, fresh); err != nil {
		t.Fatalf("CreateStatefulSet create: %v", err)
	}
	created, err := dyn.Resource(stsGVR).Namespace("ns").Get(ctx, "fresh-sts", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cvct, _, _ := unstructured.NestedSlice(created.Object, "spec", "volumeClaimTemplates")
	if len(cvct) == 0 {
		t.Fatal("created STS has no volumeClaimTemplates")
	}
	storage, _, _ := unstructured.NestedString(cvct[0].(map[string]interface{}),
		"spec", "resources", "requests", "storage")
	if storage != "20Gi" {
		t.Errorf("created storage = %q, want freshly rendered 20Gi", storage)
	}
}
