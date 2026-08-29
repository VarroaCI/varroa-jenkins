package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// builtContainerEnv returns the env name->value map for the named container of
// a StatefulSet object rendered by buildStatefulSet.
func builtContainerEnv(t *testing.T, sts *unstructured.Unstructured, container string) map[string]string {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	containers := podSpec["containers"].([]map[string]interface{})
	for _, cm := range containers {
		if cm["name"] != container {
			continue
		}
		env := map[string]string{}
		entries, _ := cm["env"].([]map[string]interface{})
		for _, em := range entries {
			name, _ := em["name"].(string)
			value, _ := em["value"].(string)
			env[name] = value
		}
		return env
	}
	t.Fatalf("container %q not found", container)
	return nil
}

func TestBuildStatefulSetPathModeEnv(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:           "ci-00000000",
		Namespace:      "team-a",
		ControllerName: "ci",
		PathPrefix:     "/jenkins/team-a/ci",
	})

	jenkinsEnv := builtContainerEnv(t, sts, "jenkins")
	if got := jenkinsEnv["JENKINS_OPTS"]; got != "--prefix=/jenkins/team-a/ci" {
		t.Errorf("JENKINS_OPTS = %q, want --prefix=/jenkins/team-a/ci", got)
	}

	miteEnv := builtContainerEnv(t, sts, "mite")
	if got := miteEnv["JENKINS_URL"]; got != "http://localhost:8080/jenkins/team-a/ci" {
		t.Errorf("mite JENKINS_URL = %q, want http://localhost:8080/jenkins/team-a/ci", got)
	}
}

func TestBuildStatefulSetSubdomainModeEnv(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:           "ci-00000000",
		Namespace:      "team-a",
		ControllerName: "ci",
	})

	jenkinsEnv := builtContainerEnv(t, sts, "jenkins")
	if _, ok := jenkinsEnv["JENKINS_OPTS"]; ok {
		t.Errorf("JENKINS_OPTS should not be set in subdomain mode, got %q", jenkinsEnv["JENKINS_OPTS"])
	}

	miteEnv := builtContainerEnv(t, sts, "mite")
	if got := miteEnv["JENKINS_URL"]; got != "http://localhost:8080" {
		t.Errorf("mite JENKINS_URL = %q, want http://localhost:8080", got)
	}
}

func TestBuildStatefulSetFleetPodLabel(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})

	podLabels, found, err := unstructured.NestedStringMap(sts.Object, "spec", "template", "metadata", "labels")
	if err != nil || !found {
		t.Fatalf("get pod template labels: found=%t err=%v", found, err)
	}
	if got := podLabels["app.kubernetes.io/managed-by"]; got != "varroa-operator" {
		t.Errorf("pod template managed-by label = %q, want varroa-operator", got)
	}

	selector, found, err := unstructured.NestedStringMap(sts.Object, "spec", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("get selector labels: found=%t err=%v", found, err)
	}
	if len(selector) != 1 || selector["app"] != "ci-00000000" {
		t.Errorf("selector labels = %#v, want only app=ci-00000000", selector)
	}

	stsLabels := sts.GetLabels()
	if len(stsLabels) != 1 || stsLabels["app"] != "ci-00000000" {
		t.Errorf("StatefulSet metadata labels = %#v, want only app=ci-00000000", stsLabels)
	}
}

func TestBuildStatefulSetBannerUsesClusterAwareControllerPath(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:           "ci-00000000",
		Namespace:      "team-a",
		ControllerName: "ci",
		ClusterName:    "dev-cluster",
		VarroaBaseURL:  "https://varroa.example.com",
	})

	bannerURL := builtContainerEnv(t, sts, "jenkins")["VARROA_BANNER_URL"]
	want := "back=https://varroa.example.com/controllers/dev-cluster/team-a/ci"
	if !strings.Contains(bannerURL, want) {
		t.Errorf("VARROA_BANNER_URL = %q, want it to contain %q", bannerURL, want)
	}
}

// TestBuildStatefulSetEnablesSystemRead asserts the jenkins container runs with
// -Djenkins.security.SystemReadPermission=true. Without it the SYSTEM_READ
// permission is disabled and the varroa:system-mite SystemRead grant is inert,
// 403ing the mite's drift-baseline /configuration-as-code/export.
func TestBuildStatefulSetEnablesSystemRead(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:           "ci-00000000",
		Namespace:      "team-a",
		ControllerName: "ci",
	})

	javaOpts := builtContainerEnv(t, sts, "jenkins")["JAVA_OPTS"]
	if !strings.Contains(javaOpts, "-Djenkins.security.SystemReadPermission=true") {
		t.Errorf("jenkins JAVA_OPTS = %q, want it to enable SystemReadPermission", javaOpts)
	}
}

// builtContainerPreStop returns the preStop exec command (joined) for the named
// container of a StatefulSet rendered by buildStatefulSet.
func builtContainerPreStop(t *testing.T, sts *unstructured.Unstructured, container string) string {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	containers := podSpec["containers"].([]map[string]interface{})
	for _, cm := range containers {
		if cm["name"] != container {
			continue
		}
		lc, _ := cm["lifecycle"].(map[string]interface{})
		if lc == nil {
			t.Fatalf("container %q has no lifecycle", container)
		}
		cmd := lc["preStop"].(map[string]interface{})["exec"].(map[string]interface{})["command"].([]string)
		return strings.Join(cmd, " ")
	}
	t.Fatalf("container %q not found", container)
	return ""
}

// builtContainerVolumeMounts returns the volumeMounts slice for the named
// container of a StatefulSet rendered by buildStatefulSet.
func builtContainerVolumeMounts(t *testing.T, sts *unstructured.Unstructured, container string) []map[string]interface{} {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	containers := podSpec["containers"].([]map[string]interface{})
	for _, cm := range containers {
		if cm["name"] != container {
			continue
		}
		mounts, _ := cm["volumeMounts"].([]map[string]interface{})
		return mounts
	}
	t.Fatalf("container %q not found", container)
	return nil
}

// builtVolumes returns the volumes slice from a StatefulSet rendered by
// buildStatefulSet.
func builtVolumes(t *testing.T, sts *unstructured.Unstructured) []map[string]interface{} {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	vols, _ := podSpec["volumes"].([]map[string]interface{})
	return vols
}

// TestBuildStatefulSetPreStopDrainBounded asserts the new file-poll preStop
// render: the jenkins preStop polls /var/run/varroa/run/drain.done with a
// deadline of DrainTimeoutSec+15, contains no curl/quietDown/computer/queue,
// and both containers mount the varroa-run emptyDir volume.
func TestBuildStatefulSetPreStopDrainBounded(t *testing.T) {
	const drainSec int64 = 900
	sts := buildStatefulSet(StatefulSetSpec{
		Name:            "ci-00000000",
		Namespace:       "team-a",
		ControllerName:  "ci",
		DrainTimeoutSec: drainSec,
	})
	cmd := builtContainerPreStop(t, sts, "jenkins")

	// Must reference the marker file.
	if !strings.Contains(cmd, "/var/run/varroa/run/drain.done") {
		t.Errorf("preStop must poll for drain.done marker, got:\n%s", cmd)
	}
	// Must contain deadline arithmetic with DrainTimeoutSec+15.
	expectedDeadline := fmt.Sprintf("%d", drainSec+15)
	if !strings.Contains(cmd, expectedDeadline) {
		t.Errorf("preStop deadline must include %d (DrainTimeoutSec+15), got:\n%s", drainSec+15, cmd)
	}
	// Must NOT contain any authenticated/jenkins HTTP calls.
	for _, bad := range []string{"curl", "quietDown", "computer", "queue"} {
		if strings.Contains(cmd, bad) {
			t.Errorf("preStop must not contain %q, got:\n%s", bad, cmd)
		}
	}

	// Both containers must mount varroa-run at /var/run/varroa/run.
	for _, cn := range []string{"jenkins", "mite"} {
		mounts := builtContainerVolumeMounts(t, sts, cn)
		found := false
		for _, m := range mounts {
			if m["name"] == "varroa-run" && m["mountPath"] == "/var/run/varroa/run" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("container %q must mount varroa-run at /var/run/varroa/run, got mounts: %v", cn, mounts)
		}
	}

	// A varroa-run emptyDir volume must exist.
	vols := builtVolumes(t, sts)
	foundVol := false
	for _, v := range vols {
		if v["name"] == "varroa-run" {
			if _, ok := v["emptyDir"]; !ok {
				t.Errorf("varroa-run volume must be emptyDir")
			}
			foundVol = true
			break
		}
	}
	if !foundVol {
		t.Errorf("varroa-run emptyDir volume must exist, got volumes: %v", vols)
	}
}

func TestCreateIngressPathRule(t *testing.T) {
	t.Run("path mode", func(t *testing.T) {
		c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
		err := c.CreateIngress(context.Background(), "ci-ingress", "team-a",
			"varroa.example.com", "/jenkins/team-a/ci", "", "nginx", nil, "")
		if err != nil {
			t.Fatalf("CreateIngress: %v", err)
		}
		ing, err := c.clientset.NetworkingV1().Ingresses("team-a").Get(context.Background(), "ci-ingress", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get ingress: %v", err)
		}
		paths := ing.Spec.Rules[0].HTTP.Paths
		if len(paths) != 1 || paths[0].Path != "/jenkins/team-a/ci" {
			t.Errorf("path = %v, want single /jenkins/team-a/ci", paths)
		}
		if len(ing.Spec.TLS) != 0 {
			t.Errorf("TLS block should be absent in path mode, got %v", ing.Spec.TLS)
		}
	})

	t.Run("subdomain mode unchanged", func(t *testing.T) {
		c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
		err := c.CreateIngress(context.Background(), "ci-ingress", "team-a",
			"ci.example.com", "", "ci-tls", "nginx", nil, "")
		if err != nil {
			t.Fatalf("CreateIngress: %v", err)
		}
		ing, err := c.clientset.NetworkingV1().Ingresses("team-a").Get(context.Background(), "ci-ingress", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get ingress: %v", err)
		}
		paths := ing.Spec.Rules[0].HTTP.Paths
		if len(paths) != 1 || paths[0].Path != "/" {
			t.Errorf("path = %v, want single /", paths)
		}
		if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "ci-tls" {
			t.Errorf("TLS = %v, want secret ci-tls", ing.Spec.TLS)
		}
	})
}

// TestCreateIngressUpdatesExisting verifies CreateIngress reconciles an already
// existing Ingress instead of leaving it stale: a second call that adds
// TLS and an annotation must patch the live object, while preserving metadata
// (resourceVersion, ownerReferences) and annotations added by other controllers.
func TestCreateIngressUpdatesExisting(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	// Initial create: HTTP-only, no TLS.
	if err := c.CreateIngress(ctx, "ci-ingress", "team-a",
		"ci.example.com", "", "", "nginx", nil, ""); err != nil {
		t.Fatalf("initial CreateIngress: %v", err)
	}

	// Simulate metadata owned/added out of band: an ownerReference and a
	// foreign annotation set by an external controller.
	ing, err := c.clientset.NetworkingV1().Ingresses("team-a").Get(ctx, "ci-ingress", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	ing.OwnerReferences = []metav1.OwnerReference{{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "ci"}}
	ing.Annotations = map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt"}
	if _, err := c.clientset.NetworkingV1().Ingresses("team-a").Update(ctx, ing, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	// Reconcile: add TLS secret and a managed annotation.
	if err := c.CreateIngress(ctx, "ci-ingress", "team-a",
		"ci.example.com", "", "ci-tls", "nginx",
		map[string]string{"nginx.ingress.kubernetes.io/ssl-redirect": "true"}, ""); err != nil {
		t.Fatalf("reconcile CreateIngress: %v", err)
	}

	got, err := c.clientset.NetworkingV1().Ingresses("team-a").Get(ctx, "ci-ingress", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress after reconcile: %v", err)
	}
	if len(got.Spec.TLS) != 1 || got.Spec.TLS[0].SecretName != "ci-tls" {
		t.Errorf("TLS = %v, want secret ci-tls", got.Spec.TLS)
	}
	if got.Annotations["nginx.ingress.kubernetes.io/ssl-redirect"] != "true" {
		t.Errorf("managed annotation not applied: %v", got.Annotations)
	}
	if got.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("foreign annotation was clobbered: %v", got.Annotations)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "ci" {
		t.Errorf("ownerReferences not preserved: %v", got.OwnerReferences)
	}
}

// TestBuildStatefulSetJvmOptsAppendsToJAVA_OPTS verifies that jvmOpts from
// PodOverrides is appended (space-joined) to the baseline JAVA_OPTS value,
// and that an empty jvmOpts leaves the baseline unchanged.
func TestBuildStatefulSetJvmOptsAppendsToJAVA_OPTS(t *testing.T) {
	t.Run("jvmOpts set appends to baseline", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
			PodOverrides: &v1alpha1.PodOverrides{
				JvmOpts: "-Xmx2g",
			},
		})
		javaOpts := builtContainerEnv(t, sts, "jenkins")["JAVA_OPTS"]
		if !strings.Contains(javaOpts, "-Djenkins.install.runSetupWizard=false") {
			t.Errorf("JAVA_OPTS missing baseline flag, got: %q", javaOpts)
		}
		if !strings.Contains(javaOpts, "-Xmx2g") {
			t.Errorf("JAVA_OPTS missing jvmOpts '-Xmx2g', got: %q", javaOpts)
		}
	})

	t.Run("empty jvmOpts leaves baseline unchanged", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
		})
		javaOpts := builtContainerEnv(t, sts, "jenkins")["JAVA_OPTS"]
		expected := "-Djenkins.install.runSetupWizard=false -Djenkins.security.SystemReadPermission=true"
		if javaOpts != expected {
			t.Errorf("JAVA_OPTS = %q, want %q", javaOpts, expected)
		}
	})

	t.Run("jvmOpts with PodOverrides nil", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
			PodOverrides:   nil,
		})
		javaOpts := builtContainerEnv(t, sts, "jenkins")["JAVA_OPTS"]
		expected := "-Djenkins.install.runSetupWizard=false -Djenkins.security.SystemReadPermission=true"
		if javaOpts != expected {
			t.Errorf("JAVA_OPTS = %q, want %q", javaOpts, expected)
		}
	})
}
