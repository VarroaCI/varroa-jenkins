package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// reqCtxKey marks the request context that the tests in this file send.
type reqCtxKey struct{}

// reqCtxStore records the context of every store call and delegates to the
// wrapped backend. It lets a test assert that a handler passes the request
// context to its Kubernetes reads and writes.
type reqCtxStore struct {
	crdstore.Backend
	mu   sync.Mutex
	ctxs []context.Context
}

func (s *reqCtxStore) record(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxs = append(s.ctxs, ctx)
}

// assertMarked fails when no store call ran, or when a call ran with a
// context that lacks the request marker.
func (s *reqCtxStore) assertMarked(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ctxs) == 0 {
		t.Fatal("no store call recorded")
	}
	for i, ctx := range s.ctxs {
		if ctx.Value(reqCtxKey{}) != "marker" {
			t.Errorf("store call %d ran with a context that is not the request context", i)
		}
	}
}

func (s *reqCtxStore) GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	s.record(ctx)
	return s.Backend.GetObject(ctx, gvr, namespace, name)
}

func (s *reqCtxStore) ListObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	s.record(ctx)
	return s.Backend.ListObjects(ctx, gvr, namespace, labelSelector)
}

func (s *reqCtxStore) CreateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.record(ctx)
	return s.Backend.CreateObject(ctx, gvr, namespace, obj)
}

func (s *reqCtxStore) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.record(ctx)
	return s.Backend.UpdateObject(ctx, gvr, namespace, obj)
}

func (s *reqCtxStore) UpdateObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.record(ctx)
	return s.Backend.UpdateObjectStatus(ctx, gvr, namespace, obj)
}

func (s *reqCtxStore) DeleteObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	s.record(ctx)
	return s.Backend.DeleteObject(ctx, gvr, namespace, name)
}

func (s *reqCtxStore) DeleteObjectIfUnchanged(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, uid, resourceVersion string) error {
	s.record(ctx)
	return s.Backend.DeleteObjectIfUnchanged(ctx, gvr, namespace, name, uid, resourceVersion)
}

func (s *reqCtxStore) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	s.record(ctx)
	return s.Backend.PatchObjectStatus(ctx, gvr, namespace, name, status)
}

func (s *reqCtxStore) PatchObjectMeta(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, meta map[string]any) error {
	s.record(ctx)
	return s.Backend.PatchObjectMeta(ctx, gvr, namespace, name, meta)
}

func (s *reqCtxStore) PatchObjectMetaAt(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, resourceVersion string, meta map[string]any) (string, error) {
	s.record(ctx)
	return s.Backend.PatchObjectMetaAt(ctx, gvr, namespace, name, resourceVersion, meta)
}

// reqCtxBrood reports a fixed local cluster and records the context of every
// Get, the remote read that controller detail and YAML make.
type reqCtxBrood struct {
	Brood
	cr   *v1alpha1.Controller
	mu   sync.Mutex
	gets []context.Context
}

func (b *reqCtxBrood) LocalCluster() string { return "core" }

func (b *reqCtxBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	b.mu.Lock()
	b.gets = append(b.gets, ctx)
	b.mu.Unlock()
	return b.cr, nil
}

// reqCtxRequest builds a request whose context carries the admin claims and
// the request marker.
func reqCtxRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := contextWithClaims(req.Context(), adminClaims)
	return req.WithContext(context.WithValue(ctx, reqCtxKey{}, "marker"))
}

func reqCtxServer(store *reqCtxStore) *Server {
	return NewServer(&Dependencies{
		Authorizer:        adminTestAuthorizer(),
		Store:             store,
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	})
}

func TestRequestHandlersPassRequestContext(t *testing.T) {
	cases := []struct {
		name   string
		seed   []any
		method string
		path   string
		body   string
		call   func(s *Server, w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "users list", method: http.MethodGet, path: "/users",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleUsers(w, r) },
		},
		{
			name: "users create", method: http.MethodPost, path: "/users",
			body: `{"username":"bob","password":"correct-horse"}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleUsers(w, r) },
		},
		{
			name: "user update", method: http.MethodPut, path: "/users/alice",
			seed: []any{&v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "test-ns"}}},
			body: `{"email":"a@example.com"}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleUpdateUser(w, r, "alice") },
		},
		{
			name: "user delete", method: http.MethodDelete, path: "/users/alice",
			seed: []any{&v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "test-ns"}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleDeleteUser(w, r) },
		},
		{
			name: "groups list", method: http.MethodGet, path: "/groups",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleGroups(w, r) },
		},
		{
			name: "groups create", method: http.MethodPost, path: "/groups",
			body: `{"name":"g2","members":[]}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleGroups(w, r) },
		},
		{
			name: "group delete", method: http.MethodDelete, path: "/groups/g1",
			seed: []any{&v1alpha1.Group{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "test-ns"}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleGroupDispatch(w, r) },
		},
		{
			name: "builtin roles", method: http.MethodGet, path: "/builtin-roles",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleBuiltinRoles(w, r) },
		},
		{
			name: "me", method: http.MethodGet, path: "/me",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleMe(w, r) },
		},
		{
			name: "preferences", method: http.MethodPut, path: "/me/preferences",
			body: `{"theme":"dark"}`,
			seed: []any{&v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "admin-user", Namespace: "test-ns"}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleUpdatePreferences(w, r) },
		},
		{
			name: "teams list", method: http.MethodGet, path: "/teams",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleTeams(w, r) },
		},
		{
			name: "teams create", method: http.MethodPost, path: "/teams",
			body: `{"name":"new-team","members":["alice"],"namespaces":["ns1"]}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleTeams(w, r) },
		},
		{
			name: "team get", method: http.MethodGet, path: "/teams/t1",
			seed: []any{&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "t1"}, Spec: v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleTeamDispatch(w, r) },
		},
		{
			name: "team update", method: http.MethodPut, path: "/teams/t1",
			seed: []any{&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "t1"}, Spec: v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}}}},
			body: `{"members":["alice"],"namespaces":["ns1"]}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleTeamDispatch(w, r) },
		},
		{
			name: "team delete", method: http.MethodDelete, path: "/teams/t1",
			seed: []any{&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "t1"}, Spec: v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleTeamDispatch(w, r) },
		},
		{
			name: "runners list", method: http.MethodGet, path: "/runners",
			seed: []any{&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}, Spec: v1alpha1.TeamSpec{Members: []string{"someone"}, Namespaces: []string{"payments"}}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleRunners(w, r) },
		},
		{
			name: "runner create", method: http.MethodPost, path: "/runners",
			seed: []any{&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}, Spec: v1alpha1.TeamSpec{Members: []string{"someone"}, Namespaces: []string{"payments"}}}},
			body: `{"name":"win-1","spec":{"team":"payments","launcher":"inbound","remoteFS":"/var/j"}}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleRunners(w, r) },
		},
		{
			name: "runner get", method: http.MethodGet, path: "/runners/r1",
			seed: []any{&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleRunnerDispatch(w, r) },
		},
		{
			name: "runner patch", method: http.MethodPatch, path: "/runners/r1",
			seed: []any{&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"}}},
			body: `{"spec":{"numExecutors":3}}`,
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleRunnerDispatch(w, r) },
		},
		{
			name: "runner delete", method: http.MethodDelete, path: "/runners/r1",
			seed: []any{&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"}}},
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.HandleRunnerDispatch(w, r) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := crdstore.NewFake()
			if len(tc.seed) > 0 {
				crdstore.MustSeed(fake, tc.seed...)
			}
			store := &reqCtxStore{Backend: fake}
			srv := reqCtxServer(store)
			w := httptest.NewRecorder()
			tc.call(srv, w, reqCtxRequest(tc.method, tc.path, tc.body))
			if w.Code >= 400 {
				t.Fatalf("status = %d, want < 400: %s", w.Code, w.Body.String())
			}
			store.assertMarked(t)
		})
	}
}

func TestRunnerDoorPassesRequestContext(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok"},
	})
	spy := &reqCtxStore{Backend: store}
	srv.deps.Store = spy

	w := httptest.NewRecorder()
	srv.HandleRunnerDoor(w, reqCtxRequest(http.MethodGet, "/runners/tok/login", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	spy.assertMarked(t)
}

func TestControllerDetailAndYAMLPassRequestContext(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Endpoint: "https://c1.example.test"},
	}
	brood := &reqCtxBrood{Brood: newFakeBrood(newFakeResourceClient()), cr: cr}
	srv := NewServer(&Dependencies{
		Authorizer:        adminTestAuthorizer(),
		Store:             crdstore.NewFake(),
		Brood:             brood,
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	})

	w := httptest.NewRecorder()
	srv.handleControllerDetail(w, reqCtxRequest(http.MethodGet, "/", ""), "remote-x", "ns1", "c1")
	w = httptest.NewRecorder()
	srv.handleControllerYAML(w, reqCtxRequest(http.MethodGet, "/", ""), "remote-x", "ns1", "c1")

	brood.mu.Lock()
	defer brood.mu.Unlock()
	if len(brood.gets) != 2 {
		t.Fatalf("Brood.Get calls = %d, want 2", len(brood.gets))
	}
	for i, ctx := range brood.gets {
		if ctx.Value(reqCtxKey{}) != "marker" {
			t.Errorf("Brood.Get call %d ran with a context that is not the request context", i)
		}
	}
}

// reqCtxDefaults returns the varroa-defaults object that gives controllers a
// root-domain endpoint.
func reqCtxDefaults() *v1alpha1.ProvisioningDefaults {
	return &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: "example.test"},
	}
}

// reqCtxController returns a non-headless controller with no host and no
// Status.Endpoint, so its endpoint comes from the root domain.
func reqCtxController(name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}}
}

// defaultsGetCounter counts the reads of varroa-defaults.
type defaultsGetCounter struct {
	crdstore.Backend
	mu sync.Mutex
	n  int
}

func (b *defaultsGetCounter) GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "provisioningdefaults" {
		b.mu.Lock()
		b.n++
		b.mu.Unlock()
	}
	return b.Backend.GetObject(ctx, gvr, namespace, name)
}

func TestControllerEndpointCallersPassRequestContext(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		fake := crdstore.NewFake()
		crdstore.MustSeed(fake, reqCtxDefaults(), reqCtxController("c1"))
		store := &reqCtxStore{Backend: fake}
		srv := reqCtxServer(store)
		w := httptest.NewRecorder()
		srv.HandleControllers(w, reqCtxRequest(http.MethodGet, "/controllers", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "https://c1.example.test") {
			t.Errorf("body lacks the root-domain endpoint: %s", w.Body.String())
		}
		store.assertMarked(t)
	})

	t.Run("detail", func(t *testing.T) {
		fake := crdstore.NewFake()
		crdstore.MustSeed(fake, reqCtxDefaults(), reqCtxController("c1"))
		store := &reqCtxStore{Backend: fake}
		srv := reqCtxServer(store)
		w := httptest.NewRecorder()
		srv.handleControllerDetail(w, reqCtxRequest(http.MethodGet, "/", ""), "", "ns1", "c1")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "https://c1.example.test") {
			t.Errorf("body lacks the root-domain endpoint: %s", w.Body.String())
		}
		store.assertMarked(t)
	})

	t.Run("break-glass", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		crdstore.MustSeed(client.crdStore, reqCtxDefaults(), &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "reachable", Namespace: "team-a", UID: "54c62aff-0000-0000-0000-000000000001"},
		})
		spy := &reqCtxStore{Backend: srv.deps.Store}
		srv.deps.Store = spy
		w := httptest.NewRecorder()
		srv.handleBreakGlass(w, reqCtxRequest(http.MethodPost, "/", `{"reason":"incident"}`), "core", "team-a", "reachable")
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "https://reachable.example.test") {
			t.Errorf("body lacks the root-domain endpoint: %s", w.Body.String())
		}
		spy.assertMarked(t)
	})
}

func TestControllerListReadsProvisioningDefaultsOnce(t *testing.T) {
	t.Run("no host on any row", func(t *testing.T) {
		fake := crdstore.NewFake()
		crdstore.MustSeed(fake, reqCtxDefaults(), reqCtxController("c1"), reqCtxController("c2"), reqCtxController("c3"))
		counter := &defaultsGetCounter{Backend: fake}
		srv := reqCtxServer(&reqCtxStore{Backend: counter})
		w := httptest.NewRecorder()
		srv.HandleControllers(w, reqCtxRequest(http.MethodGet, "/controllers", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if counter.n != 1 {
			t.Errorf("varroa-defaults reads = %d, want 1", counter.n)
		}
	})

	t.Run("every row has a host", func(t *testing.T) {
		fake := crdstore.NewFake()
		hosted := func(name string) *v1alpha1.Controller {
			cr := reqCtxController(name)
			cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: name + ".example.test"}
			return cr
		}
		crdstore.MustSeed(fake, reqCtxDefaults(), hosted("c1"), hosted("c2"), hosted("c3"))
		counter := &defaultsGetCounter{Backend: fake}
		srv := reqCtxServer(&reqCtxStore{Backend: counter})
		w := httptest.NewRecorder()
		srv.HandleControllers(w, reqCtxRequest(http.MethodGet, "/controllers", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if counter.n != 0 {
			t.Errorf("varroa-defaults reads = %d, want 0", counter.n)
		}
	})
}

func TestControllerListReadsMissingDefaultsOnce(t *testing.T) {
	fake := crdstore.NewFake()
	crdstore.MustSeed(fake, reqCtxController("c1"), reqCtxController("c2"), reqCtxController("c3"))
	counter := &defaultsGetCounter{Backend: fake}
	srv := reqCtxServer(&reqCtxStore{Backend: counter})
	w := httptest.NewRecorder()
	srv.HandleControllers(w, reqCtxRequest(http.MethodGet, "/controllers", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if counter.n != 1 {
		t.Errorf("varroa-defaults reads = %d, want 1 when the object is absent", counter.n)
	}
}
