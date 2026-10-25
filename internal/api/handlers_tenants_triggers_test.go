package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestUpdateTenant_TenantAdminRefusalNamesTenantOwnedFields asserts that a
// pipelines admin who changes a field outside the tenant-owned set is refused,
// and that the refusal names all four tenant-owned fields.
func TestUpdateTenant_TenantAdminRefusalNamesTenantOwnedFields(t *testing.T) {
	srv, store, ctx := tenantPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1", "ns2"],
		"pipelines": {"profile": "lts", "agentNamespace": "ns1"}
	}`
	req := httptest.NewRequest(http.MethodPut, "/tenants/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTenantDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("PUT status = %d, want 403; body=%s", w.Result().StatusCode, w.Body.String())
	}
	want := "exports, concurrency limits, badge, and triggers"
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("refusal = %s, want it to name %q", w.Body.String(), want)
	}
}

func TestTenantPipelinesTriggersRoundTrip(t *testing.T) {
	spec := &v1alpha1.TenantPipelinesSpec{
		Profile:        "lts",
		AgentNamespace: "ns1",
		Triggers:       &v1alpha1.TenantTriggersSpec{CommentPattern: "^/retest$", ChecksRerun: rerunBoolPtr(false)},
	}
	entry := tenantPipelinesEntryFromSpec(spec)
	if entry.Triggers == nil || entry.Triggers.CommentPattern != "^/retest$" {
		t.Fatalf("entry triggers = %+v", entry.Triggers)
	}
	back := tenantPipelinesSpecFromEntry(entry)
	if !reflect.DeepEqual(back.Triggers, spec.Triggers) {
		t.Fatalf("round trip triggers = %+v, want %+v", back.Triggers, spec.Triggers)
	}
}

func TestValidateTenantPipelines_CommentPatternInvalid(t *testing.T) {
	entry := &tenantPipelinesEntry{
		Profile:        "lts",
		AgentNamespace: "ns1",
		Triggers:       &v1alpha1.TenantTriggersSpec{CommentPattern: "(unclosed"},
	}
	msg := validateTenantPipelines(entry, []string{"ns1"})
	if msg != "pipelines.triggers.commentPattern is invalid" {
		t.Fatalf("message = %q", msg)
	}
}

func TestValidateTenantPipelines_CommentPatternTooLong(t *testing.T) {
	entry := &tenantPipelinesEntry{
		Profile:        "lts",
		AgentNamespace: "ns1",
		Triggers:       &v1alpha1.TenantTriggersSpec{CommentPattern: strings.Repeat("a", 257)},
	}
	msg := validateTenantPipelines(entry, []string{"ns1"})
	if msg != "pipelines.triggers.commentPattern is too long" {
		t.Fatalf("message = %q", msg)
	}
}

func TestTenantAdminMayChangeTriggers(t *testing.T) {
	stored := v1alpha1.TenantSpec{Namespaces: []string{"ns1"}, Pipelines: &v1alpha1.TenantPipelinesSpec{
		Profile: "lts", AgentNamespace: "ns1",
	}}
	edited := v1alpha1.TenantSpec{Namespaces: []string{"ns1"}, Pipelines: &v1alpha1.TenantPipelinesSpec{
		Profile: "lts", AgentNamespace: "ns1",
		Triggers: &v1alpha1.TenantTriggersSpec{CommentPattern: "^/retest$"},
	}}
	if !tenantSpecEqualExceptTenantOwned(stored, edited) {
		t.Fatal("a triggers-only change must pass the tenant admin check")
	}
	edited.Namespaces = []string{"ns1", "ns2"}
	if tenantSpecEqualExceptTenantOwned(stored, edited) {
		t.Fatal("a namespaces change must still fail the tenant admin check")
	}
}

func TestTenantTriggersPreservedWhenUnset(t *testing.T) {
	spec := &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"}
	entry := tenantPipelinesEntryFromSpec(spec)
	if entry.Triggers != nil {
		t.Fatalf("unset triggers mapped to %+v", entry.Triggers)
	}
}
