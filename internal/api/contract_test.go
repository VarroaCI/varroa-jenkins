package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// route represents one method×path template in the router manifest.
type route struct {
	Method string
	Path   string
}

// routeManifest is a hand-written mirror of router.go registrations plus the
// /healthz and /version stubs. Path templates use OpenAPI syntax.
// Update this when adding routes to router.go — the coverage gate catches drift.
var routeManifest = []route{
	// From router.go (paths handled via NewRouter, stripped of /api/v1 prefix)
	{Method: "GET", Path: "/api/v1/me"},
	{Method: "PUT", Path: "/api/v1/me/preferences"},
	{Method: "PUT", Path: "/api/v1/me/password"},
	{Method: "POST", Path: "/api/v1/logout"},
	// Auth OIDC endpoints (registered on top-level mux, not in router.go).
	{Method: "GET", Path: "/api/v1/auth/login"},
	{Method: "GET", Path: "/api/v1/callback"},
	{Method: "GET", Path: "/api/v1/me/apikeys"},
	{Method: "POST", Path: "/api/v1/me/apikeys"},
	{Method: "DELETE", Path: "/api/v1/me/apikeys/{prefix}"},
	{Method: "POST", Path: "/api/v1/me/apikeys/{prefix}/rotate"},
	{Method: "GET", Path: "/api/v1/auth-config"},
	{Method: "POST", Path: "/api/v1/login"},
	{Method: "GET", Path: "/api/v1/users"},
	{Method: "POST", Path: "/api/v1/users"},
	{Method: "GET", Path: "/api/v1/identity-settings"},
	{Method: "GET", Path: "/api/v1/builtin-roles"},
	{Method: "GET", Path: "/api/v1/groups"},
	{Method: "POST", Path: "/api/v1/groups"},
	{Method: "GET", Path: "/api/v1/teams"},
	{Method: "POST", Path: "/api/v1/teams"},
	{Method: "GET", Path: "/api/v1/controllers"},
	{Method: "GET", Path: "/api/v1/clusters"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}"},
	{Method: "PATCH", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/preflight"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/render"},
	{Method: "GET", Path: "/api/v1/roles"},
	{Method: "POST", Path: "/api/v1/roles"},
	{Method: "GET", Path: "/api/v1/rolebindings"},
	{Method: "POST", Path: "/api/v1/rolebindings"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/jenkinsroles"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/jenkinsroles"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/jenkinsrolebindings"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/jenkinsrolebindings"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/catalogsources"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/catalogitems"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/composedbundles"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/version-profiles"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/version-profiles"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controller-classes"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/provisioning/config"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/namespaces/deployable"},
	{Method: "GET", Path: "/api/v1/activity"},
	{Method: "GET", Path: "/api/v1/search"},
	{Method: "GET", Path: "/api/v1/me/permissions"},
	// Users sub-routes (dispatch-based)
	{Method: "PUT", Path: "/api/v1/users/{name}"},
	{Method: "DELETE", Path: "/api/v1/users/{name}"},
	{Method: "PUT", Path: "/api/v1/users/{name}/password"},
	{Method: "GET", Path: "/api/v1/users/{name}/apikeys"},
	{Method: "DELETE", Path: "/api/v1/users/{name}/apikeys/{prefix}"},
	// Groups sub-routes
	{Method: "DELETE", Path: "/api/v1/groups/{name}"},
	// Teams sub-routes
	{Method: "GET", Path: "/api/v1/teams/{name}"},
	{Method: "PUT", Path: "/api/v1/teams/{name}"},
	{Method: "DELETE", Path: "/api/v1/teams/{name}"},
	// Controller sub-routes (cluster-scoped)
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/reconcile"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/hibernate"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/wake"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/approve"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/approve-deletion"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/reprovision"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/restart"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/preview"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/yaml"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/logs"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/diff"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/plugins"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/events"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controllers/{ns}/{name}/mite/stream"},
	// RBAC sub-routes
	{Method: "GET", Path: "/api/v1/roles/{name}"},
	{Method: "PUT", Path: "/api/v1/roles/{name}"},
	{Method: "DELETE", Path: "/api/v1/roles/{name}"},
	{Method: "GET", Path: "/api/v1/rolebindings/{name}"},
	{Method: "PUT", Path: "/api/v1/rolebindings/{name}"},
	{Method: "DELETE", Path: "/api/v1/rolebindings/{name}"},
	// JenkinsRole/Binding sub-routes (cluster-scoped)
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/jenkinsroles/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/jenkinsroles/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/jenkinsroles/{name}"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/jenkinsrolebindings/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/jenkinsrolebindings/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/jenkinsrolebindings/{name}"},
	// Catalog sub-routes (cluster-scoped)
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/catalogsources/{ns}"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/catalogsources/{ns}/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/catalogsources/{ns}/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/catalogsources/{ns}/{name}"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/catalogsources/{ns}/{name}/sync"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/catalogitems/{ns}/{name}"},
	// Composed bundle sub-routes (cluster-scoped)
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/composedbundles/validate"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/{name}"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/{name}/pause"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/{name}/resume"},
	{Method: "POST", Path: "/api/v1/clusters/{cluster}/composedbundles/{ns}/preview"},
	// Provisioning
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/provisioningdefaults/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/provisioningdefaults/{name}"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/version-profiles/{name}"},
	{Method: "PUT", Path: "/api/v1/clusters/{cluster}/version-profiles/{name}"},
	{Method: "DELETE", Path: "/api/v1/clusters/{cluster}/version-profiles/{name}"},
	{Method: "GET", Path: "/api/v1/clusters/{cluster}/controller-classes/{name}"},
	// Activity
	{Method: "GET", Path: "/api/v1/activity/stream"},
	// Streams
	{Method: "GET", Path: "/api/v1/stream/brood"},
	{Method: "POST", Path: "/api/v1/stream/ticket"},
	// Brood operations
	{Method: "POST", Path: "/api/v1/brood-operations"},
	{Method: "GET", Path: "/api/v1/brood-operations"},
	{Method: "POST", Path: "/api/v1/brood-operations/preview"},
	{Method: "GET", Path: "/api/v1/brood-operations/{ns}/{name}"},
	{Method: "DELETE", Path: "/api/v1/brood-operations/{ns}/{name}"},
	{Method: "POST", Path: "/api/v1/brood-operations/{ns}/{name}/suspend"},
	{Method: "GET", Path: "/api/v1/brood-operations/{ns}/{name}/stream"},
	// Brood schedules
	{Method: "POST", Path: "/api/v1/brood-schedules"},
	{Method: "GET", Path: "/api/v1/brood-schedules"},
	{Method: "GET", Path: "/api/v1/brood-schedules/{ns}/{name}"},
	{Method: "DELETE", Path: "/api/v1/brood-schedules/{ns}/{name}"},
	{Method: "POST", Path: "/api/v1/brood-schedules/{ns}/{name}/suspend"},
	// OpenAPI spec and docs
	{Method: "GET", Path: "/api/v1/openapi.json"},
	{Method: "GET", Path: "/api/v1/docs"},
	// Stubs outside /api/v1
	{Method: "GET", Path: "/healthz"},
	{Method: "GET", Path: "/version"},
	// MCP
	{Method: "GET", Path: "/api/v1/mcp"},
	{Method: "POST", Path: "/api/v1/mcp"},
	{Method: "DELETE", Path: "/api/v1/mcp"},
	// Update Center
	{Method: "GET", Path: "/api/v1/updatecenter"},
	{Method: "GET", Path: "/api/v1/updatecenter/plugins"},
	{Method: "POST", Path: "/api/v1/updatecenter/plugins"},
	// Fleet plugin inventory
	{Method: "GET", Path: "/api/v1/fleet/plugins"},
	{Method: "GET", Path: "/api/v1/fleet/plugins/{name}"},
}

// contractCase represents one golden exchange to validate against the spec.
type contractCase struct {
	Name   string
	Method string
	Path   string
	Body   any
	// RawBody and ContentType carry a non-JSON request body (e.g. a
	// multipart/form-data upload). When RawBody is set, Body is ignored.
	RawBody            []byte
	ContentType        string
	Claims             *auth.Claims
	WantStatus         int
	SkipSpecValidation bool
}

var contractCases []contractCase

// registerContractCases appends golden cases. Designed as an extensibility
// seam (coordinator seam 1): C2/C3 call this from other _test.go files.
func registerContractCases(cs ...contractCase) {
	contractCases = append(contractCases, cs...)
}

// newContractServer builds a test server that mounts the production router
// wrapped in the auth middleware, plus /healthz and /version stubs.
// Returns the server + the loaded spec.
func newContractServer(t *testing.T) (*httptest.Server, *openapi3.T) {
	t.Helper()

	client := newFakeResourceClient()
	client.namespaces["test-ns"] = true

	// Seed a test controller for detail/update/delete ops.
	client.controllers = map[string]*v1alpha1.Controller{
		"test-ctrl": {
			ObjectMeta: metav1.ObjectMeta{Name: "test-ctrl", Namespace: "test-ns"},
			Spec:       v1alpha1.ControllerSpec{Endpoint: "https://test.example.com"},
			Status:     v1alpha1.ControllerStatus{Phase: "Connected"},
		},
	}
	// Initialize CRD maps for RBAC, catalog, composed bundles
	client.roles = map[string]*v1alpha1.VarroaRole{}
	client.roleBindings = map[string]*v1alpha1.VarroaRoleBinding{}
	client.jenkinsRoles = map[string]*v1alpha1.JenkinsRole{}
	client.jenkinsRoleBindings = map[string]*v1alpha1.JenkinsRoleBinding{}
	client.catalogSources = map[string]*v1alpha1.CatalogSource{}
	client.catalogItems = map[string]*v1alpha1.CatalogItem{}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{}
	// Seed one of each for detail/get operations
	client.roles["admin"] = &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		},
	}
	crdstore.MustSeed(client.crdStore, client.roles["admin"])
	client.roleBindings["admin-binding"] = &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}},
			RoleRef:  "admin",
		},
	}
	crdstore.MustSeed(client.crdStore, client.roleBindings["admin-binding"])
	client.jenkinsRoles["global-admin"] = &v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{Name: "global-admin"},
		Spec:       v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall.Administer"}},
	}
	crdstore.MustSeed(client.crdStore, client.jenkinsRoles["global-admin"])
	client.jenkinsRoleBindings["global-admin-binding"] = &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "global-admin-binding"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin"}},
			RoleRef:  "global-admin",
		},
	}
	crdstore.MustSeed(client.crdStore, client.jenkinsRoleBindings["global-admin-binding"])
	client.catalogSources["test-src"] = &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "test-src", Namespace: "test-ns"},
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL: "https://example.com/repo",
			Path:    "catalog",
		},
	}
	crdstore.MustSeed(client.crdStore, client.catalogSources["test-src"])
	client.catalogItems["test-item"] = &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "test-item", Namespace: "test-ns"},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef: "test-src",
			Type:      "plugin",
			Path:      "plugins/my-plugin.yaml",
		},
	}
	crdstore.MustSeed(client.crdStore, client.catalogItems["test-item"])
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "test-bundle", Namespace: "test-ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "test-item", Namespace: "test-ns"}},
			},
		},
	}
	crdstore.MustSeed(client.crdStore, client.composedBundles["test-bundle"])

	// Create a KeyVerifier for API key operations.
	keyStore := newFakeKeyStore()
	kv := apikey.NewVerifier(keyStore, "test-ns")

	// Create a TicketIssuer for stream/ticket.
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	ticketIssuer := auth.NewTicketIssuer(signer, "https://bff.test", 30*time.Second)
	ticketVerifier := auth.NewTicketVerifier(signer.PublicKey(), "https://bff.test")

	// Create a controller-runtime client for CRD operations (used by preview).
	crClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		CRClient:          crClient,
		Authorizer:        adminTestAuthorizer(),
		KeyVerifier:       kv,
		TicketIssuer:      ticketIssuer,
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		Brood:             newFakeBrood(client),
		BroodOps:          &stubBroodOps{},
		BroodSchedules:    &stubBroodSchedules{listResp: []bus.BroodScheduleResponse{{Namespace: "test-ns", Name: "test-sched"}}},
		ConfigBrood:       &stubConfigBrood{client: client, operatorNs: "test-ns"},
		IdentityConfig: IdentityConfig{
			Mode:         "local",
			CookieDomain: "example.com",
			DefaultRead:  true,
		},
	}

	// Contract-test auth provider: accepts "valid-admin-token" → adminClaims.
	mockAuth := &stubProvider{
		mode:   auth.AuthModeLocal,
		cookie: "example.com",
	}
	// Override Verify to return adminClaims for our test token.
	mockAuth.verifyFn = func(token string) (*auth.Claims, error) {
		if token == "valid-admin-token" {
			return adminClaims, nil
		}
		if token == "valid-op-token" {
			return operatorClaims, nil
		}
		return nil, http.ErrAbortHandler
	}

	rootMux := http.NewServeMux()
	// Wrap the router in the auth middleware.
	apiHandler := auth.AuthMiddleware(mockAuth, kv, ticketVerifier, nil, NewRouter(deps), slog.Default())
	rootMux.Handle("/api/v1/", apiHandler)

	// Stub /healthz — returns 200 text/plain.
	rootMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Stub /version — returns N7 JSON.
	rootMux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"component": "bff", "version": "test"})
	})

	srv := httptest.NewServer(rootMux)

	// Load the embedded spec.
	loader := &openapi3.Loader{IsExternalRefsAllowed: false}
	doc, err := loader.LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	return srv, doc
}

// stubConfigBrood implements ConfigBrood over the test client for contract tests.
type stubConfigBrood struct {
	client     *fakeResourceClient
	operatorNs string
}

func (f *stubConfigBrood) ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	items, err := f.client.ListComposedBundleCRDs(ctx, ns)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}
func (f *stubConfigBrood) GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	cr, err := f.client.GetComposedBundleCRD(ctx, name, ns)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *stubConfigBrood) CreateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) UpdateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error {
	return nil
}
func (f *stubConfigBrood) PauseComposedBundle(ctx context.Context, cluster, ns, name string, paused bool) error {
	// Check bundle exists - return not_found for missing bundles
	_, err := f.client.GetComposedBundleCRD(ctx, name, ns)
	if err != nil {
		return &BroodError{Code: bus.CodeNotFound, Msg: "bundle not found"}
	}
	return nil
}
func (f *stubConfigBrood) ComposeBundle(ctx context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error) {
	return &bus.BundleComposePreview{
		Errors:              []string{},
		Warnings:            []string{},
		Missing:             []string{},
		Drifted:             []string{},
		UnresolvedVariables: []string{},
	}, nil
}
func (f *stubConfigBrood) ListCatalogItems(ctx context.Context, cluster, ns string, filter CatalogItemFilter) ([]json.RawMessage, string, error) {
	items, err := f.client.ListCatalogItemCRDs(ctx, ns, "")
	if err != nil {
		return nil, "", err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(controller.ProjectCatalogItemSummary(cr))
		raw[i] = r
	}
	return raw, f.operatorNs, nil
}
func (f *stubConfigBrood) GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	cr, err := f.client.GetCatalogItemCRD(ctx, name, ns)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *stubConfigBrood) ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	items, err := f.client.ListCatalogSourceCRDs(ctx, ns)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}
func (f *stubConfigBrood) GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	cr, err := f.client.GetCatalogSourceCRD(ctx, name, ns)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *stubConfigBrood) CreateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) UpdateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error {
	return nil
}
func (f *stubConfigBrood) SyncCatalogSource(ctx context.Context, cluster, ns, name string) error {
	return nil
}
func (f *stubConfigBrood) ListJenkinsRoles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	items, err := f.client.ListJenkinsRoleCRDs(ctx)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}
func (f *stubConfigBrood) GetJenkinsRole(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	cr, err := f.client.GetJenkinsRoleCRD(ctx, name)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *stubConfigBrood) CreateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) UpdateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) DeleteJenkinsRole(ctx context.Context, cluster, name string) error {
	return nil
}
func (f *stubConfigBrood) ListJenkinsRoleBindings(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	items, err := f.client.ListJenkinsRoleBindingCRDs(ctx)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}
func (f *stubConfigBrood) GetJenkinsRoleBinding(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	cr, err := f.client.GetJenkinsRoleBindingCRD(ctx, name)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *stubConfigBrood) CreateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) UpdateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *stubConfigBrood) DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error {
	return nil
}

func (f *stubConfigBrood) GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	cr, err := f.client.GetProvisioningDefaultsCRD(ctx, name)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}

func (f *stubConfigBrood) UpdateProvisioningDefaults(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

func (f *stubConfigBrood) ListVersionProfiles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	items, err := f.client.ListJenkinsVersionProfileCRDs(ctx)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}

func (f *stubConfigBrood) GetVersionProfile(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	cr, err := f.client.GetJenkinsVersionProfileCRD(ctx, name)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}

func (f *stubConfigBrood) CreateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

func (f *stubConfigBrood) UpdateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

func (f *stubConfigBrood) DeleteVersionProfile(ctx context.Context, cluster, name string) error {
	return nil
}

func (f *stubConfigBrood) ViewVersionProfiles(ctx context.Context, cluster string) ([]bus.VersionProfileView, error) {
	items, err := f.client.ListJenkinsVersionProfileCRDs(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]bus.VersionProfileView, 0, len(items))
	for _, cr := range items {
		raw, _ := json.Marshal(cr)
		views = append(views, bus.VersionProfileView{Item: raw})
	}
	return views, nil
}

// TestContract runs every registered golden case through request+response
// validation against the spec, then runs the two coverage gates.
func TestContract(t *testing.T) {
	srv, doc := newContractServer(t)
	defer srv.Close()

	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("create router: %v", err)
	}

	// Load the spec for the coverage check.
	specDoc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	// Declare validation input variables used across request+response validation.
	var (
		reqValidationInput *openapi3filter.RequestValidationInput
	)

	// Use a noop auth func for security scheme validation.
	authFunc := openapi3filter.NoopAuthenticationFunc

	for _, c := range contractCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			var bodyReader io.Reader
			switch {
			case c.RawBody != nil:
				bodyReader = bytes.NewReader(c.RawBody)
			case c.Body != nil:
				js, err := json.Marshal(c.Body)
				if err != nil {
					t.Fatalf("marshal body: %v", err)
				}
				bodyReader = strings.NewReader(string(js))
			}

			// Build a request targeting the test server.
			fullURL := srv.URL + c.Path
			req, err := http.NewRequest(c.Method, fullURL, bodyReader)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			switch {
			case c.RawBody != nil && c.ContentType != "":
				req.Header.Set("Content-Type", c.ContentType)
			case c.Body != nil:
				req.Header.Set("Content-Type", "application/json")
			}

			// Inject auth claims via context if provided.
			if c.Claims != nil {
				req = req.WithContext(contextWithClaims(req.Context(), c.Claims))
				// Also set the bearer header for the auth middleware.
				token := "valid-admin-token"
				if c.Claims == operatorClaims {
					token = "valid-op-token"
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}

			// Validate the request against the spec (skip SSE/opaque ops).
			if !c.SkipSpecValidation {
				route, pathParams, err := router.FindRoute(req)
				if err != nil {
					t.Fatalf("route not found: %v (path: %s %s)", err, c.Method, c.Path)
				}

				reqValidationInput = &openapi3filter.RequestValidationInput{
					Request:    req,
					Route:      route,
					PathParams: pathParams,
					Options: &openapi3filter.Options{
						AuthenticationFunc: authFunc,
					},
				}
				if err := openapi3filter.ValidateRequest(context.Background(), reqValidationInput); err != nil {
					t.Fatalf("request validation: %v", err)
				}
			}

			// Execute the request against the test server.
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Read body for status assertion and N1 check.
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			// Assert status code.
			if resp.StatusCode != c.WantStatus {
				t.Errorf("expected status %d, got %d; body: %s", c.WantStatus, resp.StatusCode, string(body))
			}

			// Validate the response against the spec.
			if !c.SkipSpecValidation {
				// Replace the consumed body.
				resp.Body = io.NopCloser(bytes.NewReader(body))
				respValidationInput := &openapi3filter.ResponseValidationInput{
					RequestValidationInput: reqValidationInput,
					Status:                 resp.StatusCode,
					Header:                 resp.Header,
					Body:                   resp.Body,
				}
				if err := openapi3filter.ValidateResponse(context.Background(), respValidationInput); err != nil {
					t.Fatalf("response validation: %v (status %d)", err, resp.StatusCode)
				}
			}

			// N1 check: error responses must have application/json content type.
			if c.WantStatus >= 400 {
				ct := resp.Header.Get("Content-Type")
				if !strings.HasPrefix(ct, "application/json") && !c.SkipSpecValidation {
					t.Errorf("expected Content-Type application/json for error %d, got %q", c.WantStatus, ct)
				}
			}
		})
	}

	// Coverage gate 1: every spec operation (minus excluded IDs) has >=1 case.
	t.Run("coverage-gate-1-operations-covered", func(t *testing.T) {
		excludedIDs := map[string]bool{
			"streamActivity":         true,
			"streamBrood":            true,
			"streamControllerEvents": true,
			"streamControllerMite":   true,
			"mcpPost":                true,
			"mcpGet":                 true,
			"mcpDelete":              true,
			"getDocs":                true,
			"getDocsAsset":           true,
			"getOpenAPISpec":         true,
			"authLogin":              true,
			"authCallback":           true,
		}

		covered := make(map[string]bool)
		for _, c := range contractCases {
			if !c.SkipSpecValidation {
				specOp := findSpecOperation(specDoc, c.Method, c.Path)
				if specOp != "" {
					covered[specOp] = true
				}
			}
		}

		var missing []string
		for specPath, pathItem := range specDoc.Paths.Map() {
			for _, op := range pathItem.Operations() {
				if excludedIDs[op.OperationID] {
					continue
				}
				if !covered[op.OperationID] {
					missing = append(missing, op.OperationID)
				}
			}
			_ = specPath
		}

		if len(missing) > 0 {
			t.Errorf("update api/openapi/ + contract_test.go together\n  missing cases for: %v", missing)
		}
	})

	// Coverage gate 2: every routeManifest entry matches a spec path+method.
	t.Run("coverage-gate-2-manifest-matches-spec", func(t *testing.T) {
		var unmapped []string
		for _, r := range routeManifest {
			found := false
			for specPath, pathItem := range specDoc.Paths.Map() {
				for method := range pathItem.Operations() {
					if strings.EqualFold(method, r.Method) && specPath == r.Path {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				unmapped = append(unmapped, r.Method+" "+r.Path)
			}
		}
		if len(unmapped) > 0 {
			t.Errorf("update api/openapi/ + contract_test.go together\n  unmatched manifest entries: %v", unmapped)
		}
	})
}

// findSpecOperation looks up the operationId for a given method+path in the spec
// using the gorillamux router for template matching.
func findSpecOperation(doc *openapi3.T, method, path string) string {
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return ""
	}
	// Build a minimal request just for route matching.
	req, err := http.NewRequest(method, "http://placeholder"+path, nil)
	if err != nil {
		return ""
	}
	route, _, err := router.FindRoute(req)
	if err != nil {
		return ""
	}
	if route != nil && route.Operation != nil {
		return route.Operation.OperationID
	}
	return ""
}

// fakeBrood is a simple Brood stub for contract tests.
type fakeBrood struct {
	local                    string
	client                   *fakeResourceClient
	discoverNamespacesResp   *bus.NamespacesListResponse
	discoverNamespacesErr    error
	discoverNamespacesCalled bool
	// updatePatch captures the last raw patch handed to Update, so tests can
	// assert exactly what the handler forwards to the bus.
	updatePatch json.RawMessage
}

func newFakeBrood(client *fakeResourceClient) Brood {
	return &fakeBrood{local: "core", client: client}
}

func (f *fakeBrood) LocalCluster() string { return f.local }

func (f *fakeBrood) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	return []ClusterInfo{{ClusterInfo: bus.ClusterInfo{Name: f.local, State: bus.ClusterStateActive}, Core: true, Healthy: true}}, nil
}

func (f *fakeBrood) IsKnown(ctx context.Context, cluster string) bool { return true }

func (f *fakeBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	items, err := f.client.ListControllerCRDs(ctx, ns)
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: f.local, OK: false, Error: err.Error()}}, err
	}
	cc := make([]ClusterController, len(items))
	for i, cr := range items {
		cc[i] = ClusterController{Cluster: f.local, CR: cr}
	}
	return cc, []ClusterFanoutStatus{{Name: f.local, OK: true}}, nil
}

func (f *fakeBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	return f.client.GetControllerCRD(ctx, name, ns)
}

func (f *fakeBrood) Create(ctx context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	var cr v1alpha1.Controller
	if err := json.Unmarshal(req.Controller, &cr); err != nil {
		return nil, nil, err
	}
	// Production create persists via ApplyControllerSpecSSA (brood create);
	// the fake's SSA stub does not store new objects, so record the controller
	// here the same way the removed ApplyControllerCRD stub did, keeping the
	// created object visible to the fake's subsequent Get/List. Both the map
	// and the crdstore fake are seeded: the local-cluster read path
	// (getController) reads the crdstore fake, while List/Get through Brood
	// read the map.
	if f.client.controllers == nil {
		f.client.controllers = map[string]*v1alpha1.Controller{}
	}
	f.client.controllers[cr.Name] = cr.DeepCopy()
	if f.client.crdStore != nil {
		crdstore.MustSeed(f.client.crdStore, cr.DeepCopy())
	}
	return &cr, nil, nil
}

func (f *fakeBrood) Preflight(ctx context.Context, cluster string, req ControllersCreateArgs) ([]bus.Check, error) {
	return nil, nil
}

func (f *fakeBrood) Update(ctx context.Context, cluster, ns, name string, patch json.RawMessage, fieldManager string, force bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	f.updatePatch = patch
	existing, err := f.client.GetControllerCRD(ctx, name, ns)
	if err != nil {
		return nil, nil, nil, err
	}
	var patchMap map[string]interface{}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, nil, nil, err
	}
	existingJSON, _ := json.Marshal(existing)
	var merged map[string]interface{}
	json.Unmarshal(existingJSON, &merged)
	controller.MergeMap(merged, patchMap)
	mergedJSON, _ := json.Marshal(merged)
	var updated v1alpha1.Controller
	json.Unmarshal(mergedJSON, &updated)
	// Extract sparse spec for SSA.
	var specPatch map[string]any
	if specRaw, ok := patchMap["spec"]; ok {
		specJSON, _ := json.Marshal(specRaw)
		json.Unmarshal(specJSON, &specPatch)
	}
	// Mirror the brood path (command_crud.go): the seam returns the requested
	// removals that did not take effect, tested against the applied object's
	// unstructured spec.
	_, unapplied, err := f.client.ApplyControllerSpecSSA(ctx, ns, name, specPatch, fieldManager, force)
	if err != nil {
		return nil, nil, nil, err
	}
	return &updated, nil, unapplied, nil
}

func (f *fakeBrood) Delete(ctx context.Context, cluster, ns, name string) error {
	return f.client.DeleteControllerCRD(ctx, name, ns)
}

func (f *fakeBrood) DeletePod(ctx context.Context, cluster, ns, name string) error { return nil }
func (f *fakeBrood) Drain(ctx context.Context, cluster, requestedBy string) (string, error) {
	if cluster == f.local {
		return "", &BroodError{Code: bus.CodeInvalid, Msg: "the core cluster cannot be drained"}
	}
	return "draining", nil
}
func (f *fakeBrood) DrainCancel(ctx context.Context, cluster, requestedBy string) (string, error) {
	return "", &BroodError{Code: bus.CodeConflict, Msg: "cluster is not draining"}
}
func (f *fakeBrood) StateOf(ctx context.Context, cluster string) (string, error) {
	return "active", nil
}

func (f *fakeBrood) DiscoverNamespaces(ctx context.Context, cluster string) (*bus.NamespacesListResponse, error) {
	f.discoverNamespacesCalled = true
	if f.discoverNamespacesResp != nil || f.discoverNamespacesErr != nil {
		return f.discoverNamespacesResp, f.discoverNamespacesErr
	}
	return &bus.NamespacesListResponse{}, nil
}
