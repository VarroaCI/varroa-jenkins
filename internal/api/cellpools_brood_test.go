package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// twoClusterBrood knows the local cluster core and the members it names.
// Only the methods the cell-pool service calls have a body.
type twoClusterBrood struct {
	Brood
	members []string
}

func (b *twoClusterBrood) LocalCluster() string { return "core" }

func (b *twoClusterBrood) Clusters(context.Context) ([]ClusterInfo, error) {
	out := make([]ClusterInfo, 0, 1+len(b.members))
	out = append(out, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true})
	for _, m := range b.members {
		out = append(out, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: m}, Healthy: true})
	}
	return out, nil
}

func (b *twoClusterBrood) IsKnown(_ context.Context, cluster string) bool {
	if cluster == "core" {
		return true
	}
	for _, m := range b.members {
		if m == cluster {
			return true
		}
	}
	return false
}

// memberPoolBrood serves member pools from memory. A cluster in
// unreachable answers ErrClusterUnreachable, the way busConfigBrood does
// when no operator replies.
type memberPoolBrood struct {
	ConfigBrood
	mu          sync.Mutex
	pools       map[string]map[string]*v1alpha1.CellPool
	unreachable map[string]bool
	failList    map[string]bool
	scales      []bus.CellPoolScaleRequest
	scaleTarget []string
}

func (m *memberPoolBrood) ListCellPools(_ context.Context, cluster string) ([]v1alpha1.CellPool, error) {
	if m.unreachable[cluster] {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: errors.New("no responders")}
	}
	if m.failList[cluster] {
		return nil, &BroodError{Code: bus.CodeInternal, Msg: "list failed"}
	}
	var out []v1alpha1.CellPool
	for _, p := range m.pools[cluster] {
		out = append(out, *p)
	}
	return out, nil
}

func (m *memberPoolBrood) GetCellPool(_ context.Context, cluster, name string) (*v1alpha1.CellPool, error) {
	if m.unreachable[cluster] {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: errors.New("no responders")}
	}
	p, ok := m.pools[cluster][name]
	if !ok {
		return nil, &BroodError{Code: bus.CodeNotFound, Msg: "not found"}
	}
	return p, nil
}

func (m *memberPoolBrood) ScaleCellPool(_ context.Context, cluster string, req bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scales = append(m.scales, req)
	m.scaleTarget = append(m.scaleTarget, cluster)
	if m.unreachable[cluster] {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: errors.New("no responders")}
	}
	p, ok := m.pools[cluster][req.Name]
	if !ok {
		return nil, &BroodError{Code: bus.CodeNotFound, Msg: "not found"}
	}
	updated := p.DeepCopy()
	if err := cells.ApplyCellPoolDials(&updated.Spec, cells.CellPoolDials{
		Warm: req.Warm, MaxCells: req.MaxCells, MaxTenantsPerCell: req.MaxTenantsPerCell, UnreadyGracePeriod: req.UnreadyGracePeriod,
	}); err != nil {
		return nil, &BroodError{Code: bus.CodeInvalid, Msg: err.Error()}
	}
	m.pools[cluster][req.Name] = updated
	return updated, nil
}

func memberPool(name string, maxTenants int32) *v1alpha1.CellPool {
	return &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-570", Warm: 1, MaxCells: 4, MaxTenantsPerCell: maxTenants},
	}
}

// broodCellPoolServer builds a server whose brood knows core and member-c.
// The core store holds jenkins-version-2-570 and p; member-c holds
// member-2-570 and p.
func broodCellPoolServer(t *testing.T) (*Server, *crdstore.Fake, *memberPoolBrood, *recordingActivitySink, context.Context) {
	t.Helper()
	srv, store, ctx := cellPoolTestServer(t, nil)
	crdstore.MustSeed(store, memberPool("jenkins-version-2-570", 10), memberPool("p", 10))
	members := &memberPoolBrood{
		pools: map[string]map[string]*v1alpha1.CellPool{
			"member-c": {"member-2-570": memberPool("member-2-570", 4), "p": memberPool("p", 4)},
		},
		unreachable: map[string]bool{},
		failList:    map[string]bool{},
	}
	sink := &recordingActivitySink{}
	srv.deps.Brood = &twoClusterBrood{members: []string{"member-c"}}
	srv.deps.ConfigBrood = members
	srv.deps.ActivityPublisher = sink
	return srv, store, members, sink, ctx
}

func listCellPools(ctx context.Context, t *testing.T, srv *Server, query string) (int, cellPoolListResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/cellpools"+query, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPools(w, req)
	var body cellPoolListResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	}
	return w.Code, body
}

func TestCellPools_ListSpansEveryCluster(t *testing.T) {
	srv, _, _, _, ctx := broodCellPoolServer(t)
	code, body := listCellPools(ctx, t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	got := make([]string, 0, len(body.Items))
	for _, it := range body.Items {
		got = append(got, it.Cluster+"/"+it.Name)
	}
	want := []string{"core/jenkins-version-2-570", "core/p", "member-c/member-2-570", "member-c/p"}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items = %v, want %v (local cluster first, then cluster, then name)", got, want)
		}
	}
	if len(body.Clusters) != 2 || body.Clusters[0].Name != "core" || !body.Clusters[0].OK ||
		body.Clusters[1].Name != "member-c" || !body.Clusters[1].OK {
		t.Errorf("clusters = %+v, want core and member-c, both ok", body.Clusters)
	}
}

func TestCellPools_UnreachableMemberDoesNotFailTheList(t *testing.T) {
	srv, _, members, _, ctx := broodCellPoolServer(t)
	members.unreachable["member-c"] = true
	code, body := listCellPools(ctx, t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, it := range body.Items {
		if it.Cluster == "member-c" {
			t.Errorf("an unreachable member must add no item, got %s", it.Name)
		}
	}
	var member-c *ClusterFanoutStatus
	for i := range body.Clusters {
		if body.Clusters[i].Name == "member-c" {
			member-c = &body.Clusters[i]
		}
	}
	if member-c == nil || member-c.OK || member-c.Error != "cluster member-c unreachable" {
		t.Errorf("member-c status = %+v, want ok false naming member-c", member-c)
	}
}

func TestCellPools_ListNarrowsToOneCluster(t *testing.T) {
	srv, _, _, _, ctx := broodCellPoolServer(t)
	code, body := listCellPools(ctx, t, srv, "?cluster=member-c")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want the two member-c pools", len(body.Items))
	}
	for _, it := range body.Items {
		if it.Cluster != "member-c" {
			t.Errorf("item %s names cluster %q, want member-c", it.Name, it.Cluster)
		}
	}
	if len(body.Clusters) != 1 || body.Clusters[0].Name != "member-c" {
		t.Errorf("clusters = %+v, want member-c alone", body.Clusters)
	}
}

func TestCellPools_UnknownClusterIsA404(t *testing.T) {
	srv, _, _, _, ctx := broodCellPoolServer(t)
	if code, _ := listCellPools(ctx, t, srv, "?cluster=nowhere"); code != http.StatusNotFound {
		t.Errorf("list status = %d, want 404", code)
	}

	req := httptest.NewRequest(http.MethodPatch, "/cellpools/p?cluster=nowhere",
		bytes.NewReader([]byte(`{"spec":{"warm":1}}`))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("patch status = %d, want 404", w.Code)
	}
}

func TestCellPools_GetRoutesToTheNamedCluster(t *testing.T) {
	srv, _, _, _, ctx := broodCellPoolServer(t)

	req := httptest.NewRequest(http.MethodGet, "/cellpools/member-2-570?cluster=member-c", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var entry cellPoolEntry
	_ = json.Unmarshal(w.Body.Bytes(), &entry)
	if entry.Cluster != "member-c" || entry.Name != "member-2-570" {
		t.Errorf("entry = %s/%s, want member-c/member-2-570", entry.Cluster, entry.Name)
	}

	// Without a cluster the read uses the local cluster, which holds no
	// member-2-570.
	req = httptest.NewRequest(http.MethodGet, "/cellpools/member-2-570", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status without a cluster = %d, want 404", w.Code)
	}
}

func TestCellPools_UnreachableMemberIsA502(t *testing.T) {
	srv, _, members, _, ctx := broodCellPoolServer(t)
	members.unreachable["member-c"] = true

	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		req := httptest.NewRequest(method, "/cellpools/member-2-570?cluster=member-c",
			bytes.NewReader([]byte(`{"spec":{"warm":2}}`))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleCellPoolDispatch(w, req)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s status = %d, want 502", method, w.Code)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("member-c")) {
			t.Errorf("%s error must name member-c: %s", method, w.Body.String())
		}
	}
}

func TestCellPools_PatchScalesTheMemberPool(t *testing.T) {
	srv, store, members, sink, ctx := broodCellPoolServer(t)

	req := httptest.NewRequest(http.MethodPatch, "/cellpools/p?cluster=member-c",
		bytes.NewReader([]byte(`{"spec":{"warm":2}}`))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(members.scales) != 1 || members.scaleTarget[0] != "member-c" || members.scales[0].Name != "p" ||
		members.scales[0].Warm == nil || *members.scales[0].Warm != 2 || members.scales[0].MaxCells != nil {
		t.Fatalf("scale requests = %+v to %v, want one warm-only scale of p on member-c", members.scales, members.scaleTarget)
	}
	var entry cellPoolEntry
	_ = json.Unmarshal(w.Body.Bytes(), &entry)
	if entry.Cluster != "member-c" || entry.Spec.Warm != 2 {
		t.Errorf("entry = %+v, want member-c with warm 2", entry)
	}
	// The core pool of the same name is unchanged.
	core, _ := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "p", "")
	if core.Spec.Warm != 1 {
		t.Errorf("core p warm = %d, want the stored 1", core.Spec.Warm)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "cellpool.updated" || sink.events[0].Cluster != "member-c" {
		t.Errorf("events = %+v, want one cellpool.updated naming member-c", sink.events)
	}
}

func TestCellPools_RefusedScalePublishesNothing(t *testing.T) {
	srv, _, _, sink, ctx := broodCellPoolServer(t)
	for _, body := range []string{`{"spec":{"warm":-1}}`, `{"spec":{"warm":9}}`} {
		req := httptest.NewRequest(http.MethodPatch, "/cellpools/p?cluster=member-c",
			bytes.NewReader([]byte(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleCellPoolDispatch(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, w.Code)
		}
	}
	if len(sink.events) != 0 {
		t.Errorf("a refused scale must publish nothing, got %+v", sink.events)
	}
}

func TestCellPools_LocalScalePublishesTheLocalCluster(t *testing.T) {
	srv, _, members, sink, ctx := broodCellPoolServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/cellpools/p",
		bytes.NewReader([]byte(`{"spec":{"warm":2}}`))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(members.scales) != 0 {
		t.Errorf("a local scale must not reach ConfigBrood, got %+v", members.scales)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "core" {
		t.Errorf("events = %+v, want one naming core", sink.events)
	}
}

func TestCellPools_SingleClusterInstallListsCore(t *testing.T) {
	srv, store, ctx := cellPoolTestServer(t, nil)
	seedCellPool(store, "lts-2-555", 1)
	code, body := listCellPools(ctx, t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(body.Items) != 1 || body.Items[0].Cluster != "core" {
		t.Errorf("items = %+v, want one item on core", body.Items)
	}
	if len(body.Clusters) != 1 || body.Clusters[0].Name != "core" || !body.Clusters[0].OK {
		t.Errorf("clusters = %+v, want core ok", body.Clusters)
	}
	if code, _ := listCellPools(ctx, t, srv, "?cluster=member-c"); code != http.StatusNotFound {
		t.Errorf("another cluster without a brood router: status = %d, want 404", code)
	}
}

func TestCellPools_ForbiddenWithoutTheGate(t *testing.T) {
	srv, _, _, _, _ := broodCellPoolServer(t)
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "nobody"})
	if code, _ := listCellPools(ctx, t, srv, ""); code != http.StatusForbidden {
		t.Errorf("list status = %d, want 403", code)
	}
	req := httptest.NewRequest(http.MethodPatch, "/cellpools/p?cluster=member-c",
		bytes.NewReader([]byte(`{"spec":{"warm":2}}`))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("patch status = %d, want 403", w.Code)
	}
}

func poolCell(name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cells", Labels: map[string]string{cells.LabelCellPool: "p"}},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	}
}

func TestCellCap_MemberCellReadsItsOwnPool(t *testing.T) {
	srv, _, _, _, _ := broodCellPoolServer(t)
	caps := srv.cellCaps(context.Background())
	if got := caps.resolve("member-c", poolCell("d1")); got != 4 {
		t.Errorf("member-c cell cap = %d, want the member-c pool's 4", got)
	}
	if got := caps.resolve("core", poolCell("c1")); got != 10 {
		t.Errorf("core cell cap = %d, want the core pool's 10", got)
	}
	if got := caps.resolve("", poolCell("c2")); got != 10 {
		t.Errorf("an empty cluster is the local cluster: cap = %d, want 10", got)
	}
}

func TestCellCap_FailedMemberReadFallsBackToTheChartValue(t *testing.T) {
	srv, _, members, _, _ := broodCellPoolServer(t)
	srv.deps.CellMaxTenants = 7
	members.failList["member-c"] = true
	if got := srv.resolveCellCap(context.Background(), "member-c", poolCell("d1")); got != 7 {
		t.Errorf("cap = %d, want the chart value 7", got)
	}
}

func TestFillCellSummary_UsesTheRowCluster(t *testing.T) {
	srv, _, _, _, _ := broodCellPoolServer(t)
	caps := srv.cellCaps(context.Background())
	var row controllerResponse
	srv.fillCellSummary(poolCell("d1"), "member-c", &row, caps)
	if row.MaxTenants == nil || *row.MaxTenants != 4 {
		t.Errorf("maxTenants = %v, want the member-c pool's 4", row.MaxTenants)
	}
}
