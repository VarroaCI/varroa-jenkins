package controller

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// pluginsInitEnv returns the plugins-init init container's env entries in order,
// and whether the container carries an env key at all.
func pluginsInitEnv(t *testing.T, sts *unstructured.Unstructured) ([]map[string]interface{}, bool) {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	inits := podSpec["initContainers"].([]map[string]interface{})
	for _, c := range inits {
		if c["name"] != "plugins-init" {
			continue
		}
		env, ok := c["env"]
		if !ok {
			return nil, false
		}
		entries, _ := env.([]map[string]interface{})
		return entries, true
	}
	t.Fatal("plugins-init init container not found")
	return nil, false
}

// TestBuildStatefulSet_PluginsInitUpdateCenterEnv checks the plugins-init env
// names and order. The tool reads JENKINS_UC_EXPERIMENTAL for every exact pin,
// so it must carry the same URL as JENKINS_UC.
func TestBuildStatefulSet_PluginsInitUpdateCenterEnv(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:                          "ci-00000000",
		Namespace:                     "team-a",
		ControllerName:                "ci",
		PluginUpdateCenterURL:         "http://uc.ns.svc:8080/update-center.actual.json",
		PluginUpdateCenterDownloadURL: "http://uc.ns.svc:8080/download/plugins",
		PluginInfoURL:                 "http://uc.ns.svc:8080/plugin-versions.json",
	})
	env, ok := pluginsInitEnv(t, sts)
	if !ok {
		t.Fatal("plugins-init has no env")
	}
	got := make([]string, 0, len(env))
	values := map[string]string{}
	for _, e := range env {
		name, _ := e["name"].(string)
		value, _ := e["value"].(string)
		got = append(got, name)
		values[name] = value
	}
	want := []string{"JENKINS_UC", "JENKINS_UC_EXPERIMENTAL", "JENKINS_UC_DOWNLOAD_URL", "JENKINS_PLUGIN_INFO"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plugins-init env names = %v, want %v", got, want)
	}
	if values["JENKINS_UC_EXPERIMENTAL"] != values["JENKINS_UC"] {
		t.Errorf("JENKINS_UC_EXPERIMENTAL = %q, want the JENKINS_UC value %q", values["JENKINS_UC_EXPERIMENTAL"], values["JENKINS_UC"])
	}

	empty := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})
	if _, ok := pluginsInitEnv(t, empty); ok {
		t.Error("plugins-init must carry no env key when every update-center URL is empty")
	}
}
