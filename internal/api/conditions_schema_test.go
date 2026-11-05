package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// specRouteSchema answers the JSON body schema of one route response from
// the embedded spec.
func specRouteSchema(t *testing.T, method, path string, status int) *openapi3.Schema {
	t.Helper()
	doc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	item := doc.Paths.Find(path)
	if item == nil || item.GetOperation(method) == nil {
		t.Fatalf("spec has no %s %s operation", method, path)
	}
	resp := item.GetOperation(method).Responses.Status(status)
	if resp == nil || resp.Value == nil {
		t.Fatalf("%s %s declares no %d response", method, path, status)
	}
	return resp.Value.Content.Get("application/json").Schema.Value
}

// visitBody decodes raw and checks it against schema.
func visitBody(t *testing.T, schema *openapi3.Schema, raw []byte) {
	t.Helper()
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if err := schema.VisitJSON(body); err != nil {
		t.Fatalf("body does not match the spec: %v\n%s", err, raw)
	}
}

// TestTenantAgentRoutes_ConditionsMatchSpec checks the detail and the list
// bodies of a Tenant agent with a condition against the spec. The condition
// has an empty message: the Condition schema requires the key, and
// metav1.Condition always writes it.
func TestTenantAgentRoutes_ConditionsMatchSpec(t *testing.T) {
	f := newTenantAgentFixture(t)
	f.source.items[0].Status.Conditions = []metav1.Condition{{
		Type: "Paused", Status: metav1.ConditionFalse, Reason: "Running",
		LastTransitionTime: metav1.NewTime(time.Unix(1700000000, 0)),
	}}
	rec := f.do(t, f.reader, http.MethodGet, "/pipelines/team-a/agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body = %s", rec.Code, rec.Body)
	}
	visitBody(t, specRouteSchema(t, http.MethodGet, "/api/v1/pipelines/{tenant}/agent", http.StatusOK), rec.Body.Bytes())
	rec = f.do(t, f.ops, http.MethodGet, "/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", rec.Code, rec.Body)
	}
	visitBody(t, specRouteSchema(t, http.MethodGet, "/api/v1/agents", http.StatusOK), rec.Body.Bytes())
}

// TestUpdateCenterStatusConditionsMatchSpec checks the status body against
// the spec. The second condition has no transition time, so the body carries
// a null lastTransitionTime and empty reason and message keys.
func TestUpdateCenterStatusConditionsMatchSpec(t *testing.T) {
	uc := &v1alpha1.UpdateCenter{Status: v1alpha1.UpdateCenterStatus{
		Conditions: []v1alpha1.UpdateCenterCondition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: metav1.NewTime(time.Unix(1700000000, 0))},
			{Type: "PluginsPrefetched", Status: metav1.ConditionUnknown},
		},
	}}
	raw, err := json.Marshal(updateCenterStatusResponseFromCR(uc))
	if err != nil {
		t.Fatal(err)
	}
	visitBody(t, specRouteSchema(t, http.MethodGet, "/api/v1/updatecenter", http.StatusOK), raw)
}
