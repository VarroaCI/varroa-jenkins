package overlay

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestScanProtected_MiteSidecar(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "mite",
							"env": []interface{}{
								map[string]interface{}{"name": "VARROA_ENDPOINT", "value": "https://other"},
							},
						},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	if len(warns) == 0 {
		t.Fatal("expected at least 1 warning (mite sidecar)")
	}
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Path == "spec.template.spec.containers[0]" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected mite sidecar warning, got: %+v", warns)
	}
}

func TestScanProtected_VarroaEnv(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "jenkins",
							"env": []interface{}{
								map[string]interface{}{"name": "VARROA_OIDC_ISSUER", "value": "https://evil"},
							},
						},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	if len(warns) == 0 {
		t.Fatal("expected at least 1 warning (VARROA_ env)")
	}
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Message == "overrides operator-managed env var VARROA_OIDC_ISSUER" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected VARROA_OIDC_ISSUER warning, got: %+v", warns)
	}
}

func TestScanProtected_JAVA_OPTS(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "jenkins",
							"env": []interface{}{
								map[string]interface{}{"name": "JAVA_OPTS", "value": "-Xmx2g"},
							},
						},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Message == "overrides operator-managed env var JAVA_OPTS" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected JAVA_OPTS warning, got: %+v", warns)
	}
}

func TestScanProtected_CASC_JENKINS_CONFIG(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "jenkins",
							"env": []interface{}{
								map[string]interface{}{"name": "CASC_JENKINS_CONFIG", "value": "/custom"},
							},
						},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Message == "overrides operator-managed env var CASC_JENKINS_CONFIG" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CASC_JENKINS_CONFIG warning, got: %+v", warns)
	}
}

func TestScanProtected_VolumeManaged(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"volumes": []interface{}{
						map[string]interface{}{"name": "jenkins-home", "emptyDir": map[string]interface{}{}},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Message == "overrides operator-managed volume jenkins-home" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected jenkins-home volume warning, got: %+v", warns)
	}
}

func TestScanProtected_ServiceSelector(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"app": "other"},
		},
	}
	warns := ScanProtected(nil, patch, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "service" && w.Path == "spec.selector" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected service selector warning, got: %+v", warns)
	}
}

func TestScanProtected_IngressOwnerReferences(t *testing.T) {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"ownerReferences": []interface{}{
				map[string]interface{}{"name": "other"},
			},
		},
	}
	warns := ScanProtected(nil, nil, patch)
	found := false
	for _, w := range warns {
		if w.Resource == "ingress" && w.Path == "metadata.ownerReferences" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ingress ownerReferences warning, got: %+v", warns)
	}
}

func TestScanProtected_BenignPatchNoWarnings(t *testing.T) {
	// User adds a custom non-managed env var, a custom volume, and pod annotations.
	stsPatch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{"my-label": "v"},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{"my-annotation": "v"},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "jenkins",
							"env": []interface{}{
								map[string]interface{}{"name": "MY_CUSTOM_VAR", "value": "safe"},
							},
						},
					},
					"volumes": []interface{}{
						map[string]interface{}{"name": "my-custom-vol", "emptyDir": map[string]interface{}{}},
					},
				},
			},
		},
	}
	warns := ScanProtected(stsPatch, nil, nil)
	if len(warns) > 0 {
		t.Errorf("expected no warnings for benign patch, got: %+v", warns)
	}
}

func TestScanProtected_BenignServiceNoWarnings(t *testing.T) {
	svcPatch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{"my-anno": "v"},
		},
	}
	warns := ScanProtected(nil, svcPatch, nil)
	if len(warns) > 0 {
		t.Errorf("expected no warnings for benign service patch, got: %+v", warns)
	}
}

func TestScanProtected_OwnerReferences(t *testing.T) {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"ownerReferences": []interface{}{},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Path == "metadata.ownerReferences" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected statefulSet ownerReferences warning, got: %+v", warns)
	}
}

func TestScanProtected_Replicas(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": float64(3),
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Path == "spec.replicas" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected replicas warning, got: %+v", warns)
	}
}

func TestScanProtected_InitContainerManaged(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"initContainers": []interface{}{
						map[string]interface{}{
							"name":  "plugins-init",
							"image": "custom:tag",
						},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && w.Message == "overrides the operator-managed init container plugins-init" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected plugins-init warning, got: %+v", warns)
	}
}

func TestScanProtected_ServiceManagedPort(t *testing.T) {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{"name": "http", "port": float64(9090)},
			},
		},
	}
	warns := ScanProtected(nil, patch, nil)
	found := false
	for _, w := range warns {
		if w.Resource == "service" && w.Message == "overrides an operator-managed Service port" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected managed port warning, got: %+v", warns)
	}
}

func TestScanProtected_ContainerRename(t *testing.T) {
	// Index 0 should be "jenkins", index 1 should be "mite".
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "not-jenkins"},
						map[string]interface{}{"name": "not-mite"},
					},
				},
			},
		},
	}
	warns := ScanProtected(patch, nil, nil)
	jenkinsRename := false
	miteRename := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && strings.Contains(w.Message, "renames the jenkins container") {
			jenkinsRename = true
		}
		if w.Resource == "statefulSet" && strings.Contains(w.Message, "renames the mite container") {
			miteRename = true
		}
	}
	if !jenkinsRename {
		t.Errorf("expected jenkins rename warning at index 0")
	}
	if !miteRename {
		t.Errorf("expected mite rename warning at index 1")
	}
}

func TestScanProtected_NilPatches(t *testing.T) {
	warns := ScanProtected(nil, nil, nil)
	if len(warns) != 0 {
		t.Errorf("expected no warnings for nil patches, got %d", len(warns))
	}
}

func TestScanOverrides_ProtectedEditReturnsWarning(t *testing.T) {
	// PodOverrides touching the mite container => warning.
	po := &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "VARROA_ENDPOINT", Value: "https://evil"},
		},
	}
	warns, err := ScanOverrides(po, nil)
	if err != nil {
		t.Fatalf("ScanOverrides failed: %v", err)
	}
	if len(warns) == 0 {
		t.Fatal("expected at least 1 warning for VARROA_ env on jenkins container")
	}
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && strings.Contains(w.Message, "VARROA_ENDPOINT") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected VARROA_ENDPOINT warning, got: %+v", warns)
	}
}

func TestScanOverrides_BenignEditNoWarnings(t *testing.T) {
	po := &v1alpha1.PodOverrides{
		Env: []corev1.EnvVar{
			{Name: "MY_CUSTOM_VAR", Value: "safe"},
		},
	}
	warns, err := ScanOverrides(po, nil)
	if err != nil {
		t.Fatalf("ScanOverrides failed: %v", err)
	}
	if len(warns) > 0 {
		t.Errorf("expected no warnings for benign edit, got: %+v", warns)
	}
}

func TestScanOverrides_NilNil(t *testing.T) {
	warns, err := ScanOverrides(nil, nil)
	if err != nil {
		t.Fatalf("ScanOverrides(nil, nil): %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings for nil/nil, got %d", len(warns))
	}
}

func TestScanOverrides_MalformedOverlayReturnsError(t *testing.T) {
	ro := &v1alpha1.ResourceOverlay{
		StatefulSet: "{[bad yaml",
	}
	_, err := ScanOverrides(nil, ro)
	if err == nil {
		t.Fatal("expected error for malformed overlay YAML")
	}
	t.Logf("malformed overlay error: %v", err)
}

func TestScanOverrides_ResourceOverlayMiteWarning(t *testing.T) {
	// Raw overlay touching the mite container.
	ro := &v1alpha1.ResourceOverlay{
		StatefulSet: `
spec:
  template:
    spec:
      containers:
        - name: mite
          env:
            - name: VARROA_ENDPOINT
              value: override
`,
	}
	warns, err := ScanOverrides(nil, ro)
	if err != nil {
		t.Fatalf("ScanOverrides failed: %v", err)
	}
	found := false
	for _, w := range warns {
		if w.Resource == "statefulSet" && strings.Contains(w.Message, "mite sidecar") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected mite sidecar warning, got: %+v", warns)
	}
}
