package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestResolveCluster pins the one cluster rule that the REST handlers and the
// MCP Controller tools share.
func TestResolveCluster(t *testing.T) {
	known := newStoreBrood(nil, "member-b")
	cases := []struct {
		name    string
		brood   Brood
		in      string
		want    string
		wantErr string
	}{
		{name: "no brood, empty name", in: "", wantErr: "brood not configured"},
		{name: "no brood, local name", in: "core", wantErr: "brood not configured"},
		{name: "empty name selects the local cluster", brood: known, in: "", want: "core"},
		{name: "local name", brood: known, in: "core", want: "core"},
		{name: "known member", brood: known, in: "member-b", want: "member-b"},
		{name: "unknown name", brood: known, in: "nowhere", wantErr: "unknown cluster: nowhere"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCluster(context.Background(), tc.brood, tc.in)
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("ResolveCluster(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("ResolveCluster(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			var unknown *UnknownClusterError
			if tc.brood == nil && !errors.Is(err, ErrBroodNotConfigured) {
				t.Errorf("error %v is not ErrBroodNotConfigured", err)
			}
			if tc.brood != nil && !errors.As(err, &unknown) {
				t.Errorf("error %v is not an UnknownClusterError", err)
			}
		})
	}
}

// clusterRouteDeps returns the dependencies of a cluster route test. The store
// holds the Controller ns/ctrl.
func clusterRouteDeps(t *testing.T) (*Dependencies, *crdstore.Fake, *recordingActivitySink) {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns"}})
	sink := &recordingActivitySink{}
	return &Dependencies{
		Store:             store,
		Client:            newFakeResourceClient(),
		Authorizer:        adminTestAuthorizer(),
		ActivityPublisher: sink,
		Reconciler:        fakeReconcilerAPI{},
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	}, store, sink
}

// serveClusterRoute sends one request with admin claims through the router.
func serveClusterRoute(deps *Dependencies, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(contextWithClaims(req.Context(), adminClaims))
	w := httptest.NewRecorder()
	NewRouter(deps).ServeHTTP(w, req)
	return w
}

// assertRefusal checks the status and the error text of a refused call, and
// that the call changed no Controller and published no event.
func assertRefusal(t *testing.T, w *httptest.ResponseRecorder, wantCode int, wantText string, store *crdstore.Fake, sink *recordingActivitySink) {
	t.Helper()
	if w.Code != wantCode {
		t.Fatalf("status = %d, want %d: %s", w.Code, wantCode, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != wantText {
		t.Fatalf("body = %s, want error %q", w.Body.String(), wantText)
	}
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "ctrl", "ns"); err != nil {
		t.Errorf("the refused call changed the local store: %v", err)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

// Without a brood, every cluster-routed REST call answers 503 and never reads
// or writes the local store in its place.
func TestClusterRoutes_NoBroodRefusesTheCall(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/controllers", ""},
		{http.MethodGet, "/api/v1/controllers?cluster=core", ""},
		{http.MethodGet, "/api/v1/fleet/plugins", ""},
		{http.MethodGet, "/api/v1/clusters/core/controllers", ""},
		{http.MethodGet, "/api/v1/clusters/core/controllers/ns/ctrl", ""},
		{http.MethodGet, "/api/v1/clusters/core/controllers/ns/ctrl/yaml", ""},
		{http.MethodDelete, "/api/v1/clusters/core/controllers/ns/ctrl", ""},
		{http.MethodPatch, "/api/v1/clusters/core/controllers/ns/ctrl", `{"spec":{"version":"2.555"}}`},
		{http.MethodPost, "/api/v1/clusters/core/controllers/ns", `{"metadata":{"name":"new"}}`},
		{http.MethodPost, "/api/v1/clusters/core/controllers/ns/preflight", `{"metadata":{"name":"new"}}`},
		{http.MethodPost, "/api/v1/clusters/core/controllers/ns/render", `{"metadata":{"name":"new"}}`},
		{http.MethodPost, "/api/v1/clusters/core/controllers/ns/ctrl/restart", ""},
		{http.MethodPost, "/api/v1/clusters/core/controllers/ns/ctrl/break-glass", `{"reason":"incident"}`},
		{http.MethodGet, "/api/v1/clusters/core/controllers/ns/ctrl/diff", ""},
		{http.MethodGet, "/api/v1/clusters/core/controllers/ns/ctrl/plugins", ""},
		{http.MethodGet, "/api/v1/clusters/core/namespaces/deployable", ""},
		{http.MethodPost, "/api/v1/clusters/member-b/drain", `{"confirm":"member-b"}`},
		{http.MethodGet, "/api/v1/clusters/core/composedbundles", ""},
		{http.MethodGet, "/api/v1/cells/usage", ""},
		{http.MethodGet, "/api/v1/cells/usage?cluster=core", ""},
		{http.MethodGet, "/api/v1/cells/ns/ctrl/usage", ""},
		{http.MethodGet, "/api/v1/cells/ns/ctrl/grants", ""},
		{http.MethodGet, "/api/v1/cellpools", ""},
		{http.MethodGet, "/api/v1/cellpools?cluster=core", ""},
		{http.MethodGet, "/api/v1/cellpools/p", ""},
		{http.MethodPatch, "/api/v1/cellpools/p", `{"spec":{"warm":1}}`},
		{http.MethodGet, "/api/v1/brood-operations", ""},
		{http.MethodGet, "/api/v1/brood-operations?cluster=core", ""},
		{http.MethodPost, "/api/v1/brood-operations", `{"spec":{"action":{"verb":"Restart"}}}`},
		{http.MethodPost, "/api/v1/brood-operations/preview", `{"spec":{"action":{"verb":"Restart"}}}`},
		{http.MethodGet, "/api/v1/search?q=ctrl", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			deps, store, sink := clusterRouteDeps(t)
			w := serveClusterRoute(deps, rt.method, rt.path, rt.body)
			assertRefusal(t, w, http.StatusServiceUnavailable, "brood not configured", store, sink)
		})
	}
}

// An unknown cluster answers 404 with the cluster name, before any brood call.
func TestClusterRoutes_UnknownClusterRefusesTheCall(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/controllers?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/fleet/plugins?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/clusters/nowhere/controllers", ""},
		{http.MethodGet, "/api/v1/clusters/nowhere/controllers/ns/ctrl", ""},
		{http.MethodDelete, "/api/v1/clusters/nowhere/controllers/ns/ctrl", ""},
		{http.MethodPatch, "/api/v1/clusters/nowhere/controllers/ns/ctrl", `{"spec":{"version":"2.555"}}`},
		{http.MethodPost, "/api/v1/clusters/nowhere/controllers/ns/ctrl/restart", ""},
		{http.MethodGet, "/api/v1/clusters/nowhere/namespaces/deployable", ""},
		{http.MethodGet, "/api/v1/cells/usage?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/cells/ns/ctrl/usage?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/cells/ns/ctrl/grants?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/cellpools?cluster=nowhere", ""},
		{http.MethodGet, "/api/v1/cellpools/p?cluster=nowhere", ""},
		{http.MethodPatch, "/api/v1/cellpools/p?cluster=nowhere", `{"spec":{"warm":1}}`},
		{http.MethodGet, "/api/v1/brood-operations?cluster=nowhere", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			deps, store, sink := clusterRouteDeps(t)
			brood := newStoreBrood(store)
			deps.Brood = brood
			w := serveClusterRoute(deps, rt.method, rt.path, rt.body)
			assertRefusal(t, w, http.StatusNotFound, "unknown cluster: nowhere", store, sink)
			if len(brood.calls) != 0 {
				t.Errorf("brood calls = %v, want none", brood.calls)
			}
		})
	}
}

// Without a brood, the version profile list answers 503 and reads no store.
func TestHandleVersionProfiles_NoBroodRefusesTheCall(t *testing.T) {
	deps, _, _ := clusterRouteDeps(t)
	w := httptest.NewRecorder()
	NewServer(deps).HandleVersionProfiles(w, httptest.NewRequest(http.MethodGet, "/version-profiles", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "brood not configured") {
		t.Fatalf("status = %d, body = %s; want 503 brood not configured", w.Code, w.Body.String())
	}
}

// Without a brood, Search returns ErrBroodNotConfigured for a query that reads
// a source. An empty query reads nothing, so it still answers no rows.
func TestSearch_NoBroodRefusesTheQuery(t *testing.T) {
	deps, _, _ := clusterRouteDeps(t)
	srv := NewServer(deps)
	if _, err := srv.Search(context.Background(), adminClaims, "ctrl", ""); !errors.Is(err, ErrBroodNotConfigured) {
		t.Fatalf("Search error = %v, want ErrBroodNotConfigured", err)
	}
	resp, err := srv.Search(context.Background(), adminClaims, "  ", "")
	if err != nil || len(resp.Items) != 0 {
		t.Fatalf("empty query = %+v, %v; want no rows and no error", resp, err)
	}
}

// Without an authorizer, the controller list answers no rows from any cluster.
func TestControllerList_NilAuthorizerAnswersNoRows(t *testing.T) {
	deps, store, _ := clusterRouteDeps(t)
	deps.Authorizer = nil
	deps.Brood = newStoreBrood(store)
	w := serveClusterRoute(deps, http.MethodGet, "/api/v1/controllers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 0 {
		t.Errorf("items = %v, want none without an authorizer", body.Items)
	}
}
