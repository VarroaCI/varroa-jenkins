package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// specTestDeps builds admin Dependencies around a store seeded with one
// controller. The controller-mutation tools now route through the bus, so the
// store is only used for argument-rejection tests that never reach Brood.
func specTestDeps() *api.Dependencies {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"*"}, Verbs: []string{"*"}},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
			RoleRef:  "admin",
		},
	}}

	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "demo",
			Namespace:     "ns",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "varroa-ui"}},
		},
		Spec: v1alpha1.ControllerSpec{
			Version: "2.516.3",
			Hibernation: &v1alpha1.HibernationSpec{
				Enabled:            false,
				GracePeriodMinutes: 30,
			},
		},
	})

	return &api.Dependencies{
		Client:     &stubClient{},
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Store:      store,
	}
}

// update_controller must build a patch whose hibernation carries all three
// fields — nothing flattened. The MCP local fallback is gone, so the
// assertion is on the patch handed to Brood.Update rather than on a local
// store write.
func TestUpdateControllerSetsHibernation(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_controller",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "ci",
			"hibernation": map[string]interface{}{
				"enabled":             true,
				"gracePeriodMinutes":  60,
				"activityIgnoreRegex": "^/junk",
			},
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_controller: %q", toolText(tr))
	}

	var patch struct {
		Spec map[string]interface{} `json:"spec"`
	}
	if err := json.Unmarshal(brood.updatePatch, &patch); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	h, ok := patch.Spec["hibernation"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch spec has no hibernation: %s", brood.updatePatch)
	}
	if h["enabled"] != true || h["gracePeriodMinutes"] != float64(60) || h["activityIgnoreRegex"] != "^/junk" {
		t.Fatalf("hibernation patch = %#v, want all three fields", h)
	}
}

func TestUpdateControllerSpecPassthrough(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_controller",
		"arguments": map[string]interface{}{
			"namespace":  "team-a",
			"name":       "ci",
			"powerState": "Stopped",
			"spec": map[string]interface{}{
				"className": "team-a",
				"probes": map[string]interface{}{
					"startup": map[string]interface{}{"periodSeconds": 20},
				},
			},
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_controller: %q", toolText(tr))
	}

	var patch struct {
		Spec map[string]interface{} `json:"spec"`
	}
	if err := json.Unmarshal(brood.updatePatch, &patch); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patch.Spec["powerState"] != "Stopped" {
		t.Errorf("powerState = %v, want Stopped", patch.Spec["powerState"])
	}
	if patch.Spec["className"] != "team-a" {
		t.Errorf("className = %v, want team-a", patch.Spec["className"])
	}
	probes, ok := patch.Spec["probes"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch spec has no probes: %s", brood.updatePatch)
	}
	startup, _ := probes["startup"].(map[string]interface{})
	if startup == nil || startup["periodSeconds"] != float64(20) {
		t.Errorf("probes.startup.periodSeconds not applied: %#v", probes)
	}
}

// create_controller cannot be driven end-to-end here — its bus route runs the
// operator's preflight, which a stub Brood does not simulate — so the two
// halves of its wiring are pinned separately: the declared schema, and the
// merge that turns the arguments into spec.
func TestControllerToolsDeclareSpecArguments(t *testing.T) {
	handler := NewHandler(newTestDeps())
	resp := mcpRequest(t, handler, "tools/list", nil, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %s", resp.Error)
	}

	var wrapper struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &wrapper); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}

	for _, tool := range wrapper.Tools {
		if tool.Name != "create_controller" && tool.Name != "update_controller" {
			continue
		}
		for _, arg := range []string{"version", "composedBundleRef", "powerState", "hibernation", "spec"} {
			if _, ok := tool.InputSchema.Properties[arg]; !ok {
				t.Errorf("%s does not declare a %q argument", tool.Name, arg)
			}
		}
	}
}

func TestMergeControllerSpecOnFreshController(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "ns"},
	}
	spec, err := controllerSpecPatch(map[string]any{
		"version": "2.570",
		"hibernation": map[string]any{
			"enabled":            true,
			"gracePeriodMinutes": 45,
		},
	})
	if err != nil {
		t.Fatalf("controllerSpecPatch: %v", err)
	}
	merged, err := mergeControllerSpec(cr, spec)
	if err != nil {
		t.Fatalf("mergeControllerSpec: %v", err)
	}
	if merged.Name != "fresh" || merged.Namespace != "ns" {
		t.Errorf("metadata lost in the round trip: %+v", merged.ObjectMeta)
	}
	if merged.Spec.Version != "2.570" {
		t.Errorf("spec.version = %q, want 2.570", merged.Spec.Version)
	}
	if merged.Spec.Hibernation == nil || !merged.Spec.Hibernation.Enabled ||
		merged.Spec.Hibernation.GracePeriodMinutes != 45 {
		t.Errorf("spec.hibernation not applied: %+v", merged.Spec.Hibernation)
	}
}

// An unrecognized argument must be named and rejected rather than silently
// dropped — including the harder case where one arrives alongside a valid
// field, which would otherwise succeed while silently discarding half of
// what the caller sent.
func TestUpdateControllerRejectsUnrecognizedArguments(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{
			name: "only unrecognized arguments",
			args: map[string]interface{}{
				"namespace": "ns", "name": "demo",
				"resources": map[string]interface{}{"limits": map[string]interface{}{"cpu": "2"}},
				"endpoint":  "https://example.test",
			},
		},
		{
			name: "unrecognized alongside a valid one",
			args: map[string]interface{}{
				"namespace": "ns", "name": "demo",
				"version":   "2.570",
				"resources": map[string]interface{}{"limits": map[string]interface{}{"cpu": "2"}},
				"endpoint":  "https://example.test",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := specTestDeps()
			handler := NewHandler(deps)
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      "update_controller",
				"arguments": tc.args,
			}, mcpAdminClaims)

			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatal("expected an error result for an unrecognized argument")
			}
			msg := toolText(tr)
			for _, want := range []string{"endpoint", "resources", "spec"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error message does not mention %q: %s", want, msg)
				}
			}
		})
	}
}

// The passthrough is a literal merge patch, so an explicit empty value clears
// the field — the shorthands deliberately do not, and the docs say so.
func TestControllerSpecPatchPassthroughCanClear(t *testing.T) {
	spec, err := controllerSpecPatch(map[string]any{
		"version": "",
		"spec":    map[string]any{"version": "", "className": nil},
	})
	if err != nil {
		t.Fatalf("controllerSpecPatch: %v", err)
	}
	v, ok := spec["version"]
	if !ok || v != "" {
		t.Errorf("spec.version = %v (present=%v), want an explicit empty string from the passthrough", v, ok)
	}
	if c, ok := spec["className"]; !ok || c != nil {
		t.Errorf("spec.className = %v (present=%v), want an explicit null", c, ok)
	}
}

func TestControllerSpecPatchOnlyIncludesSuppliedFields(t *testing.T) {
	// Omitted arguments must not appear at all: a merge patch carrying
	// "version": "" blanks a live controller's pinned version.
	spec, err := controllerSpecPatch(map[string]any{
		"namespace": "ns",
		"name":      "demo",
		"version":   "",
	})
	if err != nil {
		t.Fatalf("controllerSpecPatch: %v", err)
	}
	if len(spec) != 0 {
		t.Fatalf("empty arguments produced a patch: %#v", spec)
	}

	spec, err = controllerSpecPatch(map[string]any{
		"version":           "2.516.3",
		"composedBundleRef": "team-bundle",
		"powerState":        "Running",
		"hibernation":       map[string]any{"enabled": true},
	})
	if err != nil {
		t.Fatalf("controllerSpecPatch: %v", err)
	}
	got, _ := json.Marshal(spec)
	want := `{"composedBundleRef":{"name":"team-bundle"},"hibernation":{"enabled":true},"powerState":"Running","version":"2.516.3"}`
	if string(got) != want {
		t.Errorf("patch = %s\nwant %s", got, want)
	}
}

func TestControllerSpecPatchShorthandBeatsSpec(t *testing.T) {
	spec, err := controllerSpecPatch(map[string]any{
		"version": "2.516.3",
		"spec": map[string]any{
			"version":   "1.0.0",
			"className": "team-a",
		},
	})
	if err != nil {
		t.Fatalf("controllerSpecPatch: %v", err)
	}
	if spec["version"] != "2.516.3" {
		t.Errorf("version = %v, want the shorthand to win with 2.516.3", spec["version"])
	}
	if spec["className"] != "team-a" {
		t.Errorf("className = %v, want it carried through from spec", spec["className"])
	}
}

// Hibernated is not a spec.powerState value anymore: it is reported in status
// and driven by the hibernate/wake action tools. The shorthand must refuse it
// (and any other value the CRD enum would refuse) rather than copy it through.
func TestControllerSpecPatchRejectsHibernated(t *testing.T) {
	for _, ps := range []string{"Hibernated", "hibernated", "Paused"} {
		t.Run(ps, func(t *testing.T) {
			spec, err := controllerSpecPatch(map[string]any{
				"powerState": ps,
			})
			if err == nil {
				t.Fatalf("controllerSpecPatch accepted powerState %q: %#v", ps, spec)
			}
			if ps == "Hibernated" && !strings.Contains(err.Error(), "hibernate_controller") {
				t.Errorf("Hibernated rejection must point at hibernate_controller, got: %v", err)
			}
		})
	}
}

func TestMCPUpdateControllerRejectsHibernatedPowerState(t *testing.T) {
	brood := &recordingBrood{}
	handler := NewHandler(broodHandler(brood).deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_controller",
		"arguments": map[string]interface{}{
			"namespace": "team-a", "name": "ci", "powerState": "Hibernated",
		},
	}, guardClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("update_controller accepted powerState Hibernated, want rejection")
	}
	if text := toolText(tr); !strings.Contains(text, "hibernate_controller") {
		t.Errorf("rejection does not point at hibernate_controller:\n%s", text)
	}
	if brood.updateCalled {
		t.Error("Brood.Update must not be called for a rejected powerState")
	}
}
