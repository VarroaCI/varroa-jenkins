package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// storedPaymentsVersion returns the resourceVersion of the stored payments Tenant.
func storedPaymentsVersion(t *testing.T, store *crdstore.Fake) string {
	t.Helper()
	tenant, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get tenant payments: %v", err)
	}
	return tenant.ResourceVersion
}

// tenantBodyAt sets metadata.resourceVersion of a PUT body to rv.
func tenantBodyAt(t *testing.T, body, rv string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	m["metadata"] = map[string]any{"resourceVersion": rv}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

func putTenant(ctx context.Context, t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/tenants/payments", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTenantDispatch(w, req)
	return w
}

// getTenantVersion reads the tenant through GET and returns its metadata.resourceVersion.
func getTenantVersion(ctx context.Context, t *testing.T, srv *Server, name string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tenants/"+name, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTenantDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}
	var got struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	return got.Metadata.ResourceVersion
}

func seedPaymentsTenant(t *testing.T, store *crdstore.Fake, members ...string) {
	t.Helper()
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Members: members, Namespaces: []string{"ns1"}, DisplayName: "Payments"},
	})
}

func mutatePaymentsMembers(t *testing.T, store *crdstore.Fake, members ...string) {
	t.Helper()
	_, err := crdstore.Mutate[v1alpha1.Tenant](context.Background(), store, "payments", "", func(obj *v1alpha1.Tenant) (bool, error) {
		obj.Spec.Members = members
		return true, nil
	})
	if err != nil {
		t.Fatalf("mutate tenant: %v", err)
	}
}

func TestTenantHandlers_GetCarriesResourceVersion(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)
	seedPaymentsTenant(t, store, "alice")
	want := storedPaymentsVersion(t, store)
	if got := getTenantVersion(ctx, t, srv, "payments"); got != want {
		t.Fatalf("GET metadata.resourceVersion = %q, want %q", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/tenants", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTenants(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"resourceVersion":"`+want+`"`) {
		t.Fatalf("list body = %s, want resourceVersion %q", w.Body.String(), want)
	}
}

func TestUpdateTenant_KeepsMetadataAndFinalizer(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "payments",
			Labels:          map[string]string{"cost-center": "pay"},
			Annotations:     map[string]string{"owner": "ops"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Controller", Name: "owner", UID: "uid-1"}},
			Finalizers:      []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})

	body := tenantBodyAt(t, `{"name":"payments","displayName":"Payments","members":["alice"],"namespaces":["ns1"]}`,
		storedPaymentsVersion(t, store))
	if w := putTenant(ctx, t, srv, body); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored tenant: %v", err)
	}
	if stored.Spec.DisplayName != "Payments" {
		t.Fatalf("displayName = %q, want Payments", stored.Spec.DisplayName)
	}
	if stored.Labels["cost-center"] != "pay" || stored.Annotations["owner"] != "ops" {
		t.Fatalf("labels/annotations = %v/%v, want both kept", stored.Labels, stored.Annotations)
	}
	if len(stored.OwnerReferences) != 1 || stored.OwnerReferences[0].Name != "owner" {
		t.Fatalf("ownerReferences = %+v, want the owner kept", stored.OwnerReferences)
	}
	if len(stored.Finalizers) != 1 || stored.Finalizers[0] != v1alpha1.TenantFinalizerTenantPlacement {
		t.Fatalf("finalizers = %v, want the placement finalizer kept", stored.Finalizers)
	}
}

func TestUpdateTenant_StaleVersionAnswers409(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)
	seedPaymentsTenant(t, store, "alice", "bob")
	read := storedPaymentsVersion(t, store)
	mutatePaymentsMembers(t, store, "alice")

	body := tenantBodyAt(t, `{"name":"payments","members":["alice","bob"],"namespaces":["ns1"]}`, read)
	w := putTenant(ctx, t, srv, body)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "changed or was deleted after it was read") {
		t.Fatalf("409 body = %s, want the stale-version message", w.Body.String())
	}
	stored, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored tenant: %v", err)
	}
	if len(stored.Spec.Members) != 1 || stored.Spec.Members[0] != "alice" {
		t.Fatalf("stored members = %v, want [alice]", stored.Spec.Members)
	}
}

func TestUpdateTenant_WithoutVersionOnExistingAnswers409(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)
	seedPaymentsTenant(t, store, "alice")

	body := `{"name":"payments","displayName":"Changed","members":["alice"],"namespaces":["ns1"]}`
	w := putTenant(ctx, t, srv, body)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exists: read it") {
		t.Fatalf("409 body = %s, want the exists message", w.Body.String())
	}
	stored, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored tenant: %v", err)
	}
	if stored.Spec.DisplayName != "Payments" {
		t.Fatalf("displayName = %q, want it unchanged", stored.Spec.DisplayName)
	}
}

func TestUpdateTenant_WithoutVersionCreates(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)

	body := `{"name":"payments","members":["alice"],"namespaces":["ns1"]}`
	if w := putTenant(ctx, t, srv, body); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", ""); err != nil {
		t.Fatalf("tenant not created: %v", err)
	}
}

func TestUpdateTenant_DeletedAfterReadAnswers409(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)

	body := tenantBodyAt(t, `{"name":"payments","members":["alice"],"namespaces":["ns1"]}`, "5")
	w := putTenant(ctx, t, srv, body)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
	_, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get after 409 = %v, want NotFound (nothing written)", err)
	}
}

func TestUpdateTenant_TenantAdminStaleVersionAnswers409BeforeFieldCheck(t *testing.T) {
	srv, store, ctx := tenantPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})
	read := storedPaymentsVersion(t, store)
	mutatePaymentsMembers(t, store, "alice", "bob")

	body := tenantBodyAt(t, `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"roleRef": "operator",
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [{"job": "release/*", "callers": ["search"]}]
		}
	}`, read)
	w := putTenant(ctx, t, srv, body)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409 (not 403); body=%s", w.Result().StatusCode, w.Body.String())
	}
}

func TestUpdateTenant_TenantAdminWithoutVersionAnswers409(t *testing.T) {
	srv, store, ctx := tenantPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})

	body := `{"name":"payments","members":["alice"],"namespaces":["ns1"],"pipelines":{"profile":"lts","agentNamespace":"ns1"}}`
	if w := putTenant(ctx, t, srv, body); w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
}

func TestUpdateTenant_TenantAdminCannotCreate(t *testing.T) {
	srv, store, ctx := tenantPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")

	body := `{"name":"payments","members":["alice"],"namespaces":["ns1"],"pipelines":{"profile":"lts","agentNamespace":"ns1"}}`
	if w := putTenant(ctx, t, srv, body); w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("PUT status = %d, want 404; body=%s", w.Result().StatusCode, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", ""); !apierrors.IsNotFound(err) {
		t.Fatalf("get after 404 = %v, want NotFound (nothing created)", err)
	}
}

// TestUpdateTenant_WriteAfterFieldCheckAnswers409 covers the window between
// the handler's field check and its write: another writer removes a member
// after the handler read the stored Tenant.
func TestUpdateTenant_WriteAfterFieldCheckAnswers409(t *testing.T) {
	srv, store, ctx := tenantPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Members:    []string{"alice", "bob"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})
	read := storedPaymentsVersion(t, store)

	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("gvr: %v", err)
	}
	fired := false
	store.SetAfterGet(gvr, func(_, _ string) {
		if fired {
			return
		}
		fired = true
		store.SetAfterGet(gvr, nil)
		mutatePaymentsMembers(t, store, "alice")
	})

	body := tenantBodyAt(t, `{
		"name": "payments",
		"members": ["alice", "bob"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [{"job": "release/*", "callers": ["search"]}]
		}
	}`, read)
	w := putTenant(ctx, t, srv, body)
	if !fired {
		t.Fatal("after-get hook never fired")
	}
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
	stored, err := crdstore.Get[v1alpha1.Tenant](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored tenant: %v", err)
	}
	if len(stored.Spec.Members) != 1 || stored.Spec.Members[0] != "alice" {
		t.Fatalf("stored members = %v, want [alice]", stored.Spec.Members)
	}
	if stored.Spec.Pipelines == nil || len(stored.Spec.Pipelines.Exports) != 0 {
		t.Fatalf("stored exports = %+v, want none written", stored.Spec.Pipelines)
	}
}

func TestUpdateTenant_ReadFailureAnswers500(t *testing.T) {
	srv, store, ctx := adminTenantTestServer(t)
	seedPaymentsTenant(t, store, "alice")
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("gvr: %v", err)
	}
	read := storedPaymentsVersion(t, store)
	store.FailNext("get", gvr, errors.New("apiserver busy"))

	body := tenantBodyAt(t, `{"name":"payments","members":["alice"],"namespaces":["ns1"],"displayName":"Changed"}`, read)
	if w := putTenant(ctx, t, srv, body); w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT status = %d, want 500; body=%s", w.Result().StatusCode, w.Body.String())
	}
}
