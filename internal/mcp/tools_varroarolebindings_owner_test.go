package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ownedVarroaRoleBindingDeps seeds two operator-made bindings and a renamed copy
// of the TenantAgent binding, which keeps the same labels.
func ownedVarroaRoleBindingDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1alpha1.TenantAgentBindingName("ns-a", "agent-a"),
				Labels: map[string]string{
					v1alpha1.LabelTenantAgent:          "true",
					v1alpha1.LabelTenantAgentNamespace: "ns-a",
					v1alpha1.LabelTenantAgentName:      "agent-a",
				},
			},
			Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "developer", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "agent:ns-a/tenant-a/agent-a"}}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1alpha1.BroodScheduleBindingName("ns-a", "nightly"),
				Annotations: map[string]string{
					v1alpha1.AnnotationBroodScheduleNamespace: "ns-a",
					v1alpha1.AnnotationBroodScheduleName:      "nightly",
				},
			},
			Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "operator", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "schedule:ns-a/nightly"}}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "copy-of-agent",
				Labels: map[string]string{
					v1alpha1.LabelTenantAgent:          "true",
					v1alpha1.LabelTenantAgentNamespace: "ns-a",
					v1alpha1.LabelTenantAgentName:      "agent-a",
				},
			},
			Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "developer"},
		},
	)
	return &api.Dependencies{Client: &stubClient{}, Store: store, Authorizer: adminTestAuthorizer()}
}

func TestSummarizeVarroaRoleBinding_Owner(t *testing.T) {
	ta := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   v1alpha1.TenantAgentBindingName("ns-a", "agent-a"),
			Labels: map[string]string{v1alpha1.LabelTenantAgent: "true", v1alpha1.LabelTenantAgentNamespace: "ns-a", v1alpha1.LabelTenantAgentName: "agent-a"},
		},
	})
	if ta.Owner == nil || *ta.Owner != (varroaRoleBindingOwner{Kind: "TenantAgent", Namespace: "ns-a", Name: "agent-a"}) {
		t.Errorf("TenantAgent owner = %+v", ta.Owner)
	}
	bs := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        v1alpha1.BroodScheduleBindingName("ns-a", "nightly"),
			Annotations: map[string]string{v1alpha1.AnnotationBroodScheduleNamespace: "ns-a", v1alpha1.AnnotationBroodScheduleName: "nightly"},
		},
	})
	if bs.Owner == nil || *bs.Owner != (varroaRoleBindingOwner{Kind: "BroodSchedule", Namespace: "ns-a", Name: "nightly"}) {
		t.Errorf("BroodSchedule owner = %+v", bs.Owner)
	}
	renamed := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "copy-of-agent",
			Labels: map[string]string{v1alpha1.LabelTenantAgent: "true", v1alpha1.LabelTenantAgentNamespace: "ns-a", v1alpha1.LabelTenantAgentName: "agent-a"},
		},
	})
	if renamed.Owner != nil {
		t.Errorf("renamed copy owner = %+v, want nil", renamed.Owner)
	}
	plain := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "vrb-plain"}})
	if plain.Owner != nil {
		t.Errorf("plain binding owner = %+v, want nil", plain.Owner)
	}
}

func TestListVarroaRoleBindings_ShowsOwner(t *testing.T) {
	handler := NewHandler(ownedVarroaRoleBindingDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_varroa_role_bindings",
		"arguments": map[string]interface{}{},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_varroa_role_bindings returned error: %v", tr.Content)
	}
	b, _ := json.Marshal(resp.Result)
	var payload struct {
		StructuredContent struct {
			Items []map[string]any `json:"items"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	owners := map[string]any{}
	for _, item := range payload.StructuredContent.Items {
		owners[item["name"].(string)] = item["owner"]
	}
	if len(owners) != 3 {
		t.Fatalf("items = %v, want 3", owners)
	}
	wantTA := map[string]any{"kind": "TenantAgent", "namespace": "ns-a", "name": "agent-a"}
	if got := owners[v1alpha1.TenantAgentBindingName("ns-a", "agent-a")]; !reflect.DeepEqual(got, wantTA) {
		t.Errorf("TenantAgent owner = %v, want %v", got, wantTA)
	}
	wantBS := map[string]any{"kind": "BroodSchedule", "namespace": "ns-a", "name": "nightly"}
	if got := owners[v1alpha1.BroodScheduleBindingName("ns-a", "nightly")]; !reflect.DeepEqual(got, wantBS) {
		t.Errorf("BroodSchedule owner = %v, want %v", got, wantBS)
	}
	if got := owners["copy-of-agent"]; got != nil {
		t.Errorf("renamed copy owner = %v, want none", got)
	}

	// The rows with owner must satisfy the declared output schema.
	var schemaDoc any
	if err := json.Unmarshal(varroaRoleBindingListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_varroa_role_bindings.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_varroa_role_bindings.json")
	if err != nil {
		t.Fatalf("declared schema does not compile: %v", err)
	}
	var instance any
	if err := json.Unmarshal(b, &instance); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	structured := instance.(map[string]any)["structuredContent"]
	if err := schema.Validate(structured); err != nil {
		t.Errorf("owned-binding list violates declared outputSchema: %v\npayload: %s", err, b)
	}
}

func TestGetVarroaRoleBinding_ReturnsAPIObject(t *testing.T) {
	handler := NewHandler(ownedVarroaRoleBindingDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_varroa_role_binding",
		"arguments": map[string]interface{}{"name": v1alpha1.TenantAgentBindingName("ns-a", "agent-a")},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("get_varroa_role_binding returned error: %v", tr.Content)
	}
	b, _ := json.Marshal(resp.Result)
	if strings.Contains(string(b), `"owner":`) {
		t.Errorf("get_varroa_role_binding must return the API object with no owner key: %s", b)
	}
	if !strings.Contains(string(b), `"varroa.dev/tenant-agent-name"`) {
		t.Errorf("get_varroa_role_binding must keep the labels: %s", b)
	}
}
