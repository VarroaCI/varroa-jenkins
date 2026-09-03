package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

func TestControllerEndpoint(t *testing.T) {
	fc := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client:            fc,
		Store:             storeFromFake(fc),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})
	tests := []struct {
		name string
		cr   *v1alpha1.Controller
		want string
	}{
		{
			name: "status endpoint wins",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
				Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "x.example.com"}},
				Status:     v1alpha1.ControllerStatus{Endpoint: "https://from-status.example.com"},
			},
			want: "https://from-status.example.com",
		},
		{
			name: "no ingress spec",
			cr:   &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"}},
			want: "",
		},
		{
			name: "empty host",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
				Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{}},
			},
			want: "",
		},
		{
			name: "subdomain mode",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
				Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "ci.example.com"}},
			},
			want: "https://ci.example.com",
		},
		{
			name: "path mode with trailing slash",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
				Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "varroa.example.com", Mode: v1alpha1.RoutingModePath}},
			},
			want: "https://varroa.example.com/jenkins/team-a/ci/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := srv.controllerEndpoint(tt.cr); got != tt.want {
				t.Errorf("controllerEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newRoutingTestServer() (*Server, *fakeResourceClient) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{}
	client.namespaces["team-a"] = true
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		Brood:             newFakeBrood(client),
	})
	return srv, client
}

func TestRoutingModeInResponses(t *testing.T) {
	srv, client := newRoutingTestServer()
	client.controllers["path-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "path-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "varroa.example.com", Mode: v1alpha1.RoutingModePath}},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["path-ctrl"])
	client.controllers["sub-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "ci.example.com"}},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["sub-ctrl"])

	// List: routingMode present and never empty.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	modes := map[string]string{}
	endpoints := map[string]string{}
	for _, item := range envelope.Items {
		name, _ := item["name"].(string)
		mode, _ := item["routingMode"].(string)
		ep, _ := item["endpoint"].(string)
		modes[name] = mode
		endpoints[name] = ep
	}
	if modes["path-ctrl"] != "path" || modes["sub-ctrl"] != "subdomain" {
		t.Errorf("list routingModes = %v, want path-ctrl:path sub-ctrl:subdomain", modes)
	}
	if endpoints["path-ctrl"] != "https://varroa.example.com/jenkins/team-a/path-ctrl/" {
		t.Errorf("path endpoint = %q, want derived path URL with trailing slash", endpoints["path-ctrl"])
	}

	// Detail: routingMode present.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/controllers/team-a/path-ctrl", nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "path-ctrl")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["routingMode"] != "path" {
		t.Errorf("detail routingMode = %v, want path", detail["routingMode"])
	}
}

func postCreateController(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a", strings.NewReader(body))
	srv.handleCreateController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a")
	return w
}

func TestCreateControllerRoutingValidation(t *testing.T) {
	t.Run("invalid mode enum rejected", func(t *testing.T) {
		srv, _ := newRoutingTestServer()
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"x.example.com","mode":"proxy"}}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ingressSpec.mode") {
			t.Errorf("error body should name the rule, got %s", w.Body.String())
		}
	})

	t.Run("path mode host mismatch rejected when dashboard host known", func(t *testing.T) {
		srv, _ := newRoutingTestServer()
		srv.deps.DashboardHost = "varroa.example.com"
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"other.example.com","mode":"path"}}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "dashboard host") {
			t.Errorf("error body should name the rule, got %s", w.Body.String())
		}
	})

	t.Run("path mode matching host accepted", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		srv.deps.DashboardHost = "varroa.example.com"
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"varroa.example.com","mode":"path"}}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
		if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.crdStore, "ci", "team-a"); err != nil {
			t.Error("controller should have been applied")
		}
	})

	t.Run("path mode allowed with unknown dashboard host", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"anything.example.com","mode":"path"}}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 (warn-and-proceed), got %d: %s", w.Code, w.Body.String())
		}
		if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.crdStore, "ci", "team-a"); err != nil {
			t.Error("controller should have been applied")
		}
	})

	t.Run("invalid annotation key rejected", func(t *testing.T) {
		srv, _ := newRoutingTestServer()
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"x.example.com","annotations":{"not a valid key!":"v"}}}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ingressSpec.annotations") {
			t.Errorf("error body should name the rule, got %s", w.Body.String())
		}
	})

	t.Run("valid annotation key accepted", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		w := postCreateController(t, srv,
			`{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"x.example.com","annotations":{"nginx.ingress.kubernetes.io/rewrite-target":"/"}}}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
		if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.crdStore, "ci", "team-a"); err != nil {
			t.Error("controller should have been applied")
		}
	})
}

func patchController(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/controllers/team-a/ci", strings.NewReader(body))
	srv.handleUpdateController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	return w
}

func TestUpdateControllerRoutingValidation(t *testing.T) {
	seed := func(client *fakeResourceClient, mode string) {
		client.controllers["ci"] = &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "ci.example.com", Mode: mode}},
		}
		crdstore.MustSeed(client.crdStore, client.controllers["ci"])
	}

	// Ingress-mode immutability and enum/annotation validation moved to the
	// operator's HandleUpdate when the BFF's local direct-apply fallback was
	// deleted (multicluster-control-plane). The BFF now routes every PATCH to
	// the bus; these two cases pin that routing succeeds for valid patches.
	t.Run("equivalent rewrite accepted", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		seed(client, "")
		w := patchController(t, srv, `{"spec":{"ingressSpec":{"mode":"subdomain"}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 (\"\" and subdomain are equivalent), got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("valid annotation key on update accepted", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		seed(client, "")
		w := patchController(t, srv, `{"spec":{"ingressSpec":{"mode":"subdomain","annotations":{"nginx.ingress.kubernetes.io/rewrite-target":"/"}}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestControllerDetailApplyResult(t *testing.T) {
	srv, client := newRoutingTestServer()
	now := metav1.Now()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Host: "ci.example.com"}},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
			LastApplyResult: &v1alpha1.ApplyResult{
				Hash:      "hash-abc",
				Timestamp: now,
				Succeeded: true,
				Sections: []v1alpha1.ApplySectionResult{
					{Name: "config", OK: true},
					{Name: "rbac", OK: true},
					{Name: "plugins", OK: true},
					{Name: "items", OK: true},
				},
			},
			ApplyHistory: []v1alpha1.ApplyResult{
				{
					Hash:      "hash-abc",
					Timestamp: now,
					Succeeded: true,
					Sections: []v1alpha1.ApplySectionResult{
						{Name: "config", OK: true},
						{Name: "rbac", OK: true},
						{Name: "plugins", OK: true},
						{Name: "items", OK: true},
					},
				},
				{
					Hash:      "hash-old",
					Timestamp: now,
					Succeeded: false,
					Sections: []v1alpha1.ApplySectionResult{
						{Name: "config", OK: true},
						{Name: "rbac", OK: true},
						{Name: "plugins", OK: false, Error: "plugin install failed"},
						{Name: "items", OK: true},
					},
				},
			},
		},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	// Detail endpoint: should include lastApplyResult and applyHistory.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers/team-a/ci", nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	lar, ok := detail["lastApplyResult"].(map[string]interface{})
	if !ok {
		t.Fatal("expected lastApplyResult in detail response")
	}
	if lar["succeeded"] != true {
		t.Error("expected lastApplyResult.succeeded=true")
	}
	if lar["hash"] != "hash-abc" {
		t.Errorf("expected hash=hash-abc, got %v", lar["hash"])
	}
	if ts, _ := lar["timestamp"].(string); ts != now.Format(time.RFC3339) {
		t.Errorf("expected timestamp in RFC3339, got %v", lar["timestamp"])
	}
	sections, _ := lar["sections"].([]interface{})
	if len(sections) != 4 {
		t.Fatalf("expected 4 sections, got %d", len(sections))
	}
	wantOrder := []string{"config", "rbac", "plugins", "items"}
	for i, s := range wantOrder {
		sec, _ := sections[i].(map[string]interface{})
		if sec["name"] != s {
			t.Errorf("section[%d] name: want %q, got %v", i, s, sec["name"])
		}
	}

	history, _ := detail["applyHistory"].([]interface{})
	if len(history) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(history))
	}
	h1, _ := history[0].(map[string]interface{})
	if h1["hash"] != "hash-abc" {
		t.Error("expected history[0].hash=hash-abc")
	}
	h2, _ := history[1].(map[string]interface{})
	if h2["hash"] != "hash-old" {
		t.Error("expected history[1].hash=hash-old")
	}
	if h2["succeeded"] != false {
		t.Error("expected history[1].succeeded=false")
	}

	// List endpoint: should NOT include apply-result fields.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w2, req2.WithContext(contextWithClaims(req2.Context(), adminClaims)))
	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, item := range envelope.Items {
		if _, exists := item["lastApplyResult"]; exists {
			t.Error("list response must not include lastApplyResult")
		}
		if _, exists := item["applyHistory"]; exists {
			t.Error("list response must not include applyHistory")
		}
	}
}

func TestControllerCreateDetailRoundTripProbes(t *testing.T) {
	srv, client := newRoutingTestServer()
	body := `{"metadata":{"name":"ci"},"spec":{"ingressSpec":{"host":"ci.example.com"},"probes":{"startup":{"failureThreshold":60},"liveness":{"disabled":true}}}}`
	w := postCreateController(t, srv, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.crdStore, "ci", "team-a"); err != nil {
		t.Fatal("controller should have been applied")
	}
	detail := getDetail(t, srv)
	probes, ok := detail["probes"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected probes in detail response, got %+v", detail["probes"])
	}
	startup, _ := probes["startup"].(map[string]interface{})
	if startup == nil || startup["failureThreshold"] != float64(60) {
		t.Fatalf("unexpected startup probes: %+v", startup)
	}
	liveness, _ := probes["liveness"].(map[string]interface{})
	if liveness == nil || liveness["disabled"] != true {
		t.Fatalf("unexpected liveness probes: %+v", liveness)
	}
}

// TestClusterDispatchTrailingSlash pins the trailing-slash 404: the joined
// sub-resource key for …/{name}/ is also "" and must not reach the bare
// resource handler (a stray-slash DELETE would otherwise delete the controller).
func TestClusterDispatchTrailingSlash(t *testing.T) {
	srv, client := newRoutingTestServer()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	do := func(method, path string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		srv.handleClusterDispatch(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
		return w.Code
	}

	// Baseline: the canonical path resolves.
	if code := do(http.MethodGet, "/clusters/core/controllers/team-a/ci"); code != http.StatusOK {
		t.Fatalf("GET canonical path: expected 200, got %d", code)
	}
	// Trailing slash is not the resource.
	if code := do(http.MethodGet, "/clusters/core/controllers/team-a/ci/"); code != http.StatusNotFound {
		t.Errorf("GET trailing slash: expected 404, got %d", code)
	}
	if code := do(http.MethodDelete, "/clusters/core/controllers/team-a/ci/"); code != http.StatusNotFound {
		t.Errorf("DELETE trailing slash: expected 404, got %d", code)
	}
}

func TestClusterDispatchNamespaces(t *testing.T) {
	srv, _ := newRoutingTestServer()
	// Note: when Brood is nil, IsKnown treats all clusters as known.
	// The unknown-cluster 404 only applies when Brood is set and the cluster
	// is not in the KV directory.

	do := func(method, path string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		srv.handleClusterDispatch(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
		return w.Code
	}

	// New cluster-scoped path returns 200 (local cluster, Brood nil).
	if code := do(http.MethodGet, "/clusters/core/namespaces/deployable"); code != http.StatusOK {
		t.Errorf("GET /clusters/core/namespaces/deployable: expected 200, got %d", code)
	}

	// Trailing slash → 404.
	if code := do(http.MethodGet, "/clusters/core/namespaces/deployable/"); code != http.StatusNotFound {
		t.Errorf("GET trailing slash: expected 404, got %d", code)
	}

	// POST to the new path → 405.
	if code := do(http.MethodPost, "/clusters/core/namespaces/deployable"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /clusters/core/namespaces/deployable: expected 405, got %d", code)
	}

	// Unknown sub-path under namespaces → 404.
	if code := do(http.MethodGet, "/clusters/core/namespaces/other"); code != http.StatusNotFound {
		t.Errorf("GET /clusters/core/namespaces/other: expected 404, got %d", code)
	}
}

func TestOldDeployableNamespacesPathReturns404(t *testing.T) {
	// The old flat path /namespaces/deployable is removed (D5 lockstep).
	mux := NewRouter(&Dependencies{
		Client: newFakeResourceClient(),
		Logger: slog.Default(),
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/namespaces/deployable")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("old path /namespaces/deployable: expected 404, got %d", resp.StatusCode)
	}
}

func TestOldProvisioningPathsReturn404(t *testing.T) {
	// The flat provisioning/version-profile paths moved under
	// /clusters/{cluster}/... (add-remote-provisioning-config-authoring, D5
	// lockstep). The old shapes must be gone, not served alongside.
	mux := NewRouter(&Dependencies{
		Client: newFakeResourceClient(),
		Logger: slog.Default(),
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, path := range []string{
		"/api/v1/provisioningdefaults/varroa-defaults",
		"/api/v1/provisioning/config",
		"/api/v1/version-profiles",
	} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("old flat path %s: expected 404, got %d", path, resp.StatusCode)
		}
	}
}

// remoteRowBrood decorates the contract-test fakeBrood with one extra remote
// cluster row so handler tests can exercise the aggregated-list remote path.
type remoteRowBrood struct {
	Brood
	remote ClusterController
}

func (f *remoteRowBrood) ListAll(ctx context.Context, ns, filter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	cc, cs, err := f.Brood.ListAll(ctx, ns, filter)
	cc = append(cc, f.remote)
	cs = append(cs, ClusterFanoutStatus{Name: f.remote.Cluster, OK: true})
	return cc, cs, err
}

// TestHandleControllersRemoteMiteFromStatus asserts that remote rows surface
// mite fields from the remote CR's status.miteStatus instead of the
// (guaranteed-miss) local KV merge.
func TestHandleControllersRemoteMiteFromStatus(t *testing.T) {
	srv, client := newRoutingTestServer()
	client.controllers["local-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["local-ctrl"])
	remoteCR := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-ctrl", Namespace: "team-b"},
		Status: v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{
			Connected:      true,
			Version:        "1.2.3",
			JenkinsVersion: "2.462.3",
			JenkinsHealth:  "healthy",
		}},
	}
	srv.deps.Brood = &remoteRowBrood{
		Brood:  newFakeBrood(client),
		remote: ClusterController{Cluster: "dev-cluster", CR: remoteCR},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var remote map[string]interface{}
	for _, item := range envelope.Items {
		if item["name"] == "remote-ctrl" {
			remote = item
		}
	}
	if remote == nil {
		t.Fatalf("remote row missing from aggregated list: %v", envelope.Items)
	}
	if remote["cluster"] != "dev-cluster" {
		t.Errorf("cluster = %v, want dev-cluster", remote["cluster"])
	}
	if remote["miteConnected"] != true {
		t.Error("remote row should surface miteConnected from status.miteStatus")
	}
	if remote["miteVersion"] != "1.2.3" || remote["jenkinsVersion"] != "2.462.3" || remote["jenkinsHealth"] != "healthy" {
		t.Errorf("remote mite fields = %v/%v/%v, want 1.2.3/2.462.3/healthy",
			remote["miteVersion"], remote["jenkinsVersion"], remote["jenkinsHealth"])
	}
}

// TestHandleControllerDetailCluster pins the bug where the detail response
// never echoed back the cluster it was requested against. The frontend
// derives its cluster for mutation routing from this field, so an empty
// value here breaks every PATCH built from the detail page.
func TestHandleControllerDetailCluster(t *testing.T) {
	srv, client := newRoutingTestServer()
	srv.deps.Brood = newFakeBrood(client)
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers/team-a/ci", nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["cluster"] != "core" {
		t.Errorf("cluster = %v, want core", detail["cluster"])
	}
}

// remoteGetBrood decorates the contract-test fakeBrood so a single Get call
// for a chosen (cluster, ns, name) returns a canned remote CR, letting
// handler tests exercise the single-controller-detail remote path.
type remoteGetBrood struct {
	Brood
	hiveCluster string
	remoteNS    string
	remoteName  string
	remoteCR    *v1alpha1.Controller
}

func (f *remoteGetBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	if cluster == f.hiveCluster && ns == f.remoteNS && name == f.remoteName {
		return f.remoteCR, nil
	}
	return f.Brood.Get(ctx, cluster, ns, name)
}

// TestHandleControllerDetailRemoteMiteFromStatus asserts that a remote-cluster
// row surfaces mite telemetry from the remote CR's status.miteStatus instead
// of the guaranteed-miss local mite registry lookup.
func TestHandleControllerDetailRemoteMiteFromStatus(t *testing.T) {
	srv, client := newRoutingTestServer()
	lastSeen := metav1.NewTime(time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	certExpiry := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	remoteCR := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-ctrl", Namespace: "team-b"},
		Status: v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{
			Connected:      true,
			Version:        "1.2.3",
			LastSeen:       &lastSeen,
			CertExpiry:     &certExpiry,
			JenkinsVersion: "2.462.3",
			JenkinsHealth:  "healthy",
		}},
	}
	srv.deps.Brood = &remoteGetBrood{
		Brood:       newFakeBrood(client),
		hiveCluster: "dev-cluster",
		remoteNS:    "team-b",
		remoteName:  "remote-ctrl",
		remoteCR:    remoteCR,
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/controllers/team-b/remote-ctrl", nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "dev-cluster", "team-b", "remote-ctrl")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["miteConnected"] != true {
		t.Error("remote detail should surface miteConnected from status.miteStatus")
	}
	if detail["miteVersion"] != "1.2.3" || detail["jenkinsVersion"] != "2.462.3" || detail["jenkinsHealth"] != "healthy" {
		t.Errorf("remote mite fields = %v/%v/%v, want 1.2.3/2.462.3/healthy",
			detail["miteVersion"], detail["jenkinsVersion"], detail["jenkinsHealth"])
	}
	if detail["lastSeen"] != lastSeen.Format("2006-01-02T15:04:05Z") {
		t.Errorf("lastSeen = %v, want %v", detail["lastSeen"], lastSeen.Format("2006-01-02T15:04:05Z"))
	}
	if detail["certExpiry"] != certExpiry.Format("2006-01-02") {
		t.Errorf("certExpiry = %v, want %v", detail["certExpiry"], certExpiry.Format("2006-01-02"))
	}
}

// TestCreateControllerRemoteBundleRefSkipsLocalValidation asserts that,
// because ComposedBundles are cluster-local, a create targeting a remote
// cluster must not validate spec.composedBundleRef against the core's API —
// the target operator validates it. Local creates keep the core-side check.
func TestCreateControllerRemoteBundleRefSkipsLocalValidation(t *testing.T) {
	srv, client := newRoutingTestServer()
	srv.deps.Brood = newFakeBrood(client)
	body := `{"metadata":{"name":"mc"},"spec":{"composedBundleRef":{"name":"remote-only-bundle"}}}`

	// Remote target: the bundle does not exist on the core, but the create
	// must be routed to the target cluster anyway.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a", strings.NewReader(body))
	srv.handleCreateController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "dev-cluster", "team-a")
	if w.Code != http.StatusCreated {
		t.Fatalf("remote create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Local target: the core-side existence check still applies.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/controllers/team-a", strings.NewReader(body))
	srv.handleCreateController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("local create with missing bundle: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not found") {
		t.Errorf("local create error should name the missing bundle, got %s", w.Body.String())
	}
}

// lastSeenMiteTransport reports one connected mite with a fixed heartbeat, so
// the list handler's local path has a lastSeen to carry.
type lastSeenMiteTransport struct {
	transport.Transport
	lastSeen time.Time
}

func (m *lastSeenMiteTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "mite-1.0", m.lastSeen, m.lastSeen.Add(72 * time.Hour), true
}

func (m *lastSeenMiteTransport) Snapshot(_, _ string) *mitev1.StateSnapshot {
	return &mitev1.StateSnapshot{JenkinsVersion: "2.462.3", JenkinsHealth: "healthy"}
}

// remoteListBrood appends one remote-cluster row to the local fake's listing so
// the list handler's remote branch is exercised.
type remoteListBrood struct {
	Brood
	remoteCluster string
	remoteCR      *v1alpha1.Controller
}

func (b *remoteListBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	cc, cs, err := b.Brood.ListAll(ctx, ns, clusterFilter)
	cc = append(cc, ClusterController{Cluster: b.remoteCluster, CR: b.remoteCR})
	return cc, cs, err
}

// The dashboard reads staleness off the list, not the detail: without lastSeen
// on the summary every row renders "never seen". Local rows take it from the
// mite registry, remote rows from the remote operator's own status.miteStatus —
// a local registry lookup for a remote row is a guaranteed miss.
func TestHandleControllersListCarriesLastSeen(t *testing.T) {
	srv, client := newRoutingTestServer()
	localSeen := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	srv.deps.MiteRegistry = &lastSeenMiteTransport{lastSeen: localSeen}

	local := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
	}
	client.controllers["local-ctrl"] = local
	crdstore.MustSeed(client.crdStore, local)

	remoteSeen := metav1.NewTime(time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC))
	srv.deps.Brood = &remoteListBrood{
		Brood:         newFakeBrood(client),
		remoteCluster: "dev-cluster",
		remoteCR: &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "remote-ctrl", Namespace: "team-b"},
			Status: v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{
				Connected: true,
				Version:   "1.2.3",
				LastSeen:  &remoteSeen,
			}},
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	byName := map[string]map[string]interface{}{}
	for _, item := range envelope.Items {
		name, _ := item["name"].(string)
		byName[name] = item
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"local-ctrl", localSeen.Format("2006-01-02T15:04:05Z")},
		{"remote-ctrl", remoteSeen.Format("2006-01-02T15:04:05Z")},
	} {
		item, ok := byName[tc.name]
		if !ok {
			t.Fatalf("%s missing from the listing: %+v", tc.name, envelope.Items)
		}
		if item["lastSeen"] != tc.want {
			t.Errorf("%s lastSeen = %v, want %v", tc.name, item["lastSeen"], tc.want)
		}
	}
}

// absentMiteTransport reports no mite at all, the registry's view of a mite
// that has disconnected.
type absentMiteTransport struct {
	transport.Transport
}

func (m *absentMiteTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "", time.Time{}, time.Time{}, false
}

func (m *absentMiteTransport) Snapshot(_, _ string) *mitev1.StateSnapshot { return nil }

// The registry forgets a mite the moment it disconnects. A local controller
// that reported once must keep the operator's persisted last-seen time on both
// the list and the detail, not collapse into "never seen".
func TestLocalDisconnectedMiteKeepsPersistedLastSeen(t *testing.T) {
	srv, client := newRoutingTestServer()
	srv.deps.MiteRegistry = &absentMiteTransport{}
	seen := metav1.NewTime(time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	local := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
		Status: v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{
			Connected:      false,
			Version:        "0.1.0",
			JenkinsVersion: "2.555.3",
			JenkinsHealth:  "unreachable",
			LastSeen:       &seen,
		}},
	}
	client.controllers["local-ctrl"] = local
	crdstore.MustSeed(client.crdStore, local)
	want := seen.Format("2006-01-02T15:04:05Z")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(envelope.Items) != 1 || envelope.Items[0]["lastSeen"] != want {
		t.Errorf("list lastSeen = %v, want %v", envelope.Items, want)
	}
	if envelope.Items[0]["miteConnected"] == true {
		t.Errorf("disconnected mite must not read as connected: %v", envelope.Items[0])
	}
	// Last-known facts ride along with the time, as they do for remote rows.
	if envelope.Items[0]["jenkinsHealth"] != "unreachable" || envelope.Items[0]["jenkinsVersion"] != "2.555.3" || envelope.Items[0]["miteVersion"] != "0.1.0" {
		t.Errorf("list must carry persisted jenkinsHealth/jenkinsVersion/miteVersion: %v", envelope.Items[0])
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/controllers/team-a/local-ctrl", nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "", "team-a", "local-ctrl")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["lastSeen"] != want {
		t.Errorf("detail lastSeen = %v, want %v", detail["lastSeen"], want)
	}
	if detail["jenkinsHealth"] != "unreachable" || detail["jenkinsVersion"] != "2.555.3" || detail["miteConnected"] == true {
		t.Errorf("detail must carry persisted facts but not liveness: %v", detail)
	}
}

// blankHealthMiteTransport reports a live mite whose first snapshot after
// reconnect carries no health verdict yet.
type blankHealthMiteTransport struct {
	transport.Transport
	lastSeen time.Time
}

func (m *blankHealthMiteTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "mite-1.0", m.lastSeen, m.lastSeen.Add(72 * time.Hour), true
}

func (m *blankHealthMiteTransport) Snapshot(_, _ string) *mitev1.StateSnapshot {
	return &mitev1.StateSnapshot{JenkinsVersion: "2.555.3", JenkinsHealth: ""}
}

// A live snapshot without a verdict must not blank the persisted last-known
// health the seed just set.
func TestLiveSnapshotWithoutHealthKeepsPersistedVerdict(t *testing.T) {
	srv, client := newRoutingTestServer()
	now := time.Now().UTC().Truncate(time.Second)
	srv.deps.MiteRegistry = &blankHealthMiteTransport{lastSeen: now}
	seen := metav1.NewTime(now.Add(-time.Hour))
	local := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
		Status: v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{
			JenkinsHealth: "unreachable",
			LastSeen:      &seen,
		}},
	}
	client.controllers["local-ctrl"] = local
	crdstore.MustSeed(client.crdStore, local)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("expected one row, got %v", envelope.Items)
	}
	row := envelope.Items[0]
	if row["jenkinsHealth"] != "unreachable" {
		t.Errorf("live snapshot without a verdict blanked the seed: %v", row)
	}
	if row["lastSeen"] != now.Format("2006-01-02T15:04:05Z") || row["miteConnected"] != true {
		t.Errorf("live heartbeat must still win for lastSeen/liveness: %v", row)
	}
}
