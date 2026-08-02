package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

func builtContainerMap(t *testing.T, sts *unstructured.Unstructured, container string) map[string]interface{} {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	rawContainers := podSpec["containers"]
	switch containers := rawContainers.(type) {
	case []map[string]interface{}:
		for _, cm := range containers {
			if cm["name"] == container {
				return cm
			}
		}
	case []interface{}:
		for _, item := range containers {
			cm, _ := item.(map[string]interface{})
			if cm != nil && cm["name"] == container {
				return cm
			}
		}
	default:
		t.Fatalf("containers has unexpected type %T", rawContainers)
	}
	t.Fatalf("container %q not found", container)
	return nil
}

func probeMap(t *testing.T, container map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	raw, ok := container[key]
	if !ok {
		return nil
	}
	probe, _ := raw.(map[string]interface{})
	return probe
}

func int64Value(t *testing.T, value any) int64 {
	t.Helper()
	switch v := value.(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		t.Fatalf("expected numeric value, got %T (%v)", value, value)
		return 0
	}
}

func assertProbeHTTPGet(t *testing.T, probe map[string]interface{}, wantPath string) {
	t.Helper()
	httpGet, _ := probe["httpGet"].(map[string]interface{})
	if httpGet == nil {
		t.Fatalf("probe has no httpGet: %#v", probe)
	}
	if got := httpGet["path"]; got != wantPath {
		t.Fatalf("httpGet.path = %v, want %q", got, wantPath)
	}
	if got := httpGet["port"]; got != "http" {
		t.Fatalf("httpGet.port = %v, want %q", got, "http")
	}
	if got := httpGet["scheme"]; got != "HTTP" {
		t.Fatalf("httpGet.scheme = %v, want %q", got, "HTTP")
	}
}

func TestBuildStatefulSetProbesDefaultsAndOverrides(t *testing.T) {
	t.Run("nil probes renders defaults", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
		})
		jenkins := builtContainerMap(t, sts, overlay.JenkinsContainerName)

		startup := probeMap(t, jenkins, "startupProbe")
		if startup == nil {
			t.Fatal("startupProbe missing")
		}
		assertProbeHTTPGet(t, startup, "/login")
		if got := int64Value(t, startup["initialDelaySeconds"]); got != 10 {
			t.Fatalf("startup initialDelaySeconds = %d, want 10", got)
		}
		if got := int64Value(t, startup["periodSeconds"]); got != 10 {
			t.Fatalf("startup periodSeconds = %d, want 10", got)
		}
		if got := int64Value(t, startup["timeoutSeconds"]); got != 5 {
			t.Fatalf("startup timeoutSeconds = %d, want 5", got)
		}
		if got := int64Value(t, startup["failureThreshold"]); got != 30 {
			t.Fatalf("startup failureThreshold = %d, want 30", got)
		}
		if got := int64Value(t, startup["successThreshold"]); got != 1 {
			t.Fatalf("startup successThreshold = %d, want 1", got)
		}

		readiness := probeMap(t, jenkins, "readinessProbe")
		if readiness == nil {
			t.Fatal("readinessProbe missing")
		}
		assertProbeHTTPGet(t, readiness, "/login")
		if _, ok := readiness["initialDelaySeconds"]; ok {
			t.Fatal("readiness initialDelaySeconds should be omitted when default is 0")
		}
		if got := int64Value(t, readiness["periodSeconds"]); got != 10 {
			t.Fatalf("readiness periodSeconds = %d, want 10", got)
		}
		if got := int64Value(t, readiness["timeoutSeconds"]); got != 5 {
			t.Fatalf("readiness timeoutSeconds = %d, want 5", got)
		}
		if got := int64Value(t, readiness["failureThreshold"]); got != 3 {
			t.Fatalf("readiness failureThreshold = %d, want 3", got)
		}
		if got := int64Value(t, readiness["successThreshold"]); got != 1 {
			t.Fatalf("readiness successThreshold = %d, want 1", got)
		}

		liveness := probeMap(t, jenkins, "livenessProbe")
		if liveness == nil {
			t.Fatal("livenessProbe missing")
		}
		assertProbeHTTPGet(t, liveness, "/login")
		if _, ok := liveness["initialDelaySeconds"]; ok {
			t.Fatal("liveness initialDelaySeconds should be omitted when default is 0")
		}
		if got := int64Value(t, liveness["periodSeconds"]); got != 10 {
			t.Fatalf("liveness periodSeconds = %d, want 10", got)
		}
		if got := int64Value(t, liveness["timeoutSeconds"]); got != 5 {
			t.Fatalf("liveness timeoutSeconds = %d, want 5", got)
		}
		if got := int64Value(t, liveness["failureThreshold"]); got != 6 {
			t.Fatalf("liveness failureThreshold = %d, want 6", got)
		}
		if got := int64Value(t, liveness["successThreshold"]); got != 1 {
			t.Fatalf("liveness successThreshold = %d, want 1", got)
		}
	})

	t.Run("partial startup override only changes one field", func(t *testing.T) {
		override := int32(60)
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
			Probes: &v1alpha1.ProbesSpec{
				Startup: &v1alpha1.ProbeSpec{FailureThreshold: &override},
			},
		})
		startup := probeMap(t, builtContainerMap(t, sts, overlay.JenkinsContainerName), "startupProbe")
		if startup == nil {
			t.Fatal("startupProbe missing")
		}
		if got := int64Value(t, startup["failureThreshold"]); got != 60 {
			t.Fatalf("startup failureThreshold = %d, want 60", got)
		}
		if got := int64Value(t, startup["periodSeconds"]); got != 10 {
			t.Fatalf("startup periodSeconds = %d, want 10", got)
		}
		if got := int64Value(t, startup["timeoutSeconds"]); got != 5 {
			t.Fatalf("startup timeoutSeconds = %d, want 5", got)
		}
		if got := int64Value(t, startup["successThreshold"]); got != 1 {
			t.Fatalf("startup successThreshold = %d, want 1", got)
		}
		if got := int64Value(t, startup["initialDelaySeconds"]); got != 10 {
			t.Fatalf("startup initialDelaySeconds = %d, want 10", got)
		}
	})

	t.Run("disabled liveness omits only that probe", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
			Probes: &v1alpha1.ProbesSpec{
				Liveness: &v1alpha1.ProbeSpec{Disabled: true},
			},
		})
		jenkins := builtContainerMap(t, sts, overlay.JenkinsContainerName)
		if probeMap(t, jenkins, "startupProbe") == nil {
			t.Fatal("startupProbe missing")
		}
		if probeMap(t, jenkins, "readinessProbe") == nil {
			t.Fatal("readinessProbe missing")
		}
		if _, ok := jenkins["livenessProbe"]; ok {
			t.Fatal("livenessProbe should be omitted when disabled")
		}
	})

	t.Run("path prefix is honored", func(t *testing.T) {
		sts := buildStatefulSet(StatefulSetSpec{
			Name:           "ci-00000000",
			Namespace:      "team-a",
			ControllerName: "ci",
			PathPrefix:     "/j/ns/name",
		})
		jenkins := builtContainerMap(t, sts, overlay.JenkinsContainerName)
		for _, key := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
			assertProbeHTTPGet(t, probeMap(t, jenkins, key), "/j/ns/name/login")
		}
	})
}

func TestControllerSpecDeepCopyCopiesProbes(t *testing.T) {
	failure := int32(30)
	spec := v1alpha1.ControllerSpec{
		Namespace: "team-a",
		Probes: &v1alpha1.ProbesSpec{
			Startup: &v1alpha1.ProbeSpec{
				FailureThreshold: &failure,
			},
		},
	}
	cp := spec
	spec.DeepCopyInto(&cp)
	if cp.Probes == nil || cp.Probes.Startup == nil || cp.Probes.Startup.FailureThreshold == nil {
		t.Fatal("deep copy did not copy probes")
	}
	*cp.Probes.Startup.FailureThreshold = 99
	if got := *spec.Probes.Startup.FailureThreshold; got != 30 {
		t.Fatalf("original failureThreshold mutated to %d, want 30", got)
	}
}

func TestBuildStatefulSetMiteContainerHasNoPorts(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name:           "ci-00000000",
		Namespace:      "team-a",
		ControllerName: "ci",
	})
	mite := builtContainerMap(t, sts, overlay.MiteContainerName)
	if _, ok := mite["ports"]; ok {
		t.Fatalf("mite container should not declare ports: %#v", mite["ports"])
	}
}

func TestProbeDefaultTableMatchesCanonicalDefaults(t *testing.T) {
	tests := []struct {
		name string
		got  probeDefaults
		want probeDefaults
	}{
		{name: "startup", got: probeDefaultTable[probeStartup], want: probeDefaults{initialDelaySeconds: 10, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 30, successThreshold: 1}},
		{name: "readiness", got: probeDefaultTable[probeReadiness], want: probeDefaults{initialDelaySeconds: 0, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 3, successThreshold: 1}},
		{name: "liveness", got: probeDefaultTable[probeLiveness], want: probeDefaults{initialDelaySeconds: 0, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 6, successThreshold: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("probeDefaultTable[%s] = %#v, want %#v", tt.name, tt.got, tt.want)
			}
		})
	}
}
