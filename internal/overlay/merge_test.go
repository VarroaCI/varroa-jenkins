package overlay

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// structuredBase returns a small StatefulSet-shaped unstructured with 2 jenkins env vars,
// 2 containers (jenkins, mite), and 2 volumes, matching the shape the real builder produces.
func structuredBase() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "StatefulSet",
			"metadata": map[string]interface{}{
				"name": "test-sts",
			},
			"spec": map[string]interface{}{
				"serviceName": "test-svc",
				"replicas":    int64(1),
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": "test",
					},
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{"app": "test"},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "jenkins",
								"image": "jenkins:lts",
								"ports": []interface{}{
									map[string]interface{}{"name": "http", "containerPort": float64(8080)},
								},
								"env": []interface{}{
									map[string]interface{}{"name": "JAVA_OPTS", "value": "-Xmx1g"},
									map[string]interface{}{"name": "VARROA_OIDC_ISSUER", "value": "https://oidc.example.com"},
								},
							},
							map[string]interface{}{
								"name":  "mite",
								"image": "mite:latest",
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name":     "jenkins-home",
								"emptyDir": map[string]interface{}{},
							},
							map[string]interface{}{
								"name": "bootstrap",
								"secret": map[string]interface{}{
									"secretName": "test-bootstrap",
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestMerge_EnvAddedWithoutListReplacement(t *testing.T) {
	base := structuredBase()

	// Patch adding one env var to the jenkins container.
	patchYAML := []byte(`
spec:
  template:
    spec:
      containers:
        - name: jenkins
          env:
            - name: MY_CUSTOM_VAR
              value: custom-value
`)

	merged, err := Merge(base, patchYAML, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(merged.Object, "spec", "template", "spec", "containers")
	var jenkins map[string]interface{}
	for _, c := range containers {
		cm := c.(map[string]interface{})
		if cm["name"] == "jenkins" {
			jenkins = cm
			break
		}
	}
	if jenkins == nil {
		t.Fatal("jenkins container not found in merged result")
	}

	envList, _, _ := unstructured.NestedSlice(jenkins, "env")
	if len(envList) != 3 {
		t.Fatalf("expected 3 env vars, got %d", len(envList))
	}

	envByName := make(map[string]string)
	for _, e := range envList {
		em := e.(map[string]interface{})
		envByName[em["name"].(string)] = em["value"].(string)
	}

	// Existing env vars preserved.
	if v := envByName["JAVA_OPTS"]; v != "-Xmx1g" {
		t.Errorf("JAVA_OPTS = %q, want -Xmx1g", v)
	}
	if v := envByName["VARROA_OIDC_ISSUER"]; v != "https://oidc.example.com" {
		t.Errorf("VARROA_OIDC_ISSUER = %q, want https://oidc.example.com", v)
	}
	// New env var added.
	if v := envByName["MY_CUSTOM_VAR"]; v != "custom-value" {
		t.Errorf("MY_CUSTOM_VAR = %q, want custom-value", v)
	}
}

func TestMerge_ContainerAddedByName(t *testing.T) {
	base := structuredBase() // has jenkins, mite

	// Patch adding a third container "sidecar".
	patchYAML := []byte(`
spec:
  template:
    spec:
      containers:
        - name: sidecar
          image: sidecar:latest
          env:
            - name: SIDECAR_VAR
              value: "1"
`)

	merged, err := Merge(base, patchYAML, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(merged.Object, "spec", "template", "spec", "containers")
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	contByName := make(map[string]map[string]interface{})
	for _, c := range containers {
		cm := c.(map[string]interface{})
		contByName[cm["name"].(string)] = cm
	}

	// Original containers preserved.
	if _, ok := contByName["jenkins"]; !ok {
		t.Error("jenkins container missing")
	}
	if _, ok := contByName["mite"]; !ok {
		t.Error("mite container missing")
	}
	// New container added.
	sc, ok := contByName["sidecar"]
	if !ok {
		t.Fatal("sidecar container missing")
	}
	if img, ok := sc["image"].(string); !ok || img != "sidecar:latest" {
		t.Errorf("sidecar image = %v, want sidecar:latest", sc["image"])
	}
}

func TestMerge_BaseNotMutated(t *testing.T) {
	base := structuredBase()
	// Deep-copy via JSON round-trip for comparison.
	origBytes, _ := json.Marshal(base.Object)
	var origCopy map[string]interface{}
	_ = json.Unmarshal(origBytes, &origCopy)

	patchYAML := []byte(`
spec:
  template:
    spec:
      containers:
        - name: jenkins
          env:
            - name: MY_VAR
              value: v
`)

	merged, err := Merge(base, patchYAML, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}
	if merged == base {
		t.Error("Merge returned the same pointer as base; expected a new unstructured")
	}

	// Verify base is unchanged.
	baseBytes, _ := json.Marshal(base.Object)
	origBytes2, _ := json.Marshal(origCopy)
	if string(baseBytes) != string(origBytes2) {
		t.Error("base was mutated by Merge")
	}
}

func TestMerge_NilOrEmptyPatch(t *testing.T) {
	base := structuredBase()

	// nil patch.
	result, err := Merge(base, nil, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge(nil) failed: %v", err)
	}
	if result != base {
		t.Error("Merge(nil) did not return base unchanged")
	}

	// Empty patch.
	result, err = Merge(base, []byte{}, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge(empty) failed: %v", err)
	}
	if result != base {
		t.Error("Merge(empty) did not return base unchanged")
	}

	// Whitespace-only — this is not nil/empty, so YAMLToJSON will parse it.
	result, err = Merge(base, []byte("   \n"), appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("Merge(whitespace) failed: %v", err)
	}
	if result == base {
		t.Error("Merge(whitespace) returned base unchanged; whitespace is a non-empty patch")
	}
}

func TestParsePatch_MalformedYAML(t *testing.T) {
	err := ParsePatch([]byte("{[bad"), appsv1.StatefulSet{})
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	t.Logf("malformed YAML error: %v", err)
}

func TestParsePatch_ValidPatch(t *testing.T) {
	validYAML := []byte(`
spec:
  template:
    spec:
      containers:
        - name: jenkins
          env:
            - name: FOO
              value: bar
`)
	err := ParsePatch(validYAML, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("expected nil for valid patch, got: %v", err)
	}
}

func TestParsePatch_Empty(t *testing.T) {
	if err := ParsePatch(nil, appsv1.StatefulSet{}); err != nil {
		t.Errorf("ParsePatch(nil): %v", err)
	}
	if err := ParsePatch([]byte{}, appsv1.StatefulSet{}); err != nil {
		t.Errorf("ParsePatch(empty): %v", err)
	}
}

func TestMerge_ServiceAnnotations(t *testing.T) {
	baseSvc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name": "test-svc",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"app": "test"},
				"ports": []interface{}{
					map[string]interface{}{"name": "http", "port": float64(8080)},
				},
			},
		},
	}

	patchYAML := []byte(`
metadata:
  annotations:
    my-annotation: "custom"
`)

	merged, err := Merge(baseSvc, patchYAML, corev1.Service{})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}

	anns, _, _ := unstructured.NestedStringMap(merged.Object, "metadata", "annotations")
	if v := anns["my-annotation"]; v != "custom" {
		t.Errorf("annotation = %q, want custom", v)
	}

	// Original fields intact.
	ports, _, _ := unstructured.NestedSlice(merged.Object, "spec", "ports")
	if len(ports) != 1 {
		t.Errorf("expected 1 port, got %d", len(ports))
	}
}
