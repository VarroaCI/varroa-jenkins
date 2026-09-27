package controller

import (
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// clearCellsEnv unsets every env var ParseCellsConfigFromEnv reads, so each
// test starts from a clean slate regardless of the OS environment.
func clearCellsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"VARROA_CELLS_WARM_POOL_ENABLED",
		"VARROA_CELLS_WARM_POOL_PER_PROFILE",
		"VARROA_CELLS_MAX_TENANTS",
		"VARROA_CELLS_UNREADY_GRACE",
		"VARROA_NETWORKPOLICY_ENABLED",
		"VARROA_NETWORKPOLICY_APISERVER_EGRESS",
		"VARROA_CELL_ISOLATION",
		"VARROA_CELLS_CELL_TEMPLATE",
	} {
		t.Setenv(key, "")
	}
}

func TestParseCellsConfigFromEnv_Defaults(t *testing.T) {
	clearCellsEnv(t)

	got := ParseCellsConfigFromEnv(slog.Default())

	want := CellsConfig{
		WarmPoolEnabled:    false,
		WarmPoolPerProfile: 1,
		MaxTenantsPerCell:  10,
		UnreadyGracePeriod: 5 * time.Minute,
		CellTemplate:       defaultCellTemplate(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseCellsConfigFromEnv() defaults = %+v, want %+v", got, want)
	}
}

func TestParseCellsConfigFromEnv_JSONFields(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELLS_WARM_POOL_ENABLED", "true")
	t.Setenv("VARROA_CELLS_WARM_POOL_PER_PROFILE", "3")
	t.Setenv("VARROA_CELLS_MAX_TENANTS", "25")
	t.Setenv("VARROA_CELLS_UNREADY_GRACE", "10m")
	t.Setenv("VARROA_NETWORKPOLICY_ENABLED", "true")
	t.Setenv("VARROA_NETWORKPOLICY_APISERVER_EGRESS", `{"cidrs":["10.0.0.0/8"],"ports":[443,6443]}`)
	t.Setenv("VARROA_CELL_ISOLATION", `{"enabled":true,"extraIngress":[{"ports":[{"port":8080}]}],"extraEgress":[]}`)

	got := ParseCellsConfigFromEnv(slog.Default())

	if !got.WarmPoolEnabled {
		t.Error("WarmPoolEnabled = false, want true")
	}
	if got.WarmPoolPerProfile != 3 {
		t.Errorf("WarmPoolPerProfile = %d, want 3", got.WarmPoolPerProfile)
	}
	if got.MaxTenantsPerCell != 25 {
		t.Errorf("MaxTenantsPerCell = %d, want 25", got.MaxTenantsPerCell)
	}
	if got.UnreadyGracePeriod != 10*time.Minute {
		t.Errorf("UnreadyGracePeriod = %v, want 10m", got.UnreadyGracePeriod)
	}
	if !got.NetworkPolicyEnabled {
		t.Error("NetworkPolicyEnabled = false, want true")
	}

	wantEgress := APIServerEgress{CIDRs: []string{"10.0.0.0/8"}, Ports: []int32{443, 6443}}
	if got.APIServerEgress.CIDRs[0] != wantEgress.CIDRs[0] || len(got.APIServerEgress.Ports) != 2 {
		t.Errorf("APIServerEgress = %+v, want %+v", got.APIServerEgress, wantEgress)
	}

	if !got.CellIsolation.Enabled {
		t.Error("CellIsolation.Enabled = false, want true")
	}
	if len(got.CellIsolation.ExtraIngress) != 1 {
		t.Fatalf("CellIsolation.ExtraIngress = %+v, want 1 entry", got.CellIsolation.ExtraIngress)
	}
	wantPort := networkingv1.NetworkPolicyPort{Port: got.CellIsolation.ExtraIngress[0].Ports[0].Port}
	if wantPort.Port == nil || wantPort.Port.IntValue() != 8080 {
		t.Errorf("CellIsolation.ExtraIngress[0].Ports[0].Port = %v, want 8080", got.CellIsolation.ExtraIngress[0].Ports[0].Port)
	}
	if len(got.CellIsolation.ExtraEgress) != 0 {
		t.Errorf("CellIsolation.ExtraEgress = %+v, want empty", got.CellIsolation.ExtraEgress)
	}
}

func TestParseCellsConfigFromEnv_BadDuration(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELLS_UNREADY_GRACE", "not-a-duration")

	got := ParseCellsConfigFromEnv(slog.Default())

	if got.UnreadyGracePeriod != 5*time.Minute {
		t.Errorf("UnreadyGracePeriod = %v, want fallback 5m on parse error", got.UnreadyGracePeriod)
	}
}

func TestParseCellsConfigFromEnv_BadJSON(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELL_ISOLATION", "{not valid json")

	got := ParseCellsConfigFromEnv(slog.Default())

	if got.CellIsolation.Enabled {
		t.Error("CellIsolation.Enabled = true, want fallback zero value on parse error")
	}
}

func TestParseCellsConfigFromEnv_PerProfileFloorIsOne(t *testing.T) {
	t.Setenv("VARROA_CELLS_WARM_POOL_PER_PROFILE", "0")
	got := ParseCellsConfigFromEnv(slog.Default())
	if got.WarmPoolPerProfile != 1 {
		t.Errorf("WarmPoolPerProfile = %d, want 1", got.WarmPoolPerProfile)
	}
}

// The default template must carry the exact numbers the chart default
// carries, because the operator falls back to it when the variable is absent.
func TestDefaultCellTemplate_Values(t *testing.T) {
	tmpl := defaultCellTemplate()
	if tmpl == nil || tmpl.Resources == nil || tmpl.MiteResources == nil || tmpl.PodOverrides == nil {
		t.Fatalf("defaultCellTemplate() = %+v, want resources, miteResources, and podOverrides", tmpl)
	}
	wantJenkins := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("3Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
	}
	if !equality.Semantic.DeepEqual(*tmpl.Resources, wantJenkins) {
		t.Errorf("resources = %+v, want %+v", *tmpl.Resources, wantJenkins)
	}
	if _, ok := tmpl.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("resources carries a CPU limit, want none")
	}
	wantMite := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	if !equality.Semantic.DeepEqual(*tmpl.MiteResources, wantMite) {
		t.Errorf("miteResources = %+v, want %+v", *tmpl.MiteResources, wantMite)
	}
	if tmpl.PodOverrides.JvmOpts != "-XX:MaxRAMPercentage=50" {
		t.Errorf("jvmOpts = %q, want -XX:MaxRAMPercentage=50", tmpl.PodOverrides.JvmOpts)
	}
	if len(tmpl.PodOverrides.PodLabels) != 0 {
		t.Errorf("podLabels = %v, want none", tmpl.PodOverrides.PodLabels)
	}
	aff := tmpl.PodOverrides.Affinity
	if aff == nil || aff.PodAntiAffinity == nil || len(aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("affinity = %+v, want one preferred anti-affinity term", aff)
	}
	if len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("affinity carries a required term, want preferred only")
	}
	term := aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0]
	if term.Weight != 100 || term.PodAffinityTerm.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("term = %+v, want weight 100 and topologyKey kubernetes.io/hostname", term)
	}
	sel := term.PodAffinityTerm.LabelSelector
	if sel == nil || len(sel.MatchExpressions) != 1 || sel.MatchExpressions[0].Key != "varroa.dev/cell-pool" ||
		sel.MatchExpressions[0].Operator != "Exists" {
		t.Errorf("labelSelector = %+v, want varroa.dev/cell-pool Exists", sel)
	}
}

// The Go default and the chart default are two copies of one value. This
// test reads the chart value, sends it through the same parse the operator
// runs, and compares the result with the Go default.
func TestDefaultCellTemplate_MatchesChartDefault(t *testing.T) {
	raw, err := os.ReadFile("../../charts/varroa/values.yaml")
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	var values struct {
		Cells struct {
			CellTemplate json.RawMessage `json:"cellTemplate"`
		} `json:"cells"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	if len(values.Cells.CellTemplate) == 0 {
		t.Fatal("chart values carry no cells.cellTemplate")
	}
	clearCellsEnv(t)
	t.Setenv("VARROA_CELLS_CELL_TEMPLATE", string(values.Cells.CellTemplate))
	got := ParseCellsConfigFromEnv(slog.Default()).CellTemplate
	if !equality.Semantic.DeepEqual(got, defaultCellTemplate()) {
		t.Errorf("chart cells.cellTemplate parses to %+v, want the Go default %+v", got, defaultCellTemplate())
	}
}

func TestParseCellsConfigFromEnv_CellTemplate(t *testing.T) {
	custom := &v1alpha1.CellTemplate{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
		},
	}
	cases := []struct {
		name  string
		value string
		want  *v1alpha1.CellTemplate
	}{
		{name: "absent", value: "", want: defaultCellTemplate()},
		{name: "empty object opts out", value: "{}", want: nil},
		{name: "null opts out", value: "null", want: nil},
		{name: "known keys all null opt out", value: `{"resources":null,"podOverrides":null}`, want: nil},
		{name: "custom template", value: `{"resources":{"requests":{"memory":"8Gi"}}}`, want: custom},
		{name: "invalid JSON falls back", value: "{not json", want: defaultCellTemplate()},
		{name: "trailing data falls back", value: `{"resources":{"requests":{"memory":"8Gi"}}} {}`, want: defaultCellTemplate()},
		{name: "unknown key falls back", value: `{"resource":{"requests":{"memory":"8Gi"}}}`, want: defaultCellTemplate()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearCellsEnv(t)
			t.Setenv("VARROA_CELLS_CELL_TEMPLATE", tc.value)
			got := ParseCellsConfigFromEnv(slog.Default()).CellTemplate
			if !equality.Semantic.DeepEqual(got, tc.want) {
				t.Errorf("CellTemplate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Each parse returns its own value, so a caller that edits the template
// cannot change the default another caller reads.
func TestDefaultCellTemplate_FreshValue(t *testing.T) {
	a := defaultCellTemplate()
	a.PodOverrides.JvmOpts = "changed"
	if defaultCellTemplate().PodOverrides.JvmOpts == "changed" {
		t.Error("defaultCellTemplate() shares state between calls")
	}
}
