package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// jenkinsContainerEnvValue returns the value of an env entry on the jenkins
// container of a built StatefulSet, and whether the entry is present.
func jenkinsContainerEnvValue(t *testing.T, sts *unstructured.Unstructured, name string) (string, bool) {
	t.Helper()
	containers, ok := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]map[string]interface{})
	if !ok {
		t.Fatal("containers not found on the built StatefulSet")
	}
	for _, cm := range containers {
		if cm["name"] != "jenkins" {
			continue
		}
		envs, _ := cm["env"].([]map[string]interface{})
		for _, em := range envs {
			if em["name"] == name {
				v, _ := em["value"].(string)
				return v, true
			}
		}
	}
	return "", false
}

// TestBuildStatefulSet_KubernetesJenkinsURLEnv asserts that a headless pod
// carries KUBERNETES_JENKINS_URL set to the in-cluster endpoint, and that a
// controller that is not headless carries none.
func TestBuildStatefulSet_KubernetesJenkinsURLEnv(t *testing.T) {
	const endpoint = "http://ci-00000000-svc.team-a.svc.cluster.local:8080"

	t.Run("headless pod gets the in-cluster endpoint", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
			Headless:          true,
			InClusterEndpoint: endpoint,
		})
		got, ok := jenkinsContainerEnvValue(t, sts, "KUBERNETES_JENKINS_URL")
		if !ok {
			t.Fatal("KUBERNETES_JENKINS_URL absent on a headless pod, want present")
		}
		if want := endpoint + "/"; got != want {
			t.Errorf("KUBERNETES_JENKINS_URL = %q, want %q", got, want)
		}
	})

	t.Run("non-headless pod has no KUBERNETES_JENKINS_URL", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
			InClusterEndpoint: endpoint,
		})
		if _, ok := jenkinsContainerEnvValue(t, sts, "KUBERNETES_JENKINS_URL"); ok {
			t.Error("KUBERNETES_JENKINS_URL present on a non-headless pod, want absent")
		}
	})
}
