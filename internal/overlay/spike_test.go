package overlay

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// buildTestStatefulSet returns an *unstructured.Unstructured shaped like the
// real buildStatefulSet output (containers "jenkins"/"mite", env, volumes, etc.)
// so the spike tests exercise the same merge paths the real code will.
func buildTestStatefulSet() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "StatefulSet",
			"metadata": map[string]interface{}{
				"name":      "test-controller",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"app": "test-controller",
				},
			},
			"spec": map[string]interface{}{
				"serviceName": "test-controller-svc",
				"replicas":    int64(1),
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": "test-controller",
					},
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "test-controller",
						},
					},
					"spec": map[string]interface{}{
						"serviceAccountName": "test-sa",
						"securityContext": map[string]interface{}{
							"fsGroup": int64(1000),
						},
						"initContainers": []interface{}{
							map[string]interface{}{
								"name":    "plugins-init",
								"image":   "jenkins:lts",
								"command": []interface{}{"sh", "-c"},
								"args":    []interface{}{"jenkins-plugin-cli ..."},
								"securityContext": map[string]interface{}{
									"runAsUser": int64(1000),
								},
								"volumeMounts": []interface{}{
									map[string]interface{}{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
									map[string]interface{}{"name": "plugins", "mountPath": "/var/run/varroa/plugins"},
								},
							},
						},
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "jenkins",
								"image": "jenkins:lts",
								"ports": []interface{}{
									map[string]interface{}{"name": "http", "containerPort": int64(8080)},
									map[string]interface{}{"name": "agent", "containerPort": int64(50000)},
								},
								"volumeMounts": []interface{}{
									map[string]interface{}{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
									map[string]interface{}{"name": "init-scripts", "mountPath": "/var/run/varroa/init"},
									map[string]interface{}{"name": "bootstrap", "mountPath": "/var/run/varroa/bootstrap"},
									map[string]interface{}{"name": "casc-bundle", "mountPath": "/var/jenkins_home/casc"},
									map[string]interface{}{"name": "varroa-run", "mountPath": "/var/run/varroa/run"},
								},
								"env": []interface{}{
									map[string]interface{}{"name": "JAVA_OPTS", "value": "-Djenkins.install.runSetupWizard=false"},
									map[string]interface{}{"name": "VARROA_OIDC_ISSUER", "value": "https://oidc.example.com"},
									map[string]interface{}{"name": "CASC_JENKINS_CONFIG", "value": "/var/jenkins_home/casc"},
								},
							},
							map[string]interface{}{
								"name":    "mite",
								"image":   "mite:latest",
								"command": []interface{}{"/app/varroa-mite"},
								"ports": []interface{}{
									map[string]interface{}{"name": "grpc", "containerPort": int64(9090)},
								},
								"env": []interface{}{
									map[string]interface{}{"name": "VARROA_ENDPOINT", "value": "https://endpoint"},
								},
								"volumeMounts": []interface{}{
									map[string]interface{}{"name": "bootstrap", "mountPath": "/var/run/varroa/bootstrap"},
									map[string]interface{}{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
								},
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "init-scripts",
								"configMap": map[string]interface{}{
									"name": "test-init",
								},
							},
							map[string]interface{}{
								"name": "bootstrap",
								"secret": map[string]interface{}{
									"secretName": "test-bootstrap",
								},
							},
							map[string]interface{}{
								"name": "plugins",
								"configMap": map[string]interface{}{
									"name": "test-plugins",
								},
							},
							map[string]interface{}{
								"name":     "casc-bundle",
								"emptyDir": map[string]interface{}{},
							},
							map[string]interface{}{
								"name":     "varroa-run",
								"emptyDir": map[string]interface{}{},
							},
						},
					},
				},
				"volumeClaimTemplates": []interface{}{
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name": "jenkins-home",
						},
						"spec": map[string]interface{}{
							"accessModes": []interface{}{"ReadWriteOnce"},
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"storage": "10Gi",
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestSpike_StatefulSetMergeEnv(t *testing.T) {
	base := buildTestStatefulSet()

	// Build a patch adding one env var to the jenkins container.
	patchSts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "jenkins",
							Env: []corev1.EnvVar{
								{Name: "MY_CUSTOM_VAR", Value: "custom-value"},
							},
						},
					},
				},
			},
		},
	}

	origJSON, err := json.Marshal(base.Object)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}

	patchJSON, err := json.Marshal(patchSts)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}

	mergedJSON, err := strategicpatch.StrategicMergePatch(origJSON, patchJSON, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("StrategicMergePatch failed: %v", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Verify apiVersion/kind are preserved.
	if apiVer, ok := merged["apiVersion"].(string); !ok || apiVer != "apps/v1" {
		t.Errorf("apiVersion = %q, want apps/v1", apiVer)
	}
	if kind, ok := merged["kind"].(string); !ok || kind != "StatefulSet" {
		t.Errorf("kind = %q, want StatefulSet", kind)
	}

	// Verify existing env vars are preserved AND the new one is added.
	containers := merged["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})
	var jenkinsContainer map[string]interface{}
	for _, c := range containers {
		cm := c.(map[string]interface{})
		if cm["name"] == "jenkins" {
			jenkinsContainer = cm
			break
		}
	}
	if jenkinsContainer == nil {
		t.Fatal("jenkins container not found in merged result")
	}
	envList := jenkinsContainer["env"].([]interface{})
	envByName := make(map[string]string)
	for _, e := range envList {
		em := e.(map[string]interface{})
		envByName[em["name"].(string)] = em["value"].(string)
	}

	// Original env vars must still be present.
	if v := envByName["JAVA_OPTS"]; v != "-Djenkins.install.runSetupWizard=false" {
		t.Errorf("JAVA_OPTS = %q, want original value", v)
	}
	if v := envByName["VARROA_OIDC_ISSUER"]; v != "https://oidc.example.com" {
		t.Errorf("VARROA_OIDC_ISSUER = %q, want original value", v)
	}
	// New env var must be added.
	if v := envByName["MY_CUSTOM_VAR"]; v != "custom-value" {
		t.Errorf("MY_CUSTOM_VAR = %q, want custom-value", v)
	}

	// Verify port values survive round-trip (json.Unmarshal into interface{} yields
	// float64 — that's expected Go stdlib behavior; the VALUE is preserved).
	ports := jenkinsContainer["ports"].([]interface{})
	for _, p := range ports {
		pm := p.(map[string]interface{})
		switch pm["name"] {
		case "http":
			cp, ok := pm["containerPort"].(float64)
			if !ok {
				t.Fatalf("http port type = %T", pm["containerPort"])
			}
			if cp != 8080 {
				t.Errorf("http port = %v, want 8080", cp)
			}
		case "agent":
			cp, ok := pm["containerPort"].(float64)
			if !ok {
				t.Fatalf("agent port type = %T", pm["containerPort"])
			}
			if cp != 50000 {
				t.Errorf("agent port = %v, want 50000", cp)
			}
		}
	}
}

func TestSpike_StatefulSetMergeVolume(t *testing.T) {
	base := buildTestStatefulSet()

	// Build a patch adding one new volume.
	patchSts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "my-custom-volume",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	origJSON, err := json.Marshal(base.Object)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}
	patchJSON, err := json.Marshal(patchSts)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}

	mergedJSON, err := strategicpatch.StrategicMergePatch(origJSON, patchJSON, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("StrategicMergePatch failed: %v", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	volumes := merged["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["volumes"].([]interface{})

	// Original volumes must be preserved.
	volByName := make(map[string]bool)
	for _, v := range volumes {
		vm := v.(map[string]interface{})
		volByName[vm["name"].(string)] = true
	}
	for _, name := range []string{"init-scripts", "bootstrap", "plugins", "casc-bundle", "varroa-run"} {
		if !volByName[name] {
			t.Errorf("original volume %q missing after merge", name)
		}
	}
	if !volByName["my-custom-volume"] {
		t.Error("new volume 'my-custom-volume' missing after merge")
	}
}

func TestSpike_ServiceMergeAnnotations(t *testing.T) {
	// Build a small Service unstructured to test corev1.Service schema merge.
	baseSvc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "test-svc",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"app": "test",
				},
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "test",
				},
				"ports": []interface{}{
					map[string]interface{}{"name": "http", "port": int64(8080), "targetPort": int64(8080)},
					map[string]interface{}{"name": "agent", "port": int64(50000), "targetPort": int64(50000)},
				},
			},
		},
	}

	// Patch adding an annotation.
	patchSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"my-custom-annotation": "custom-value",
			},
		},
	}

	origJSON, err := json.Marshal(baseSvc.Object)
	if err != nil {
		t.Fatalf("marshal baseSvc: %v", err)
	}
	patchJSON, err := json.Marshal(patchSvc)
	if err != nil {
		t.Fatalf("marshal patchSvc: %v", err)
	}

	mergedJSON, err := strategicpatch.StrategicMergePatch(origJSON, patchJSON, corev1.Service{})
	if err != nil {
		t.Fatalf("StrategicMergePatch failed: %v", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Verify apiVersion/kind preserved.
	if apiVer, ok := merged["apiVersion"].(string); !ok || apiVer != "v1" {
		t.Errorf("apiVersion = %q, want v1", apiVer)
	}
	if kind, ok := merged["kind"].(string); !ok || kind != "Service" {
		t.Errorf("kind = %q, want Service", kind)
	}

	// Verify annotation added.
	anns := merged["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if v := anns["my-custom-annotation"]; v != "custom-value" {
		t.Errorf("annotation = %q, want custom-value", v)
	}

	// Verify spec and ports intact.
	spec := merged["spec"].(map[string]interface{})
	ports := spec["ports"].([]interface{})
	if len(ports) != 2 {
		t.Errorf("got %d ports, want 2", len(ports))
	}
	port0 := ports[0].(map[string]interface{})
	if p, ok := port0["port"].(float64); !ok || p != 8080 {
		t.Errorf("port 0 port = %T(%v), want 8080", port0["port"], port0["port"])
	}
}

func TestSpike_TypeMetaHazard(t *testing.T) {
	// Build a patch from a zero-value appsv1.StatefulSet that only sets one env var.
	// This simulates what CompilePodOverrides does: the typed struct emits
	// empty "apiVersion":"" and "kind":"" unless stripped.
	patchSts := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "jenkins",
							Env: []corev1.EnvVar{
								{Name: "MY_VAR", Value: "my-val"},
							},
						},
					},
				},
			},
		},
	}

	patchJSON, err := json.Marshal(patchSts)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}

	// Verify the marshaled patch does NOT contain apiVersion/kind (they're omitted
	// by the struct's `omitempty` tags when empty — this is good, not a hazard,
	// for the marshaled-from-typed-struct path).
	var patchMap map[string]interface{}
	if err := json.Unmarshal(patchJSON, &patchMap); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	_, hasAPIVersion := patchMap["apiVersion"]
	_, hasKind := patchMap["kind"]
	t.Logf("patch has apiVersion=%v, kind=%v (omitempty causes zero values to be omitted)", hasAPIVersion, hasKind)

	if hasAPIVersion || hasKind {
		t.Log("NOTE: patch unexpectedly contains apiVersion/kind; verify the struct tags.")
	}

	// Now apply this patch to a real base and verify apiVersion/kind are preserved.
	base := buildTestStatefulSet()
	origJSON, err := json.Marshal(base.Object)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}

	mergedJSON, err := strategicpatch.StrategicMergePatch(origJSON, patchJSON, appsv1.StatefulSet{})
	if err != nil {
		t.Fatalf("StrategicMergePatch failed: %v", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	apiVer, _ := merged["apiVersion"].(string)
	kind, _ := merged["kind"].(string)
	t.Logf("merged apiVersion = %q, kind = %q", apiVer, kind)

	// Finding: a patch built from a zero-value typed struct does NOT carry empty
	// apiVersion/kind (they are omitted by omitempty), so it does NOT blank the
	// base. However, a raw user overlay YAML that explicitly includes
	// `apiVersion: ""` or `kind: ""` WOULD blank those fields. CompilePodOverrides
	// is safe, but the Merge function should still strip these from raw overlays
	// as a defensive measure (design §1.2).
	if apiVer == "" || kind == "" {
		t.Errorf("TypeMeta would be blanked by a patch carrying empty apiVersion/kind; "+
			"this test uses a typed struct so omitempty protects us (apiVersion=%q, kind=%q)",
			apiVer, kind)
	} else {
		t.Log("OK: apiVersion/kind preserved. Typed struct marshaling with omitempty " +
			"is safe; raw overlays still need defensive stripping.")
	}
}
