package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/plugininv"
)

// ---------------------------------------------------------------------------
// spyBrood — records ListAll calls, controllable
// ---------------------------------------------------------------------------

type spyBrood struct {
	listAllCalls int
	cluster      string

	listAllControllers []ClusterController
	listAllStatuses    []ClusterFanoutStatus
	listAllErr         error

	known map[string]bool
}

func (s *spyBrood) LocalCluster() string { return s.cluster }
func (s *spyBrood) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	infos := make([]ClusterInfo, 0, len(s.known)+1)
	for name := range s.known {
		infos = append(infos, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: name}})
	}
	infos = append(infos, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: s.cluster}})
	return infos, nil
}
func (s *spyBrood) IsKnown(ctx context.Context, cluster string) bool {
	if cluster == s.cluster {
		return true
	}
	return s.known[cluster]
}
func (s *spyBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	s.listAllCalls++
	return s.listAllControllers, s.listAllStatuses, s.listAllErr
}
func (s *spyBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *spyBrood) Create(ctx context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	return nil, nil, fmt.Errorf("not implemented")
}
func (s *spyBrood) Preflight(ctx context.Context, cluster string, req ControllersCreateArgs) ([]bus.Check, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *spyBrood) Update(ctx context.Context, cluster, ns, name string, patch json.RawMessage, fieldManager string, force bool) (*v1alpha1.Controller, []bus.Check, error) {
	return nil, nil, fmt.Errorf("not implemented")
}
func (s *spyBrood) Delete(ctx context.Context, cluster, ns, name string) error    { return nil }
func (s *spyBrood) DeletePod(ctx context.Context, cluster, ns, name string) error { return nil }
func (s *spyBrood) Drain(ctx context.Context, cluster, requestedBy string) (string, error) {
	return "", nil
}
func (s *spyBrood) DrainCancel(ctx context.Context, cluster, requestedBy string) (string, error) {
	return "", nil
}
func (s *spyBrood) StateOf(ctx context.Context, cluster string) (string, error) {
	return "", nil
}
func (s *spyBrood) DiscoverNamespaces(ctx context.Context, cluster string) (*bus.NamespacesListResponse, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// makePluginController builds a test controller. All parameters have defaults
// in callers but are kept as params for clarity when a test needs a variant.
//
//nolint:unparam
func makePluginController(name string, stale, degraded, truncated bool, dt bool) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
			PluginInventory: &v1alpha1.PluginInventoryStatus{
				Hash:           "abc",
				Source:         "jenkins-api",
				Stale:          stale,
				Degraded:       degraded,
				Truncated:      truncated,
				Total:          1,
				DriftTruncated: dt,
			},
		},
	}
}

func makeTestServer(reader FleetPluginInventory, brood Brood, authorizer *Authorizer) *Server {
	logger := slog.New(slog.DiscardHandler)
	return &Server{
		deps: &Dependencies{
			Logger:               logger,
			FleetPluginInventory: reader,
			Brood:                brood,
			Authorizer:           authorizer,
		},
	}
}

func decodeJSON(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 5.8: 405, 400, absent affected, 404, spy-zero-calls
// ---------------------------------------------------------------------------

func TestHandleFleetPlugins_MethodNotAllowed(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/fleet/plugins", nil)
		w := httptest.NewRecorder()
		srv.HandleFleetPlugins(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", method, w.Code)
		}
	}
}

func TestHandleFleetPlugins_ParseErrorNoOperator(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=4.0.0", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	body := decodeJSON(t, w.Body)
	if errMsg, ok := body["error"].(string); !ok || !strings.Contains(errMsg, "no recognised comparison operator") {
		t.Errorf("error = %v, want 'no recognised comparison operator'", body["error"])
	}
}

func TestHandleFleetPlugins_ParseErrorEmptyOperand(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=<", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_ParseErrorEmptyClause(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=<1.0,", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for trailing empty clause, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_PresentButBlankAffected(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for present-but-blank affected, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_UnknownCluster404(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	spy := &spyBrood{cluster: "core", known: map[string]bool{}}
	srv := makeTestServer(reader, spy, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?cluster=nosuch", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown cluster, got %d", w.Code)
	}
}

func TestHandleFleetPluginDetail_EmptyName404(t *testing.T) {
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins/", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPluginDetail(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for empty name, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_SpyBroodZeroCallsOn502(t *testing.T) {
	spy := &spyBrood{cluster: "core", known: map[string]bool{}}
	srv := makeTestServer(nil, spy, nil) // reader is nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
	if spy.listAllCalls != 0 {
		t.Errorf("expected 0 ListAll calls on 502 path, got %d", spy.listAllCalls)
	}
}

func TestHandleFleetPlugins_SpyBroodZeroCallsOn400(t *testing.T) {
	spy := &spyBrood{cluster: "core", known: map[string]bool{}}
	reader := newFakeFleetInventory(nil, nil)
	srv := makeTestServer(reader, spy, nil)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=<", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if spy.listAllCalls != 0 {
		t.Errorf("expected 0 ListAll calls on 400 path, got %d", spy.listAllCalls)
	}
}

// ---------------------------------------------------------------------------
// 5.9: 502, 400-before-502, Authorizer-nil, RBAC
// ---------------------------------------------------------------------------

func TestHandleFleetPlugins_502WithPopulatedFleet(t *testing.T) {
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	srv := makeTestServer(nil, spy, nil) // reader nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 with populated fleet, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_502WithZeroRows(t *testing.T) {
	spy := &spyBrood{
		cluster:            "core",
		known:              map[string]bool{},
		listAllControllers: []ClusterController{},
		listAllStatuses:    []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	srv := makeTestServer(nil, spy, nil) // reader nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 with zero rows, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_502BothReaderAndAuthorizerNil(t *testing.T) {
	spy := &spyBrood{
		cluster:            "core",
		known:              map[string]bool{},
		listAllControllers: []ClusterController{},
		listAllStatuses:    []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	srv := makeTestServer(nil, spy, nil) // reader+authorizer both nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 with both nil, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_400Precedes502(t *testing.T) {
	spy := &spyBrood{cluster: "core", known: map[string]bool{}}
	srv := makeTestServer(nil, spy, nil) // reader nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?affected=<", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 before 502, got %d", w.Code)
	}
}

func TestHandleFleetPlugins_AuthorizerNilReturnsZeroRows(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1.0", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	srv := makeTestServer(reader, spy, nil) // Authorizer nil
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w.Body)
	items, _ := resp["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected 0 items with nil authorizer, got %d", len(items))
	}
	cov, _ := resp["coverage"].(map[string]any)
	if ct, _ := cov["controllersTotal"].(float64); ct != 0 {
		t.Errorf("controllersTotal = %v, want 0", ct)
	}
}

func TestHandleFleetPlugins_PermissiveAuthorizerReturnsRows(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1.0", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	// Authorizer with nil resolver + defaultRead=true: nil claims → permits reads.
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil) // nil claims
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w.Body)
	cov, _ := resp["coverage"].(map[string]any)
	if ct, _ := cov["controllersTotal"].(float64); ct != 1 {
		t.Errorf("controllersTotal = %v, want 1", ct)
	}
}

// ---------------------------------------------------------------------------
// 5.10: R22/R28 coverage — remote clusters, cluster filter
// ---------------------------------------------------------------------------

func TestHandleFleetPlugins_RemoteClusterNotCovered(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{"remote": true},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
			{Cluster: "remote", CR: makePluginController("ctrl-r", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "remote", OK: true},
		},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w.Body)
	cov, _ := resp["coverage"].(map[string]any)
	if complete, _ := cov["complete"].(bool); complete {
		t.Error("coverage.complete should be false with remote cluster")
	}
	if cnc, _ := cov["clustersNotCovered"].(float64); cnc != 1 {
		t.Errorf("clustersNotCovered = %v, want 1", cnc)
	}
	clusters, _ := resp["clusters"].([]any)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}
	var remote map[string]any
	for _, c := range clusters {
		cm := c.(map[string]any)
		if cm["name"].(string) == "remote" {
			remote = cm
		}
	}
	if remote == nil {
		t.Fatal("remote cluster not found in clusters array")
	}
	if remote["ok"].(bool) {
		t.Error("remote cluster should be ok: false")
	}
	if ct, _ := cov["controllersTotal"].(float64); ct != 1 {
		t.Errorf("controllersTotal = %v, want 1 (remote not counted)", ct)
	}
}

func TestHandleFleetPlugins_RemoteClusterNoVisibleControllersStillNotCovered(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{"remote": true},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
			{Cluster: "remote", CR: makePluginController("ctrl-r", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "remote", OK: true},
		},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	cov, _ := resp["coverage"].(map[string]any)
	if complete, _ := cov["complete"].(bool); complete {
		t.Error("coverage.complete should be false even with zero caller-visible remote controllers")
	}
	if cnc, _ := cov["clustersNotCovered"].(float64); cnc != 1 {
		t.Errorf("clustersNotCovered = %v, want 1", cnc)
	}
}

func TestHandleFleetPlugins_ClusterFilterCannotRaiseCompleteness(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{"remote": true},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "remote", OK: true},
		},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins?cluster=core", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	cov, _ := resp["coverage"].(map[string]any)
	if complete, _ := cov["complete"].(bool); complete {
		t.Error("coverage.complete should be false with ?cluster=core in multi-cluster install")
	}
}

func TestHandleFleetPlugins_SingleClusterComplete(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "p", Version: "1", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	cov, _ := resp["coverage"].(map[string]any)
	if complete, _ := cov["complete"].(bool); !complete {
		t.Error("coverage.complete should be true with single local cluster")
	}
	if cnc, _ := cov["clustersNotCovered"].(float64); cnc != 0 {
		t.Errorf("clustersNotCovered = %v, want 0", cnc)
	}
}

func TestHandleFleetPluginDetail_UnknownPluginEmptyAnswer(t *testing.T) {
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "other-plugin", Version: "1", Class: plugininv.ClassDeclared}},
			CollectedAt: time.Now(),
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins/does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPluginDetail(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown plugin, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	items, _ := resp["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected 0 items for unknown plugin, got %d", len(items))
	}
}

func TestHandleFleetPluginDetail_DrilldownReturnsDrillItems(t *testing.T) {
	collectedAt := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	reader := newFakeFleetInventory(map[types.NamespacedName]ControllerInventory{
		nn("ctrl-a"): {
			Plugins:     []InstalledPlugin{{Name: "git-client", Version: "4.0.0", Class: plugininv.ClassDeclared}},
			CollectedAt: collectedAt,
			Envelope:    ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"},
		},
	}, nil)
	spy := &spyBrood{
		cluster: "core",
		known:   map[string]bool{},
		listAllControllers: []ClusterController{
			{Cluster: "core", CR: makePluginController("ctrl-a", false, false, false, false)},
		},
		listAllStatuses: []ClusterFanoutStatus{{Name: "core", OK: true}},
	}
	authz := NewAuthorizer(nil, true)
	srv := makeTestServer(reader, spy, authz)
	req := httptest.NewRequest(http.MethodGet, "/fleet/plugins/git-client", nil)
	w := httptest.NewRecorder()
	srv.HandleFleetPluginDetail(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w.Body)
	items, _ := resp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 drilldown item, got %d", len(items))
	}
	di := items[0].(map[string]any)
	if di["controller"] != "ctrl-a" {
		t.Errorf("controller = %v, want ctrl-a", di["controller"])
	}
	if di["version"] != "4.0.0" {
		t.Errorf("version = %v, want 4.0.0", di["version"])
	}
	if di["class"] != plugininv.ClassDeclared {
		t.Errorf("class = %v, want %s", di["class"], plugininv.ClassDeclared)
	}
}

// ---------------------------------------------------------------------------
// Contract cases for fleet plugin routes
// ---------------------------------------------------------------------------

func init() {
	// The contract-test server does not wire FleetPluginInventory, so both
	// fleet plugin endpoints return 502. These cases verify the routes exist
	// and the response shape matches the spec.
	registerContractCases(
		contractCase{
			Name:       "listFleetPlugins",
			Method:     "GET",
			Path:       "/api/v1/fleet/plugins",
			Claims:     adminClaims,
			WantStatus: http.StatusBadGateway,
		},
		contractCase{
			Name:       "getFleetPlugin",
			Method:     "GET",
			Path:       "/api/v1/fleet/plugins/git-client",
			Claims:     adminClaims,
			WantStatus: http.StatusBadGateway,
		},
	)
}
