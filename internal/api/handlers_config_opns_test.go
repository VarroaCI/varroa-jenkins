package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// configOpnsBrood reports two clusters. core is the BFF's own cluster. edge
// is a member whose heartbeat carries the operator namespace varroa-edge.
// The BFF operator namespace is varroa-system, so edge differs from it.
type configOpnsBrood struct {
	Brood
	clusters []ClusterInfo
	err      error
}

func newConfigOpnsBrood() *configOpnsBrood {
	return &configOpnsBrood{clusters: []ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "edge", OperatorNamespace: "varroa-edge"}, Healthy: true},
		// stale keeps its entry after the heartbeat TTL expires.
		{ClusterInfo: bus.ClusterInfo{Name: "stale", OperatorNamespace: "varroa-stale"}, Healthy: false},
	}}
}

func (b *configOpnsBrood) LocalCluster() string { return "core" }

func (b *configOpnsBrood) Clusters(context.Context) ([]ClusterInfo, error) {
	return b.clusters, b.err
}

func (b *configOpnsBrood) ListAll(context.Context, string, string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return nil, nil, nil
}

// configOpnsAuthorizer grants read on composedbundles, catalogitems and
// catalogsources to dev-user, scoped to team-a.
func configOpnsAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"composedbundles", "catalogitems", "catalogsources"}, Verbs: []string{"read"}},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
			RoleRef:  "developer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

func configOpnsServer(brood *configOpnsBrood, config ConfigBrood) *Server {
	return NewServer(&Dependencies{
		Authorizer:        configOpnsAuthorizer(),
		ConfigBrood:       config,
		Brood:             brood,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
}

func configOpnsRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return req.WithContext(contextWithClaims(req.Context(), scopedDevClaims))
}

func configOpnsNamespaces(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, 0, len(resp.Items))
	for _, raw := range resp.Items {
		var probe struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &probe)
		out = append(out, probe.Metadata.Namespace)
	}
	return out
}

func TestDispatchCatalogSources_RemoteOperatorNamespaceAllowed(t *testing.T) {
	srv := configOpnsServer(newConfigOpnsBrood(), &scopeTestConfigBrood{})
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/edge/catalogsources/varroa-edge/x")
	srv.dispatchCatalogSources(w, req, "edge", []string{"varroa-edge", "x"})
	if w.Code != http.StatusOK {
		t.Fatalf("get remote operator-namespace source: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDispatchCatalogSources_RemoteBFFNamespaceForbidden(t *testing.T) {
	srv := configOpnsServer(newConfigOpnsBrood(), &scopeTestConfigBrood{})
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/edge/catalogsources/varroa-system/x")
	srv.dispatchCatalogSources(w, req, "edge", []string{"varroa-system", "x"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("get BFF operator namespace on member: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDispatchComposedBundles_RemoteOperatorNamespaceAllowed(t *testing.T) {
	brood := &scopeTestConfigBrood{bundles: []json.RawMessage{
		scopeTestBundleRaw("team-a", "a"),
		scopeTestBundleRaw("varroa-edge", "starter"),
		scopeTestBundleRaw("varroa-system", "bff-starter"),
	}}
	srv := configOpnsServer(newConfigOpnsBrood(), brood)

	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/edge/composedbundles/varroa-edge/starter")
	srv.dispatchComposedBundles(w, req, "edge", []string{"varroa-edge", "starter"})
	if w.Code != http.StatusOK {
		t.Fatalf("get remote operator-namespace bundle: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = configOpnsRequest("/clusters/edge/composedbundles")
	srv.dispatchComposedBundles(w, req, "edge", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list remote bundles: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := configOpnsNamespaces(t, w.Body.Bytes())
	if len(got) != 2 || got[0] != "team-a" || got[1] != "varroa-edge" {
		t.Fatalf("list namespaces = %v, want [team-a varroa-edge]", got)
	}
}

func TestDispatchCatalogItems_RemoteOperatorNamespaceList(t *testing.T) {
	brood := &scopeTestConfigBrood{items: []json.RawMessage{
		scopeTestObject("team-a", "a"),
		scopeTestObject("varroa-edge", "starter"),
		scopeTestObject("varroa-system", "bff-starter"),
	}}
	srv := configOpnsServer(newConfigOpnsBrood(), brood)
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/edge/catalogitems")
	srv.dispatchCatalogItems(w, req, "edge", []string{})
	if w.Code != http.StatusOK {
		t.Fatalf("list remote catalog items: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := configOpnsNamespaces(t, w.Body.Bytes())
	if len(got) != 2 || got[0] != "team-a" || got[1] != "varroa-edge" {
		t.Fatalf("list namespaces = %v, want [team-a varroa-edge]", got)
	}
	var resp struct {
		OperatorNamespace string `json:"operatorNamespace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OperatorNamespace != "varroa-edge" {
		t.Fatalf("operatorNamespace = %q, want varroa-edge", resp.OperatorNamespace)
	}
}

func TestDispatchCatalogItems_LocalEchoesOperatorNamespace(t *testing.T) {
	srv := configOpnsServer(newConfigOpnsBrood(), &scopeTestConfigBrood{})
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/core/catalogitems")
	srv.dispatchCatalogItems(w, req, "core", []string{})
	if w.Code != http.StatusOK {
		t.Fatalf("list local catalog items: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OperatorNamespace string `json:"operatorNamespace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OperatorNamespace != "varroa-system" {
		t.Fatalf("operatorNamespace = %q, want varroa-system", resp.OperatorNamespace)
	}
}

func TestDispatchCatalogSources_UnhealthyMemberNoExemption(t *testing.T) {
	srv := configOpnsServer(newConfigOpnsBrood(), &scopeTestConfigBrood{})
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/stale/catalogsources/varroa-stale/x")
	srv.dispatchCatalogSources(w, req, "stale", []string{"varroa-stale", "x"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("get on unhealthy member: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDispatchCatalogItems_UnknownMemberNoExemption(t *testing.T) {
	srv := configOpnsServer(newConfigOpnsBrood(), &scopeTestConfigBrood{})
	w := httptest.NewRecorder()
	req := configOpnsRequest("/clusters/ghost/catalogitems/varroa-system/x")
	srv.dispatchCatalogItems(w, req, "ghost", []string{"varroa-system", "x"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("get on unknown member: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// configOpnsSearchConfigBrood answers one catalog item per namespace listed
// for each cluster. Every item name contains "team".
type configOpnsSearchConfigBrood struct {
	ConfigBrood
	namespaces map[string][]string
}

func (c *configOpnsSearchConfigBrood) ListCatalogItems(_ context.Context, cluster, _ string, _ CatalogItemFilter) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, ns := range c.namespaces[cluster] {
		raw, err := json.Marshal(bus.CatalogItemSummary{Name: "team-" + ns, Namespace: ns, Type: "pipeline"})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func TestSearch_CatalogItems_PerClusterOperatorNamespace(t *testing.T) {
	config := &configOpnsSearchConfigBrood{namespaces: map[string][]string{
		"core": {"varroa-system"},
		"edge": {"varroa-edge", "varroa-system"},
	}}
	srv := NewServer(&Dependencies{
		Logger:            slog.Default(),
		Authorizer:        configOpnsAuthorizer(),
		Brood:             newConfigOpnsBrood(),
		ConfigBrood:       config,
		OperatorNamespace: "varroa-system",
	})
	resp, err := srv.Search(context.Background(), scopedDevClaims, "team", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	got := map[string]bool{}
	for _, item := range resp.Items {
		if item.Type == "catalogitem" {
			got[item.Cluster+"/"+item.Namespace] = true
		}
	}
	want := []string{"core/varroa-system", "edge/varroa-edge"}
	if len(got) != len(want) {
		t.Fatalf("catalog items = %v, want %v", got, want)
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("catalog items = %v, missing %s", got, key)
		}
	}
}

func TestClusterOperatorNamespace(t *testing.T) {
	clusters := []ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core", OperatorNamespace: "ignored-local"}},
		{ClusterInfo: bus.ClusterInfo{Name: "edge", OperatorNamespace: "varroa-edge"}, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "bare"}, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "stale", OperatorNamespace: "varroa-stale"}, Healthy: false},
	}
	tests := []struct {
		name    string
		cluster string
		want    string
	}{
		{"local uses the BFF value", "core", "varroa-system"},
		{"member uses its heartbeat", "edge", "varroa-edge"},
		{"member with empty heartbeat grants nothing", "bare", ""},
		{"unhealthy member grants nothing", "stale", ""},
		{"unknown member grants nothing", "ghost", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clusterOperatorNamespace(clusters, tt.cluster, "core", "varroa-system"); got != tt.want {
				t.Fatalf("clusterOperatorNamespace(%q) = %q, want %q", tt.cluster, got, tt.want)
			}
		})
	}
}
