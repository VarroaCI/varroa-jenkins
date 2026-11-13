package assistantconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	sigsyaml "sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
)

// labSpec returns a spec with one keyless provider named lab.
func labSpec() v1alpha1.AssistantConfigSpec {
	return v1alpha1.AssistantConfigSpec{
		Providers: []v1alpha1.AssistantProvider{{
			Name: "lab", Kind: v1alpha1.AssistantProviderKindOpenAICompatible, BaseURL: "http://192.0.2.1:8082/v1",
			Models: []string{"m1", "m2"}, Trust: v1alpha1.AssistantTrustInternal, Auth: v1alpha1.AssistantAuthNone,
			CIDRs: []string{"192.0.2.1/32"}, Port: 8082,
		}},
		DefaultProvider:     "lab",
		DefaultModel:        "m2",
		ExternalDataSharing: "logs",
		Budgets:             v1alpha1.AssistantBudgets{Session: 1000, UserDay: 2000, TenantDay: 3000, Steps: 4},
		ApprovalExpiry:      "2h",
	}
}

func TestFromSpecMapsEveryField(t *testing.T) {
	t.Parallel()
	c, err := FromSpec(labSpec())
	if err != nil {
		t.Fatalf("FromSpec: %v", err)
	}
	if len(c.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(c.Providers))
	}
	e := c.Providers[0]
	if e.Name != "lab" || e.Kind != agent.ProviderOpenAICompatible || e.BaseURL != "http://192.0.2.1:8082/v1" ||
		e.Trust != agent.TrustInternal || !e.Default || e.KeyRequired {
		t.Fatalf("endpoint = %+v", e)
	}
	if c.DefaultProvider != "lab" || c.DefaultModel != "m2" || c.ModelFor("lab") != "m2" {
		t.Fatalf("default = %q/%q, model for lab = %q", c.DefaultProvider, c.DefaultModel, c.ModelFor("lab"))
	}
	if c.ExternalDataSharing != agent.DataLogs {
		t.Fatalf("sharing = %q", c.ExternalDataSharing)
	}
	if c.Budgets != (provider.Budgets{Session: 1000, UserDay: 2000, TenantDay: 3000, Steps: 4}) {
		t.Fatalf("budgets = %+v", c.Budgets)
	}
	if c.ApprovalExpiry != 2*time.Hour {
		t.Fatalf("expiry = %s", c.ApprovalExpiry)
	}
}

func TestFromSpecTakesDefaultsForZeroValues(t *testing.T) {
	t.Parallel()
	c, err := FromSpec(v1alpha1.AssistantConfigSpec{})
	if err != nil {
		t.Fatalf("FromSpec: %v", err)
	}
	if c.Budgets.Session != v1alpha1.AssistantDefaultSessionBudget || c.Budgets.Steps != int(v1alpha1.AssistantDefaultSteps) {
		t.Fatalf("budgets = %+v, want the defaults", c.Budgets)
	}
	if c.ExternalDataSharing != agent.DataMetadata || c.ApprovalExpiry != 24*time.Hour {
		t.Fatalf("sharing = %q, expiry = %s", c.ExternalDataSharing, c.ApprovalExpiry)
	}
}

func TestFromSpecRefusesBrokenSpec(t *testing.T) {
	t.Parallel()
	bad := map[string]func(s *v1alpha1.AssistantConfigSpec){
		"default names no listed model": func(s *v1alpha1.AssistantConfigSpec) { s.DefaultModel = "nope" },
		"default provider unknown":      func(s *v1alpha1.AssistantConfigSpec) { s.DefaultProvider = "ghost" },
		"bad expiry":                    func(s *v1alpha1.AssistantConfigSpec) { s.ApprovalExpiry = "soon" },
		"bad sharing":                   func(s *v1alpha1.AssistantConfigSpec) { s.ExternalDataSharing = "all" },
	}
	for name, mutate := range bad {
		s := labSpec()
		mutate(&s)
		if _, err := FromSpec(s); err == nil {
			t.Errorf("%s: FromSpec accepted the spec", name)
		}
	}
}

func TestBuildAbsentObjectUsesDefaults(t *testing.T) {
	t.Parallel()
	snap := Build(nil, factories.DefaultFactories(), t.TempDir())
	if snap.Err != nil || snap.ResourceVersion != "" {
		t.Fatalf("snapshot err = %v, rv = %q", snap.Err, snap.ResourceVersion)
	}
	if len(snap.Config.Providers) != 0 || snap.Registry == nil {
		t.Fatalf("providers = %d, registry nil = %v", len(snap.Config.Providers), snap.Registry == nil)
	}
	if snap.Config.Budgets.UserDay != v1alpha1.AssistantDefaultUserDayBudget {
		t.Fatalf("user day budget = %d, want the default", snap.Config.Budgets.UserDay)
	}
}

func TestBuildKeepsErrorWithNoProviders(t *testing.T) {
	t.Parallel()
	s := labSpec()
	s.ApprovalExpiry = "soon"
	obj := &v1alpha1.AssistantConfig{Spec: s, ObjectMeta: metav1.ObjectMeta{ResourceVersion: "9"}}
	snap := Build(obj, factories.DefaultFactories(), t.TempDir())
	if snap.Err == nil {
		t.Fatal("a broken object built without an error")
	}
	if len(snap.Config.Providers) != 0 || snap.Registry == nil || len(snap.Registry.List()) != 0 {
		t.Fatal("a broken object kept providers")
	}
	if snap.ResourceVersion != "9" {
		t.Fatalf("resourceVersion = %q, want 9", snap.ResourceVersion)
	}
}

func TestLiveSwapsSnapshotAndNotifies(t *testing.T) {
	t.Parallel()
	live := NewLive()
	calls := 0
	live.OnChange(func() { calls++ })
	first := live.Load()
	next := Build(&v1alpha1.AssistantConfig{Spec: labSpec()}, factories.DefaultFactories(), t.TempDir())
	live.Store(next)
	if live.Load() != next || live.Load() == first {
		t.Fatal("Load did not return the stored snapshot")
	}
	if calls != 1 {
		t.Fatalf("hook ran %d times, want 1", calls)
	}
	select {
	case <-live.Synced():
		t.Fatal("a live value that was never listed reports synced")
	default:
	}
}

// fakeObject returns the AssistantConfig object with one provider and the named default.
func fakeObject(name, defProv, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "AssistantConfig",
		"metadata":   map[string]any{"name": name, "resourceVersion": rv},
		"spec": map[string]any{
			"providers": []any{map[string]any{
				"name": "lab", "kind": "openai-compatible", "baseURL": "http://192.0.2.1:8082/v1",
				"models": []any{"m1"}, "trust": "internal", "auth": "none",
			}},
			"defaultProvider":     defProv,
			"defaultModel":        "m1",
			"externalDataSharing": "metadata",
			"approvalExpiry":      "1h",
			"budgets":             map[string]any{"session": int64(1000), "userDay": int64(1000), "tenantDay": int64(1000), "steps": int64(4)},
		},
	}}
}

// TestWatchFollowsObject proves that a change of the named object reaches the
// live value, that another name is ignored, and that a delete clears the providers.
func TestWatchFollowsObject(t *testing.T) {
	gvr := v1alpha1.SchemeGroupVersion.WithResource("assistantconfigs")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "AssistantConfigList"},
		fakeObject(v1alpha1.AssistantConfigName, "lab", "1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := NewLive()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, dyn, live, factories.DefaultFactories(), t.TempDir(), nil) }()
	select {
	case <-live.Synced():
	case <-time.After(10 * time.Second):
		t.Fatal("the watch did not sync")
	}
	if got := live.Load().Config.DefaultProvider; got != "lab" {
		t.Fatalf("default after sync = %q, want lab", got)
	}

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the snapshot did not change", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	res := dyn.Resource(gvr)
	if _, err := res.Update(ctx, fakeObject(v1alpha1.AssistantConfigName, "lab", "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	waitFor("update", func() bool { return live.Load().ResourceVersion == "2" })

	if _, err := res.Create(ctx, fakeObject("other", "", "5"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := res.Delete(ctx, v1alpha1.AssistantConfigName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor("delete", func() bool { return len(live.Load().Config.Providers) == 0 })
	if rv := live.Load().ResourceVersion; rv != "" {
		t.Fatalf("resourceVersion after delete = %q, want empty", rv)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Watch returned %v", err)
	}
}

// TestEnumsMatchAgentRegistry pins that the CRD enum strings are the agent enum values.
func TestEnumsMatchAgentRegistry(t *testing.T) {
	t.Parallel()
	pairs := map[string]string{
		v1alpha1.AssistantProviderKindAnthropic:        string(agent.ProviderAnthropic),
		v1alpha1.AssistantProviderKindOpenAICompatible: string(agent.ProviderOpenAICompatible),
		v1alpha1.AssistantTrustInternal:                string(agent.TrustInternal),
		v1alpha1.AssistantTrustExternal:                string(agent.TrustExternal),
		v1alpha1.AssistantSharingNone:                  string(agent.DataNone),
		v1alpha1.AssistantSharingMetadata:              string(agent.DataMetadata),
		v1alpha1.AssistantSharingLogs:                  string(agent.DataLogs),
	}
	for crd, reg := range pairs {
		if crd != reg {
			t.Errorf("CRD value %q differs from the agent value %q", crd, reg)
		}
	}
}

// TestDefaultsMatchCRD pins that the Go defaults equal the defaults of the CRD schema.
func TestDefaultsMatchCRD(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "varroa", "crds", "varroa.dev_assistantconfigs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := sigsyaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	props := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	defaultOf := func(path ...string) any {
		node := props
		for i, p := range path {
			field := node[p].(map[string]any)
			if i == len(path)-1 {
				return field["default"]
			}
			node = field["properties"].(map[string]any)
		}
		return nil
	}
	want := map[string]any{
		"approvalExpiry":      v1alpha1.AssistantDefaultApprovalExpiry,
		"externalDataSharing": v1alpha1.AssistantDefaultSharing,
	}
	for k, v := range want {
		if got := defaultOf(k); got != v {
			t.Errorf("CRD default of %s = %v, want %v", k, got, v)
		}
	}
	budgets := map[string]int64{
		"session":   v1alpha1.AssistantDefaultSessionBudget,
		"userDay":   v1alpha1.AssistantDefaultUserDayBudget,
		"tenantDay": v1alpha1.AssistantDefaultTenantDayBudget,
		"steps":     int64(v1alpha1.AssistantDefaultSteps),
	}
	for k, v := range budgets {
		got, ok := defaultOf("budgets", k).(int)
		if !ok {
			// YAML decodes integers as float64 through sigs.k8s.io/yaml.
			f, fok := defaultOf("budgets", k).(float64)
			if !fok || int64(f) != v {
				t.Errorf("CRD default of budgets.%s = %v, want %d", k, defaultOf("budgets", k), v)
			}
			continue
		}
		if int64(got) != v {
			t.Errorf("CRD default of budgets.%s = %d, want %d", k, got, v)
		}
	}
}

// TestDeleteTombstoneGivesNoProviders proves that a delete delivered as a
// tombstone clears the providers, as a plain delete does.
func TestDeleteTombstoneGivesNoProviders(t *testing.T) {
	t.Parallel()
	live := NewLiveWith(Build(&v1alpha1.AssistantConfig{Spec: labSpec()}, factories.DefaultFactories(), t.TempDir()))
	if len(live.Load().Config.Providers) != 1 {
		t.Fatal("the seed snapshot holds no provider")
	}
	deleteHandler(live, factories.DefaultFactories(), t.TempDir())(cache.DeletedFinalStateUnknown{
		Key: v1alpha1.AssistantConfigName,
		Obj: fakeObject(v1alpha1.AssistantConfigName, "lab", "4"),
	})
	if n := len(live.Load().Config.Providers); n != 0 {
		t.Fatalf("providers after a tombstone delete = %d, want 0", n)
	}
}

// TestFallbackRegistryDoesNotPanic proves that a snapshot whose providers do not
// build keeps a usable registry: no panic, the error set, and no providers.
func TestFallbackRegistryDoesNotPanic(t *testing.T) {
	t.Parallel()
	snap := Build(&v1alpha1.AssistantConfig{Spec: labSpec()}, map[agent.ProviderKind]provider.Factory{}, t.TempDir())
	if snap.Registry == nil {
		t.Fatal("registry is nil after a failed build")
	}
	if snap.Err == nil {
		t.Fatal("a provider without a factory built without an error")
	}
	if _, ok := snap.Registry.Get("lab"); ok {
		t.Fatal("the fallback registry holds the broken provider")
	}
}
