package overlay

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestCompilePodOverrides_Nil(t *testing.T) {
	yamlBytes, err := CompilePodOverrides(nil, "jenkins")
	if err != nil {
		t.Fatalf("CompilePodOverrides(nil) failed: %v", err)
	}
	if yamlBytes != nil {
		t.Errorf("expected nil, got %d bytes", len(yamlBytes))
	}
}

func TestCompilePodOverrides_NoJvmOpts(t *testing.T) {
	// When only jvmOpts is set, the compiled patch should be nil/empty
	// because jvmOpts is handled by the builder, not the patch.
	po := &v1alpha1.PodOverrides{
		JvmOpts: "-Xmx2g",
	}
	yamlBytes, err := CompilePodOverrides(po, "jenkins")
	if err != nil {
		t.Fatalf("CompilePodOverrides failed: %v", err)
	}
	if yamlBytes != nil {
		t.Errorf("expected nil for jvmOpts-only override, got %d bytes: %s", len(yamlBytes), string(yamlBytes))
	}
}

func TestCompilePodOverrides_Env(t *testing.T) {
	// Set env (probes moved to ProbesSpec, no longer on PodOverrides).
	// The compiled patch should place env under containers[name=jenkins]
	// and produce a valid YAML patch.
	po := &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "MY_VAR", Value: "my-val"},
			{Name: "ANOTHER", Value: "another-val"},
		},
	}

	yamlBytes, err := CompilePodOverrides(po, "jenkins")
	if err != nil {
		t.Fatalf("CompilePodOverrides failed: %v", err)
	}
	if len(yamlBytes) == 0 {
		t.Fatal("expected non-empty YAML patch")
	}
	t.Logf("Compiled patch:\n%s", string(yamlBytes))

	// Apply the compiled patch onto a base StatefulSet using Merge().
	base := structuredBase()
	merged, err := Merge(base, yamlBytes, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}

	// Verify the jenkins container has the new env vars AND the original ones.
	containers, _, _ := unstructured.NestedSlice(merged.Object, "spec", "template", "spec", "containers")
	var jenkins map[string]interface{}
	var mite map[string]interface{}
	for _, c := range containers {
		cm := c.(map[string]interface{})
		switch cm["name"] {
		case "jenkins":
			jenkins = cm
		case "mite":
			mite = cm
		}
	}
	if jenkins == nil {
		t.Fatal("jenkins container not found in merged result")
	}

	// Check env: original 2 + new 2 = 4.
	envList, _, _ := unstructured.NestedSlice(jenkins, "env")
	if len(envList) != 4 {
		t.Fatalf("expected 4 env vars on jenkins, got %d", len(envList))
	}
	envByName := make(map[string]string)
	for _, e := range envList {
		em := e.(map[string]interface{})
		envByName[em["name"].(string)] = em["value"].(string)
	}
	// Original preserved.
	if v := envByName["JAVA_OPTS"]; v != "-Xmx1g" {
		t.Errorf("JAVA_OPTS = %q, want -Xmx1g", v)
	}
	if v := envByName["VARROA_OIDC_ISSUER"]; v != "https://oidc.example.com" {
		t.Errorf("VARROA_OIDC_ISSUER = %q, want https://oidc.example.com", v)
	}
	// New env added.
	if v := envByName["MY_VAR"]; v != "my-val" {
		t.Errorf("MY_VAR = %q, want my-val", v)
	}
	if v := envByName["ANOTHER"]; v != "another-val" {
		t.Errorf("ANOTHER = %q, want another-val", v)
	}

	// Verify mite container is untouched.
	if mite == nil {
		t.Fatal("mite container not found in merged result")
	}
	if img, ok := mite["image"].(string); !ok || img != "mite:latest" {
		t.Errorf("mite image = %v, want mite:latest", mite["image"])
	}
	// Mite should have no env (it had none in the base and none were added).
	_, miteHasEnv, _ := unstructured.NestedSlice(mite, "env")
	if miteHasEnv {
		t.Error("mite container unexpectedly has env after merge")
	}
}

func TestCompilePodOverrides_NoEmptyTypeMeta(t *testing.T) {
	po := &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "FOO", Value: "bar"},
		},
	}
	yamlBytes, err := CompilePodOverrides(po, "jenkins")
	if err != nil {
		t.Fatalf("CompilePodOverrides failed: %v", err)
	}

	// Convert back to map and check for apiVersion/kind.
	patchJSON, err := yaml.YAMLToJSON(yamlBytes)
	if err != nil {
		t.Fatalf("YAMLToJSON: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(patchJSON, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := m["apiVersion"]; ok {
		t.Error("compiled patch has apiVersion key (should be stripped)")
	}
	if _, ok := m["kind"]; ok {
		t.Error("compiled patch has kind key (should be stripped)")
	}
}

func TestCompilePodOverrides_AllFields(t *testing.T) {
	// Exercise every field category: container, pod-spec, pod-meta, sts-meta.
	po := &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "ENV1", Value: "v1"},
		},
		Volumes: []corev1.Volume{
			{Name: "custom-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		PodLabels:      map[string]string{"custom-pod-label": "v"},
		PodAnnotations: map[string]string{"custom-pod-anno": "v"},
		Labels:         map[string]string{"custom-sts-label": "v"},
		Annotations:    map[string]string{"custom-sts-anno": "v"},
		NodeSelector:   map[string]string{"disktype": "ssd"},
	}

	yamlBytes, err := CompilePodOverrides(po, "jenkins")
	if err != nil {
		t.Fatalf("CompilePodOverrides failed: %v", err)
	}
	if len(yamlBytes) == 0 {
		t.Fatal("expected non-empty YAML patch")
	}
	t.Logf("All-fields patch:\n%s", string(yamlBytes))

	// Apply onto a base.
	base := structuredBase()
	merged, err := Merge(base, yamlBytes, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}

	// Check custom label landed on StatefulSet metadata.
	stsLabels, _, _ := unstructured.NestedStringMap(merged.Object, "metadata", "labels")
	if v, ok := stsLabels["custom-sts-label"]; !ok || v != "v" {
		t.Errorf("sts label custom-sts-label = %v, want v", v)
	}

	// Check custom annotation landed on StatefulSet metadata.
	stsAnns, _, _ := unstructured.NestedStringMap(merged.Object, "metadata", "annotations")
	if v, ok := stsAnns["custom-sts-anno"]; !ok || v != "v" {
		t.Errorf("sts annotation custom-sts-anno = %v, want v", v)
	}

	// Check pod label landed on template metadata.
	podLabels, _, _ := unstructured.NestedStringMap(merged.Object, "spec", "template", "metadata", "labels")
	if v, ok := podLabels["custom-pod-label"]; !ok || v != "v" {
		t.Errorf("pod label custom-pod-label = %v, want v", v)
	}

	// Check pod annotation landed on template metadata.
	podAnns, _, _ := unstructured.NestedStringMap(merged.Object, "spec", "template", "metadata", "annotations")
	if v, ok := podAnns["custom-pod-anno"]; !ok || v != "v" {
		t.Errorf("pod annotation custom-pod-anno = %v, want v", v)
	}

	// Check volumes include the custom one and original ones.
	vols, _, _ := unstructured.NestedSlice(merged.Object, "spec", "template", "spec", "volumes")
	volByName := make(map[string]bool)
	for _, v := range vols {
		vm := v.(map[string]interface{})
		volByName[vm["name"].(string)] = true
	}
	if !volByName["custom-vol"] {
		t.Error("custom-vol not found in merged volumes")
	}
	if !volByName["jenkins-home"] {
		t.Error("jenkins-home not found in merged volumes (was replaced)")
	}

	// Check nodeSelector.
	ns, _, _ := unstructured.NestedStringMap(merged.Object, "spec", "template", "spec", "nodeSelector")
	if v, ok := ns["disktype"]; !ok || v != "ssd" {
		t.Errorf("nodeSelector disktype = %v, want ssd", v)
	}
}
