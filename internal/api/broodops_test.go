package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

func init() { _ = v1alpha1.AddToScheme(scheme.Scheme) }

func testScheme() *runtime.Scheme { return scheme.Scheme }

// TestBroodHandlers_MethodNotAllowed asserts PUT on list returns 405.
func TestBroodHandlers_MethodNotAllowed(t *testing.T) {
	srv := newBroodTestServer()
	req := httptest.NewRequest(http.MethodPut, "/brood-operations", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{}))
	w := httptest.NewRecorder()
	srv.handleBroodOperations(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for PUT on list, got %d", w.Code)
	}
}

// TestBroodHandlers_403RoleMatrix asserts role gating for each endpoint.
func TestBroodHandlers_403RoleMatrix(t *testing.T) {
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}
	adminBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}},
			RoleRef:  "admin",
		},
	}
	viewerRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}},
	}
	viewerBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
			RoleRef:  "viewer",
		},
	}
	authz := NewAuthorizer(rbac.NewTestResolverWithRoles(
		[]*v1alpha1.VarroaRole{adminRole, viewerRole},
		[]*v1alpha1.VarroaRoleBinding{adminBinding, viewerBinding},
	), false)

	adminClaims := &auth.Claims{Subject: "admin", Groups: []string{"admins"}}
	viewerClaims := &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}

	client := &minimalBroodClient{}
	crClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	brood := &stubBrood{localCluster: "core"}
	broodOps := &stubBroodOps{}

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		claims *auth.Claims
		want   int
	}{
		{"create-admin", http.MethodPost, "/brood-operations", `{"namespace":"team-ns","spec":{"action":{"verb":"reconcile"},"targets":{"names":["ctrl-a"]}}}`, adminClaims, http.StatusCreated},
		{"create-viewer-403", http.MethodPost, "/brood-operations", `{"namespace":"team-ns","spec":{"action":{"verb":"reconcile"},"targets":{"names":["ctrl-a"]}}}`, viewerClaims, http.StatusForbidden},
		{"create-execgroovy-admin", http.MethodPost, "/brood-operations", `{"namespace":"team-ns","spec":{"action":{"verb":"executeGroovy","groovy":{"script":"println(\"ok\")"}},"targets":{"names":["ctrl-a"]}}}`, adminClaims, http.StatusCreated},
		{"create-execgroovy-viewer-403", http.MethodPost, "/brood-operations", `{"namespace":"team-ns","spec":{"action":{"verb":"executeGroovy","groovy":{"script":"println(\"ok\")"}},"targets":{"names":["ctrl-a"]}}}`, viewerClaims, http.StatusForbidden},
		{"create-execgroovy-admin-operator-ns", http.MethodPost, "/brood-operations", `{"namespace":"operator-ns","spec":{"action":{"verb":"executeGroovy","groovy":{"script":"println(\"ok\")"}},"targets":{"names":["ns-a/ctrl-a"]}}}`, adminClaims, http.StatusCreated},
		{"create-execgroovy-viewer-operator-ns-403", http.MethodPost, "/brood-operations", `{"namespace":"operator-ns","spec":{"action":{"verb":"executeGroovy","groovy":{"script":"println(\"ok\")"}},"targets":{"names":["ctrl-a"]}}}`, viewerClaims, http.StatusForbidden},
		{"delete-admin", http.MethodDelete, "/brood-operations/ns/op", "", adminClaims, http.StatusNotFound},
		{"delete-viewer-403", http.MethodDelete, "/brood-operations/ns/op", "", viewerClaims, http.StatusForbidden},
		{"suspend-admin", http.MethodPost, "/brood-operations/ns/op/suspend", `{"suspend":true}`, adminClaims, http.StatusNotFound},
		{"suspend-viewer-403", http.MethodPost, "/brood-operations/ns/op/suspend", `{"suspend":true}`, viewerClaims, http.StatusForbidden},
		{"list-viewer", http.MethodGet, "/brood-operations", "", viewerClaims, http.StatusOK},
		{"detail-viewer-404", http.MethodGet, "/brood-operations/ns/missing", "", viewerClaims, http.StatusNotFound},
		{"preview-viewer", http.MethodPost, "/brood-operations/preview", `{"namespace":"team-ns","spec":{"action":{"verb":"reconcile"},"targets":{"selector":{}}}}`, viewerClaims, http.StatusOK},
		{"preview-viewer-operator-ns-403", http.MethodPost, "/brood-operations/preview", `{"spec":{"action":{"verb":"reconcile"},"targets":{"selector":{}}}}`, viewerClaims, http.StatusForbidden},
		{"detail-viewer-operator-ns-403", http.MethodGet, "/brood-operations/operator-ns/op", "", viewerClaims, http.StatusForbidden},
		{"stream-no-claims-401", http.MethodGet, "/brood-operations/ns/op/stream", "", nil, http.StatusUnauthorized},
		{"stream-viewer-404", http.MethodGet, "/brood-operations/ns/missing/stream", "", viewerClaims, http.StatusNotFound},
		{"stream-viewer-operator-ns-403", http.MethodGet, "/brood-operations/operator-ns/op/stream", "", viewerClaims, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(&Dependencies{
				Client:            client,
				Store:             storeFromMinimal(client),
				CRClient:          crClient,
				Authorizer:        authz,
				Logger:            slog.Default(),
				OperatorNamespace: "operator-ns",
				Brood:             brood,
				BroodOps:          broodOps,
			})
			var bodyReader io.Reader
			if tt.body == "" {
				bodyReader = http.NoBody
			} else {
				bodyReader = strings.NewReader(tt.body)
			}
			req := httptest.NewRequest(tt.method, tt.path, bodyReader)
			if tt.claims != nil {
				req = req.WithContext(auth.ContextWithClaims(req.Context(), tt.claims))
			}
			w := httptest.NewRecorder()
			srv.handleBroodOperations(w, req)
			if w.Code != tt.want {
				t.Errorf("expected status %d, got %d; body: %s", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

// A cluster/ns/name entry that names an unknown cluster answers 400 before
// any create or preview call reaches the store.
func TestBroodHandlers_UnknownQualifierClusterRefused(t *testing.T) {
	body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"names":["core/ns/ctrl-a","nope/ns/ctrl-b"]}}}`
	for _, path := range []string{"/brood-operations", "/brood-operations/preview"} {
		t.Run(path, func(t *testing.T) {
			ops := &countingBroodOps{}
			srv := newBroodTestServer()
			srv.deps.BroodOps = ops
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
			w := httptest.NewRecorder()
			srv.handleBroodOperations(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] != `unknown cluster "nope"` {
				t.Errorf("body = %s, want error %q", w.Body.String(), `unknown cluster "nope"`)
			}
			if ops.creates != 0 || ops.previews != 0 {
				t.Errorf("creates = %d, previews = %d, want none", ops.creates, ops.previews)
			}
		})
	}
}

// Without a brood operations store, every brood operation route answers 503
// after its access check.
func TestBroodHandlers_NoBroodOpsStore(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/brood-operations", ""},
		{http.MethodPost, "/brood-operations", `{"namespace":"ns","spec":{"action":{"verb":"reconcile"},"targets":{"names":["ctrl-a"]}}}`},
		{http.MethodPost, "/brood-operations/preview", `{"namespace":"ns","spec":{"action":{"verb":"reconcile"},"targets":{"selector":{}}}}`},
		{http.MethodGet, "/brood-operations/ns/op", ""},
		{http.MethodDelete, "/brood-operations/ns/op", ""},
		{http.MethodPost, "/brood-operations/ns/op/suspend", `{"suspend":true}`},
		{http.MethodGet, "/brood-operations/ns/op/stream", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			srv := newBroodTestServer()
			srv.deps.BroodOps = nil
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
			w := httptest.NewRecorder()
			srv.handleBroodOperations(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] != "brood operations not configured" {
				t.Errorf("body = %s, want error %q", w.Body.String(), "brood operations not configured")
			}
		})
	}

	t.Run("refused caller gets 403", func(t *testing.T) {
		srv := newBroodTestServer()
		srv.deps.BroodOps = nil
		req := httptest.NewRequest(http.MethodDelete, "/brood-operations/ns/op", nil)
		req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
		w := httptest.NewRecorder()
		srv.handleBroodOperations(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
		}
	})
}

// postBroodAsAdmin posts body to path on srv with admin claims.
func postBroodAsAdmin(srv *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodOperations(w, req)
	return w
}

// broodWithMembers returns a brood with the local cluster core and two
// members. member-b is healthy and member-c is not.
func broodWithMembers() *stubBrood {
	return &stubBrood{localCluster: "core", members: []ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "member-c"}},
		{ClusterInfo: bus.ClusterInfo{Name: "member-b"}, Healthy: true},
	}}
}

// broodClusterRow is the cluster, ok, and error of one response row.
type broodClusterRow struct {
	Cluster string `json:"cluster"`
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
}

// broodClusterRows decodes the clusters rows of a create or preview response.
func broodClusterRows(t *testing.T, w *httptest.ResponseRecorder) []broodClusterRow {
	t.Helper()
	var resp struct {
		Clusters []broodClusterRow `json:"clusters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return resp.Clusters
}

// ["all"] expands to the local cluster and every member, healthy or not.
// Create and preview fan out to the local cluster first.
func TestBroodHandlers_ClustersAllExpands(t *testing.T) {
	body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"selector":{},"namespaces":["all"]}},"clusters":["all"]}`
	want := []string{"core", "member-b", "member-c"}
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/brood-operations", http.StatusCreated},
		{"/brood-operations/preview", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ops := &countingBroodOps{}
			srv := newBroodTestServer()
			srv.deps.Brood = broodWithMembers()
			srv.deps.BroodOps = ops
			w := postBroodAsAdmin(srv, tc.path, body)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if !reflect.DeepEqual(ops.clusters, want) {
				t.Errorf("fan-out clusters = %v, want %v", ops.clusters, want)
			}
			var got []string
			for _, row := range broodClusterRows(t, w) {
				got = append(got, row.Cluster)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("response clusters = %v, want %v", got, want)
			}
		})
	}
}

// An unreachable cluster gets the same response row and status in an ["all"]
// create as in the explicit clusters list of the same brood.
func TestBroodHandlers_ClustersAllUnreachableMember(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unreachable []string
		code        int
	}{
		{"one member unreachable", []string{"member-c"}, http.StatusCreated},
		{"every cluster unreachable", []string{"core", "member-b", "member-c"}, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forms := []string{`["all"]`, `["core","member-b","member-c"]`}
			rows := make([][]broodClusterRow, 0, len(forms))
			for _, clusters := range forms {
				ops := &countingBroodOps{unreachable: map[string]bool{}}
				for _, c := range tc.unreachable {
					ops.unreachable[c] = true
				}
				srv := newBroodTestServer()
				srv.deps.Brood = broodWithMembers()
				srv.deps.BroodOps = ops
				body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"selector":{},"namespaces":["all"]}},"clusters":` + clusters + `}`
				w := postBroodAsAdmin(srv, "/brood-operations", body)
				if w.Code != tc.code {
					t.Fatalf("clusters %s: status = %d, want %d: %s", clusters, w.Code, tc.code, w.Body.String())
				}
				rows = append(rows, broodClusterRows(t, w))
			}
			if !reflect.DeepEqual(rows[0], rows[1]) {
				t.Errorf("all rows = %+v, explicit rows = %+v", rows[0], rows[1])
			}
			for _, row := range rows[0] {
				if row.Cluster == "member-c" && (row.OK || !strings.Contains(row.Error, "unreachable")) {
					t.Errorf("member-c row = %+v, want ok false and an unreachable error", row)
				}
			}
		})
	}
}

// A failed cluster list read for ["all"] answers 503 before any fan-out.
func TestBroodHandlers_ClustersAllListError(t *testing.T) {
	body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"selector":{},"namespaces":["all"]}},"clusters":["all"]}`
	for _, path := range []string{"/brood-operations", "/brood-operations/preview"} {
		t.Run(path, func(t *testing.T) {
			ops := &countingBroodOps{}
			srv := newBroodTestServer()
			srv.deps.Brood = &stubBrood{localCluster: "core", clustersErr: errors.New("list clusters: directory down")}
			srv.deps.BroodOps = ops
			w := postBroodAsAdmin(srv, path, body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] != "list clusters: directory down" {
				t.Errorf("body = %s, want error %q", w.Body.String(), "list clusters: directory down")
			}
			if ops.creates != 0 || ops.previews != 0 {
				t.Errorf("creates = %d, previews = %d, want none", ops.creates, ops.previews)
			}
		})
	}
}

// An empty clusters array with cluster/ns/name entries partitions like an
// absent clusters field.
func TestBroodHandlers_EmptyClustersWithQualifiedNames(t *testing.T) {
	body := `{"spec":{"action":{"verb":"reconcile"},"targets":{"names":["member-b/ns/ctrl-b","core/ns/ctrl-a"]}},"clusters":[]}`
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/brood-operations", http.StatusCreated},
		{"/brood-operations/preview", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ops := &countingBroodOps{}
			srv := newBroodTestServer()
			srv.deps.Brood = broodWithMembers()
			srv.deps.BroodOps = ops
			w := postBroodAsAdmin(srv, tc.path, body)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if want := []string{"core", "member-b"}; !reflect.DeepEqual(ops.clusters, want) {
				t.Errorf("fan-out clusters = %v, want %v", ops.clusters, want)
			}
		})
	}
}

// The local executeGroovy policy applies only when the partitioned clusters
// hold the local cluster alone. A grammar refusal comes before it.
func TestBroodHandlers_ExecuteGroovyPolicyLocalOnly(t *testing.T) {
	const denied = "executeGroovy is disabled by ProvisioningDefaults.broodPolicy"
	for _, tc := range []struct {
		name     string
		targets  string
		clusters string
		members  bool
		code     int
		wantErr  string
	}{
		{"local name", `{"names":["ns/ctrl-a"]}`, "", true, http.StatusForbidden, denied},
		{"explicit local cluster", `{"names":["ns/ctrl-a"]}`, `["core"]`, true, http.StatusForbidden, denied},
		{"local qualifier", `{"names":["core/ns/ctrl-a"]}`, "", true, http.StatusForbidden, denied},
		{"member qualifier", `{"names":["member-b/ns/ctrl-b"]}`, "", true, http.StatusCreated, ""},
		{"local and member qualifiers", `{"names":["core/ns/ctrl-a","member-b/ns/ctrl-b"]}`, "", true, http.StatusCreated, ""},
		{"explicit member cluster", `{"names":["ns/ctrl-b"]}`, `["member-b"]`, true, http.StatusCreated, ""},
		{"all with members", `{"selector":{},"namespaces":["all"]}`, `["all"]`, true, http.StatusCreated, ""},
		{"all without members", `{"selector":{},"namespaces":["all"]}`, `["all"]`, false, http.StatusForbidden, denied},
		{"unknown qualifier before policy", `{"names":["core/ns/ctrl-a","nope/ns/ctrl-b"]}`, "", true, http.StatusBadRequest, `unknown cluster "nope"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := crdstore.NewFake()
			disabled := false
			crdstore.MustSeed(st, &v1alpha1.ProvisioningDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: provisioningDefaultsName},
				Spec: v1alpha1.ProvisioningDefaultsSpec{BroodPolicy: &v1alpha1.BroodPolicy{
					ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{Enabled: &disabled},
				}},
			})
			ops := &countingBroodOps{}
			srv := newBroodTestServer()
			srv.deps.Store = st
			srv.deps.BroodOps = ops
			if tc.members {
				srv.deps.Brood = broodWithMembers()
			}
			body := `{"spec":{"action":{"verb":"executeGroovy","groovy":{"script":"println(1)"}},"targets":` + tc.targets + `}`
			if tc.clusters != "" {
				body += `,"clusters":` + tc.clusters
			}
			body += `}`
			w := postBroodAsAdmin(srv, "/brood-operations", body)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if tc.wantErr != "" {
				var resp map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] != tc.wantErr {
					t.Errorf("body = %s, want error %q", w.Body.String(), tc.wantErr)
				}
				if ops.creates != 0 {
					t.Errorf("creates = %d, want none", ops.creates)
				}
			}
		})
	}
}

// TestBroodHandlers_PreviewOK verifies the preview endpoint works for viewers.
func TestBroodHandlers_PreviewOK(t *testing.T) {
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}
	ctrl.APIVersion = "varroa.dev/v1alpha1"
	ctrl.Kind = "Controller"
	crClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(ctrl).Build()

	srv := newBroodTestServer()
	srv.deps.CRClient = crClient
	srv.deps.Authorizer = newBroodTestAuthorizer()

	body := `{"namespace":"ns","spec":{"action":{"verb":"reconcile"},"targets":{"names":["ctrl-a"]}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-operations/preview", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodOperations(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for preview, got %d: %s", w.Code, w.Body.String())
		return
	}
	var resp broodPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Clusters) != 1 || !resp.Clusters[0].OK {
		t.Errorf("expected 1 ok cluster section, got: %+v", resp.Clusters)
	}
}

// TestBroodHandlers_StreamSSE tests the SSE per-run stream.
func TestBroodHandlers_StreamSSE(t *testing.T) {
	crClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	client := &broodStreamClient{}

	authz := NewAuthorizer(rbac.NewTestResolverWithRoles(
		[]*v1alpha1.VarroaRole{{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"read"}}}}}},
		[]*v1alpha1.VarroaRoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "vb"}, Spec: v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "v"}}, RoleRef: "viewer"}}},
	), false)

	brood := &stubBrood{localCluster: "core"}
	broodOps := &stubBroodOps{
		getChildren: []ClusterBroodOp{
			{
				Cluster: "core",
				Op: &v1alpha1.BroodOperation{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "test-op",
						Namespace:       "ns",
						ResourceVersion: "100",
					},
					Spec: v1alpha1.BroodOperationSpec{
						Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
					},
					Status: v1alpha1.BroodOperationStatus{
						Phase:   v1alpha1.BroodOperationPhaseSucceeded,
						Summary: v1alpha1.BroodSummary{Total: 1, Succeeded: 1},
					},
				},
			},
		},
	}

	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromStream(client),
		CRClient:          crClient,
		Authorizer:        authz,
		Logger:            slog.Default(),
		OperatorNamespace: "operator-ns",
		Brood:             brood,
		BroodOps:          broodOps,
	})

	req := httptest.NewRequest(http.MethodGet, "/brood-operations/ns/test-op/stream", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "v", Groups: []string{"v"}}))
	w := httptest.NewRecorder()

	srv.handleBroodStreamWithPoll(w, req, "ns", "test-op", 10*time.Millisecond, time.Minute)

	scanner := bufio.NewScanner(w.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}

	hasStatus := false
	hasClosed := false
	for _, e := range events {
		if e == "status" {
			hasStatus = true
		}
		if e == "closed" {
			hasClosed = true
		}
	}
	if !hasStatus {
		t.Error("expected at least one 'status' SSE event")
	}
	if !hasClosed {
		t.Error("expected a 'closed' SSE event for terminal phase")
	}
}

// newBroodStreamTestServer builds a minimal *Server wired for the SSE stream
// handler tests below, backed by the given BroodOps stub.
func newBroodStreamTestServer(broodOps BroodOps) *Server {
	crClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	client := &broodStreamClient{}
	authz := NewAuthorizer(rbac.NewTestResolverWithRoles(
		[]*v1alpha1.VarroaRole{{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"read"}}}}}},
		[]*v1alpha1.VarroaRoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "vb"}, Spec: v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "v"}}, RoleRef: "viewer"}}},
	), false)
	brood := &stubBrood{localCluster: "core"}
	return NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromStream(client),
		CRClient:          crClient,
		Authorizer:        authz,
		Logger:            slog.Default(),
		OperatorNamespace: "operator-ns",
		Brood:             brood,
		BroodOps:          broodOps,
	})
}

// sseEvents parses "event: "-prefixed lines out of a raw SSE body.
func sseEvents(body *bytes.Buffer) []string {
	scanner := bufio.NewScanner(body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	return events
}

// TestBroodHandlers_StreamSSE_TerminalDespiteEmptyReachableSet covers
// an operation already terminal at connect time, whose CR then disappears
// from the fan-out (e.g. garbage-collected, or its cluster becomes
// unreachable) on every subsequent poll, must still close the stream — not
// hang waiting for a reachable target set that will never reappear.
func TestBroodHandlers_StreamSSE_TerminalDespiteEmptyReachableSet(t *testing.T) {
	var callCount int32
	broodOps := &stubBroodOps{
		getFunc: func() []ClusterBroodOp {
			n := atomic.AddInt32(&callCount, 1)
			if n == 1 {
				return []ClusterBroodOp{
					{
						Cluster: "core",
						Op: &v1alpha1.BroodOperation{
							ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns", ResourceVersion: "1"},
							Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile}},
							Status: v1alpha1.BroodOperationStatus{
								Phase:   v1alpha1.BroodOperationPhaseSucceeded,
								Summary: v1alpha1.BroodSummary{Total: 1, Succeeded: 1},
							},
						},
					},
				}
			}
			// Every call after the first simulates the target set becoming
			// unreachable/empty (e.g. the CR was GC'd once terminal). If the
			// handler only detected terminal phase from inside the poll
			// loop, it would never observe it again and would hang.
			return nil
		},
	}
	srv := newBroodStreamTestServer(broodOps)

	req := httptest.NewRequest(http.MethodGet, "/brood-operations/ns/test-op/stream", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "v", Groups: []string{"v"}}))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		// pollInterval is short and maxDuration generous relative to the
		// test's own wait bound below, so a pass here proves the terminal
		// check — not the deadline backstop — is what closed the stream.
		srv.handleBroodStreamWithPoll(w, req, "ns", "test-op", 5*time.Millisecond, time.Minute)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close on an already-terminal operation with an empty reachable set")
	}

	events := sseEvents(w.Body)
	var hasClosed bool
	for _, e := range events {
		if e == "closed" {
			hasClosed = true
		}
	}
	if !hasClosed {
		t.Errorf("expected a 'closed' SSE event, got events: %v", events)
	}
	if strings.Contains(w.Body.String(), "deadline_exceeded") {
		t.Error("stream closed via the deadline backstop, not the terminal-phase check")
	}
}

// TestBroodHandlers_StreamSSE_DeadlineExpiry covers an operation that
// never reaches a terminal phase because its target set is empty on every
// poll (all target clusters unreachable) must still close after the
// server-side deadline, with a final SSE event carrying an informative
// reason/message instead of hanging forever.
func TestBroodHandlers_StreamSSE_DeadlineExpiry(t *testing.T) {
	var callCount int32
	broodOps := &stubBroodOps{
		getFunc: func() []ClusterBroodOp {
			n := atomic.AddInt32(&callCount, 1)
			if n == 1 {
				// Reachable and non-terminal at connect time, so the
				// pre-loop terminal check does not fire immediately.
				return []ClusterBroodOp{
					{
						Cluster: "core",
						Op: &v1alpha1.BroodOperation{
							ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns", ResourceVersion: "1"},
							Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile}},
							Status: v1alpha1.BroodOperationStatus{
								Phase:   v1alpha1.BroodOperationPhaseRunning,
								Summary: v1alpha1.BroodSummary{Total: 1},
							},
						},
					},
				}
			}
			// Empty reachable set forever after: the deadline is the only
			// thing that can end this stream.
			return nil
		},
	}
	srv := newBroodStreamTestServer(broodOps)

	req := httptest.NewRequest(http.MethodGet, "/brood-operations/ns/test-op/stream", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "v", Groups: []string{"v"}}))
	w := httptest.NewRecorder()

	maxDuration := 30 * time.Millisecond
	done := make(chan struct{})
	go func() {
		srv.handleBroodStreamWithPoll(w, req, "ns", "test-op", 5*time.Millisecond, maxDuration)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close on deadline expiry with an empty reachable set")
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: closed") {
		t.Errorf("expected a 'closed' SSE event, got body: %s", body)
	}
	if !strings.Contains(body, "deadline_exceeded") {
		t.Errorf("expected the closed event to report deadline_exceeded, got body: %s", body)
	}
	// Deliberately no assertion on callCount: the deadline timer and poll
	// ticker are both started immediately before the select loop, so under
	// scheduler contention (GC pause, race-detector overhead, a starved
	// CI runner) both can already be ready by the time the goroutine is
	// scheduled — select picks pseudo-randomly among ready cases, so the
	// deadline case can win before a single poll tick is ever consumed.
	// The deadline_exceeded payload assertions above already prove the
	// behavior this test exists to cover; a poll-count floor would make
	// the test flaky without adding coverage.
}

// TestValidStreamScope verifies the validStreamScope function accepts broodop scopes.
func TestValidStreamScope(t *testing.T) {
	tests := []struct {
		scope string
		valid bool
	}{
		{"brood", true},
		{"activity", true},
		{"controller:core/ns/name", true},
		{"broodop:a/b", true},
		{"broodop:ns/name", true},
		{"", false},
		{"broodop:", false},
		{"broodop:onlyns", false},
		{"controller:", false},
		{"controller:ns/name", false},
		{"unknown", false},
	}
	for _, tt := range tests {
		got := validStreamScope(tt.scope)
		if got != tt.valid {
			t.Errorf("validStreamScope(%q) = %v, want %v", tt.scope, got, tt.valid)
		}
	}
}

// --- Stubs ---

// stubBrood implements Brood for tests.
// stubBrood knows its local cluster, dev-cluster, and each entry of members.
// Clusters lists the local cluster and members, or fails with clustersErr.
type stubBrood struct {
	localCluster string
	members      []ClusterInfo
	clustersErr  error
}

func (s *stubBrood) LocalCluster() string { return s.localCluster }
func (s *stubBrood) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	if s.clustersErr != nil {
		return nil, s.clustersErr
	}
	self := ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: s.localCluster}, Core: true, Healthy: true}
	return append([]ClusterInfo{self}, s.members...), nil
}
func (s *stubBrood) IsKnown(ctx context.Context, cluster string) bool {
	for _, m := range s.members {
		if m.Name == cluster {
			return true
		}
	}
	return cluster == s.localCluster || cluster == "dev-cluster"
}
func (s *stubBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return nil, nil, nil
}
func (s *stubBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	return nil, nil
}
func (s *stubBrood) Create(ctx context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	return nil, nil, nil
}
func (s *stubBrood) Preflight(ctx context.Context, cluster string, req ControllersCreateArgs) ([]bus.Check, error) {
	return nil, nil
}
func (s *stubBrood) Update(ctx context.Context, cluster, ns, name string, patch json.RawMessage, fieldManager string, force bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	return nil, nil, nil, nil
}
func (s *stubBrood) Delete(ctx context.Context, cluster, ns, name string) error    { return nil }
func (s *stubBrood) DeletePod(ctx context.Context, cluster, ns, name string) error { return nil }
func (s *stubBrood) Drain(ctx context.Context, cluster, requestedBy string) (string, error) {
	return "", nil
}
func (s *stubBrood) DrainCancel(ctx context.Context, cluster, requestedBy string) (string, error) {
	return "", nil
}
func (s *stubBrood) StateOf(ctx context.Context, cluster string) (string, error) { return "", nil }
func (s *stubBrood) DiscoverNamespaces(ctx context.Context, cluster string) (*bus.NamespacesListResponse, error) {
	return &bus.NamespacesListResponse{}, nil
}

// stubBroodOps implements BroodOps for tests.
type stubBroodOps struct {
	getChildren []ClusterBroodOp
	// getFunc, when set, takes precedence over getChildren and is invoked
	// on every Get call — used to simulate a fan-out result that changes
	// across polls (e.g. a target set that goes from reachable to
	// unreachable, or an already-terminal op whose CR later disappears).
	getFunc func() []ClusterBroodOp
}

func (s *stubBroodOps) Create(ctx context.Context, clusters []string, ns, name string, specs map[string]v1alpha1.BroodOperationSpec, startedBy string) []ClusterCreateResult {
	results := make([]ClusterCreateResult, len(clusters))
	for i, c := range clusters {
		results[i] = ClusterCreateResult{Cluster: c, OK: true, Op: &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}}
	}
	return results
}
func (s *stubBroodOps) List(ctx context.Context, ns, clusterFilter string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	return nil, []ClusterFanoutStatus{{Name: "core", OK: true}}, nil
}
func (s *stubBroodOps) Get(ctx context.Context, ns, name string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	if s.getFunc != nil {
		return s.getFunc(), []ClusterFanoutStatus{{Name: "core", OK: true}}, nil
	}
	if s.getChildren != nil {
		return s.getChildren, []ClusterFanoutStatus{{Name: "core", OK: true}}, nil
	}
	return nil, []ClusterFanoutStatus{{Name: "core", OK: true}}, nil
}
func (s *stubBroodOps) Cancel(ctx context.Context, clusters []string, ns, name string) []ClusterActionResult {
	results := make([]ClusterActionResult, len(clusters))
	for i, c := range clusters {
		results[i] = ClusterActionResult{Cluster: c, OK: true}
	}
	return results
}
func (s *stubBroodOps) Suspend(ctx context.Context, clusters []string, ns, name string, suspend bool) []ClusterActionResult {
	results := make([]ClusterActionResult, len(clusters))
	for i, c := range clusters {
		results[i] = ClusterActionResult{Cluster: c, OK: true}
	}
	return results
}
func (s *stubBroodOps) Preview(ctx context.Context, clusters []string, ns string, specs map[string]v1alpha1.BroodOperationSpec) []ClusterPreviewResult {
	results := make([]ClusterPreviewResult, len(clusters))
	for i, c := range clusters {
		results[i] = ClusterPreviewResult{Cluster: c, OK: true}
	}
	return results
}

// countingBroodOps counts the create and preview calls that reach the store
// and keeps the clusters of the last call. A cluster in unreachable answers
// with the row of a cluster that does not reply. A cluster in noOperator
// answers with the row of a cluster whose operator received nothing.
type countingBroodOps struct {
	stubBroodOps
	creates, previews int
	clusters          []string
	unreachable       map[string]bool
	noOperator        map[string]bool
}

func (c *countingBroodOps) Create(ctx context.Context, clusters []string, ns, name string, specs map[string]v1alpha1.BroodOperationSpec, startedBy string) []ClusterCreateResult {
	c.creates++
	c.clusters = append([]string(nil), clusters...)
	results := c.stubBroodOps.Create(ctx, clusters, ns, name, specs, startedBy)
	for i, r := range results {
		if c.unreachable[r.Cluster] {
			results[i] = ClusterCreateResult{Cluster: r.Cluster, Error: "cluster " + r.Cluster + " unreachable: nats: timeout"}
		}
		if c.noOperator[r.Cluster] {
			code, msg := clusterRowFailure(r.Cluster, nats.ErrNoResponders)
			results[i] = ClusterCreateResult{Cluster: r.Cluster, Code: code, Error: msg}
		}
	}
	return results
}

func (c *countingBroodOps) Preview(ctx context.Context, clusters []string, ns string, specs map[string]v1alpha1.BroodOperationSpec) []ClusterPreviewResult {
	c.previews++
	c.clusters = append([]string(nil), clusters...)
	results := c.stubBroodOps.Preview(ctx, clusters, ns, specs)
	for i, r := range results {
		if c.unreachable[r.Cluster] {
			results[i] = ClusterPreviewResult{Cluster: r.Cluster, Error: "cluster " + r.Cluster + " unreachable: nats: timeout"}
		}
		if c.noOperator[r.Cluster] {
			code, msg := clusterRowFailure(r.Cluster, nats.ErrNoResponders)
			results[i] = ClusterPreviewResult{Cluster: r.Cluster, Code: code, Error: msg}
		}
	}
	return results
}

// --- Test helpers ---

// newBroodTestServer creates a Server with minimal dependencies.
func newBroodTestServer() *Server {
	crClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	client := &minimalBroodClient{}
	return NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromMinimal(client),
		CRClient:          crClient,
		Authorizer:        newBroodTestAuthorizer(),
		Logger:            slog.Default(),
		OperatorNamespace: "operator-ns",
		Brood:             &stubBrood{localCluster: "core"},
		BroodOps:          &stubBroodOps{},
	})
}

// newBroodTestAuthorizer grants full admin to the "admins" group and
// controllers read to the "viewers" group.
func newBroodTestAuthorizer() *Authorizer {
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}},
	}
	adminBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}},
			RoleRef:  "admin",
		},
	}
	viewerRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}},
	}
	viewerBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
			RoleRef:  "viewer",
		},
	}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(
		[]*v1alpha1.VarroaRole{adminRole, viewerRole},
		[]*v1alpha1.VarroaRoleBinding{adminBinding, viewerBinding},
	), false)
}

// minimalBroodClient stubs BroodOperation CRD methods for tests.
type minimalBroodClient struct {
	controller.ResourceClient
	broodOps []*v1alpha1.BroodOperation
}

// storeFromMinimal seeds a fake store with the minimal client's brood
// operations (the store is the read surface after the crdstore migration).
func storeFromMinimal(c *minimalBroodClient) *crdstore.Fake {
	st := crdstore.NewFake()
	for _, o := range c.broodOps {
		crdstore.MustSeed(st, o)
	}
	return st
}

func storeFromStream(c *broodStreamClient) *crdstore.Fake {
	st := crdstore.NewFake()
	if c.nextOp != nil {
		crdstore.MustSeed(st, c.nextOp)
	}
	return st
}

func (m *minimalBroodClient) ListBroodOperationCRDs(_ context.Context, _ string) ([]*v1alpha1.BroodOperation, error) {
	return m.broodOps, nil
}
func (m *minimalBroodClient) GetBroodOperationCRD(_ context.Context, _, _ string) (*v1alpha1.BroodOperation, error) {
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("broodoperations"), "none")
}
func (m *minimalBroodClient) ApplyBroodOperationCRD(_ context.Context, _ *v1alpha1.BroodOperation) error {
	return nil
}
func (m *minimalBroodClient) DeleteBroodOperationCRD(_ context.Context, _, _ string) error {
	return nil
}
func (m *minimalBroodClient) PatchBroodOperationStatus(_ context.Context, _, _ string, _ *v1alpha1.BroodOperationStatus) error {
	return nil
}

// broodStreamClient returns pre-configured BroodOperation CRs for SSE tests.
type broodStreamClient struct {
	controller.ResourceClient
	nextOp *v1alpha1.BroodOperation
}

func (f *broodStreamClient) GetBroodOperationCRD(_ context.Context, _, _ string) (*v1alpha1.BroodOperation, error) {
	return f.nextOp, nil
}
func (f *broodStreamClient) ListBroodOperationCRDs(_ context.Context, _ string) ([]*v1alpha1.BroodOperation, error) {
	return nil, nil
}
func (f *broodStreamClient) ApplyBroodOperationCRD(_ context.Context, _ *v1alpha1.BroodOperation) error {
	return nil
}
func (f *broodStreamClient) DeleteBroodOperationCRD(_ context.Context, _, _ string) error {
	return nil
}
func (f *broodStreamClient) PatchBroodOperationStatus(_ context.Context, _, _ string, _ *v1alpha1.BroodOperationStatus) error {
	return nil
}
