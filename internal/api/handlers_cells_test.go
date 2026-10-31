package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeCellReader answers the three cell-usage methods from a seeded map.
// Embedding recordstore.Reader keeps every other method unimplemented, so
// a test that depends on one panics rather than reading a wrong value.
type fakeCellReader struct {
	recordstore.Reader

	schemaTooOld bool
	// unreachable makes every read fail as a store that did not answer.
	unreachable bool
	// usage is keyed by "<cluster>/<namespace>/<cell>".
	usage map[string]recordstore.CellUsage
	// tenantUsage is keyed by "<cluster>/<namespace>/<cell>/<tenant>" and
	// holds one row for each of that tenant's jobs.
	tenantUsage map[string][]recordstore.CellJobUsage
}

func newFakeCellReader() *fakeCellReader {
	return &fakeCellReader{
		usage:       map[string]recordstore.CellUsage{},
		tenantUsage: map[string][]recordstore.CellJobUsage{},
	}
}

func (f *fakeCellReader) cellKey(cluster, namespace, cell string) string {
	return cluster + "/" + namespace + "/" + cell
}

func (f *fakeCellReader) CellUsage(ctx context.Context, cluster, namespace, cell string) (recordstore.CellUsage, error) {
	many, err := f.CellUsageMany(ctx, cluster, namespace, []string{cell})
	if err != nil {
		return recordstore.CellUsage{}, err
	}
	return many[cell], nil
}

func (f *fakeCellReader) CellUsageMany(_ context.Context, cluster, namespace string, cellNames []string) (map[string]recordstore.CellUsage, error) {
	if f.unreachable {
		return nil, fmt.Errorf("%w: read cell usage: connection refused", recordstore.ErrUnavailable)
	}
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	out := make(map[string]recordstore.CellUsage, len(cellNames))
	for _, cell := range cellNames {
		usage, ok := f.usage[f.cellKey(cluster, namespace, cell)]
		if !ok {
			usage = recordstore.CellUsage{Cluster: cluster, Namespace: namespace, Cell: cell}
		}
		if usage.WindowStart.IsZero() {
			usage.WindowStart = time.Now().UTC().Add(-24 * time.Hour)
		}
		out[cell] = usage
	}
	return out, nil
}

func (f *fakeCellReader) CellTenantUsage(_ context.Context, cluster, namespace, cell, tenant string) ([]recordstore.CellJobUsage, error) {
	if f.unreachable {
		return nil, fmt.Errorf("%w: read cell usage: connection refused", recordstore.ErrUnavailable)
	}
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	return f.tenantUsage[f.cellKey(cluster, namespace, cell)+"/"+tenant], nil
}

// cellController builds one cell Controller fixture.
func newCellControllerFixture(namespace, name string, tenants []string, maxTenants int32, labels map[string]string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: maxTenants}},
		Status: v1alpha1.ControllerStatus{
			Cell:       &v1alpha1.CellStatus{Tenants: tenants},
			MiteStatus: &v1alpha1.MiteStatus{JenkinsHealth: "healthy", JenkinsVersion: "2.531.1"},
		},
	}
}

// cellReadAuthorizer grants controllers:read in the named namespaces.
func cellReadAuthorizer(namespaces ...string) *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "cell-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cell-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-user"}},
			RoleRef:  "cell-viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: namespaces},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

var cellUserClaims = &auth.Claims{Subject: "cell-user"}

// cellsTestServer assembles a *Server wired for the two cell usage
// reads.
func cellsTestServer(client *fakeResourceClient, authorizer *Authorizer, reader recordstore.Reader) *Server {
	store := storeFromFake(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             newFakeBrood(client),
		Authorizer:        authorizer,
		RecordStore:       reader,
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
		CellMaxTenants:    0,
	}
	return NewServer(deps)
}

// doCellRequest calls one cell handler with the given path values and
// claims.
func doCellRequest(t *testing.T, handler func(http.ResponseWriter, *http.Request), path string, pathValues map[string]string, claims *auth.Claims) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	if claims != nil {
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// decodeCellUsage decodes one cellUsageJSON body.
func decodeCellUsage(t *testing.T, rec *httptest.ResponseRecorder) cellUsageJSON {
	t.Helper()
	var out cellUsageJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode cell usage: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// decodeCellUsageList decodes the items envelope of the batch read.
func decodeCellUsageList(t *testing.T, rec *httptest.ResponseRecorder) []cellUsageJSON {
	t.Helper()
	var out struct {
		Items []cellUsageJSON `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode cell usage list: %v (body %s)", err, rec.Body.String())
	}
	return out.Items
}

// twoCellClient seeds one readable cell, one unreadable cell in another
// namespace, and one ordinary controller.
func twoCellClient() *fakeResourceClient {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"cell-a": newCellControllerFixture("ns-a", "cell-a", []string{"acme", "beta"}, 4,
			map[string]string{cells.LabelCellPool: "pool-a", cells.LabelCellProfile: "lts-2-531"}),
		"cell-b": newCellControllerFixture("ns-b", "cell-b", []string{"gamma"}, 0, nil),
		"plain": {
			ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "ns-a"},
			Spec:       v1alpha1.ControllerSpec{},
		},
	}
	return client
}

// TestListCellUsageDropsUnreadableCell asserts the batch read lists the
// cell the caller may read and does not name the other.
func TestListCellUsageDropsUnreadableCell(t *testing.T) {
	client := twoCellClient()
	reader := newFakeCellReader()
	reader.usage["core/ns-a/cell-a"] = recordstore.CellUsage{
		Cluster: "core", Namespace: "ns-a", Cell: "cell-a",
		Tenants: []recordstore.CellTenantUsage{
			{Tenant: "acme", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 1, RunsLast24h: 4, FailedLast24h: 1, Jobs: 3}},
			{Tenant: "beta", CellUsageCounts: recordstore.CellUsageCounts{Jobs: 1}},
		},
		Totals: recordstore.CellUsageCounts{RunningNow: 1, RunsLast24h: 4, FailedLast24h: 1, Jobs: 4},
	}
	srv := cellsTestServer(client, cellReadAuthorizer("ns-a"), reader)

	rec := doCellRequest(t, srv.handleListCellUsage, "/cells/usage", nil, cellUserClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	items := decodeCellUsageList(t, rec)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1: %s", len(items), rec.Body.String())
	}
	if items[0].Name != "cell-a" {
		t.Errorf("name = %q, want cell-a", items[0].Name)
	}
	if items[0].MaxTenants != 4 || items[0].FreeSlots != 2 {
		t.Errorf("maxTenants = %d, freeSlots = %d, want 4 and 2", items[0].MaxTenants, items[0].FreeSlots)
	}
	if items[0].CellPool != "pool-a" || items[0].Profile != "lts-2-531" {
		t.Errorf("cellPool = %q, profile = %q, want pool-a and lts-2-531", items[0].CellPool, items[0].Profile)
	}
	if len(items[0].Tenants) != 2 || items[0].Totals.Jobs != 4 {
		t.Errorf("tenants = %+v, totals = %+v", items[0].Tenants, items[0].Totals)
	}
}

// TestGetCellUsageRefusesAnUnreadableCell asserts the single read answers
// 404 for a caller the controller read check refuses.
func TestGetCellUsageRefusesAnUnreadableCell(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), newFakeCellReader())
	rec := doCellRequest(t, srv.handleCellUsage, "/cells/ns-b/cell-b/usage",
		map[string]string{"namespace": "ns-b", "name": "cell-b"}, cellUserClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestGetCellUsageRefusesAnOrdinaryController asserts a controller with
// no cell role answers 404.
func TestGetCellUsageRefusesAnOrdinaryController(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), newFakeCellReader())
	rec := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/plain/usage",
		map[string]string{"namespace": "ns-a", "name": "plain"}, cellUserClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestCellUsageDeniesWithoutAuthorizer asserts a nil authorizer denies
// both paths: the single read is not found, and the list is empty.
func TestCellUsageDeniesWithoutAuthorizer(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), nil, newFakeCellReader())

	single := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if single.Code != http.StatusNotFound {
		t.Errorf("single status = %d, want 404", single.Code)
	}

	list := doCellRequest(t, srv.handleListCellUsage, "/cells/usage", nil, cellUserClaims)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", list.Code)
	}
	if items := decodeCellUsageList(t, list); len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
}

// errorBodyCode returns the code field of a JSON error body, or "" when
// the body carries none.
func errorBodyCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (body %s)", err, rec.Body.String())
	}
	return body["code"]
}

// TestCellUsageWithoutRecordStore asserts both paths answer 503 with no
// code when the record store is absent. A store that is off is not a
// store that did not answer.
func TestCellUsageWithoutRecordStore(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), nil)

	single := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if single.Code != http.StatusServiceUnavailable {
		t.Errorf("single status = %d, want 503", single.Code)
	}
	if got := errorBodyCode(t, single); got != "" {
		t.Errorf("single code = %q, want none", got)
	}

	list := doCellRequest(t, srv.handleListCellUsage, "/cells/usage", nil, cellUserClaims)
	if list.Code != http.StatusServiceUnavailable {
		t.Errorf("list status = %d, want 503", list.Code)
	}
	if got := errorBodyCode(t, list); got != "" {
		t.Errorf("list code = %q, want none", got)
	}
}

// TestCellUsageWithAnOldSchema asserts both paths answer 503 with no code
// when the reader reports an old schema.
func TestCellUsageWithAnOldSchema(t *testing.T) {
	reader := newFakeCellReader()
	reader.schemaTooOld = true
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), reader)

	single := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if single.Code != http.StatusServiceUnavailable {
		t.Errorf("single status = %d, want 503", single.Code)
	}
	if got := errorBodyCode(t, single); got != "" {
		t.Errorf("single code = %q, want none", got)
	}

	list := doCellRequest(t, srv.handleListCellUsage, "/cells/usage", nil, cellUserClaims)
	if list.Code != http.StatusServiceUnavailable {
		t.Errorf("list status = %d, want 503", list.Code)
	}
	if got := errorBodyCode(t, list); got != "" {
		t.Errorf("list code = %q, want none", got)
	}
}

// TestCellUsageUnreachableCarriesTheCode asserts both paths answer 503 with
// the unreachable code when the store does not answer. The page tells this
// case apart from a store that is off, so the code must reach the body.
func TestCellUsageUnreachableCarriesTheCode(t *testing.T) {
	reader := newFakeCellReader()
	reader.unreachable = true
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), reader)

	single := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if single.Code != http.StatusServiceUnavailable {
		t.Errorf("single status = %d, want 503", single.Code)
	}
	if got := errorBodyCode(t, single); got != codeRecordStoreUnreachable {
		t.Errorf("single code = %q, want %q", got, codeRecordStoreUnreachable)
	}

	list := doCellRequest(t, srv.handleListCellUsage, "/cells/usage", nil, cellUserClaims)
	if list.Code != http.StatusServiceUnavailable {
		t.Errorf("list status = %d, want 503", list.Code)
	}
	if got := errorBodyCode(t, list); got != codeRecordStoreUnreachable {
		t.Errorf("list code = %q, want %q", got, codeRecordStoreUnreachable)
	}
}

// TestCellUsageOmitsGaugesForARemoteCluster asserts a remote cell reports
// no queue length and no gauge time, and reads its Jenkins health from
// the stored mite status.
func TestCellUsageOmitsGaugesForARemoteCluster(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), newFakeCellReader())
	rec := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage?cluster=remote",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	usage := decodeCellUsage(t, rec)
	if usage.QueueLength != nil || usage.QueueLengthAt != nil {
		t.Errorf("queueLength = %v, queueLengthAt = %v, want both absent", usage.QueueLength, usage.QueueLengthAt)
	}
	if usage.JenkinsHealth != "healthy" {
		t.Errorf("jenkinsHealth = %q, want healthy from the stored mite status", usage.JenkinsHealth)
	}
	if usage.Cluster != "remote" {
		t.Errorf("cluster = %q, want remote", usage.Cluster)
	}
}

// TestCellUsageReportsTheChartDefaultCap asserts a cell whose
// spec.cell.maxTenants is zero reports the environment value, and a
// cell with no pool label omits cellPool.
func TestCellUsageReportsTheChartDefaultCap(t *testing.T) {
	client := twoCellClient()
	srv := cellsTestServer(client, cellReadAuthorizer("ns-a", "ns-b"), newFakeCellReader())
	srv.deps.CellMaxTenants = 10

	rec := doCellRequest(t, srv.handleCellUsage, "/cells/ns-b/cell-b/usage",
		map[string]string{"namespace": "ns-b", "name": "cell-b"}, cellUserClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	usage := decodeCellUsage(t, rec)
	if usage.MaxTenants != 10 || usage.FreeSlots != 9 {
		t.Errorf("maxTenants = %d, freeSlots = %d, want 10 and 9", usage.MaxTenants, usage.FreeSlots)
	}
	if usage.CellPool != "" {
		t.Errorf("cellPool = %q, want absent", usage.CellPool)
	}
}

// TestResolveCellCapChain asserts the first-match chain over the four
// tiers: the cell value, the owning pool's maxTenantsPerCell, the chart
// value, and the compiled default. A cell whose pool is gone skips the
// pool tier.
func TestResolveCellCapChain(t *testing.T) {
	client := newFakeResourceClient()
	crdstore.MustSeed(client.crdStore, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lts"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "jenkins-lts", MaxTenantsPerCell: 4},
	})
	srv := cellsTestServer(client, nil, nil)
	ctx := context.Background()

	withCap := newCellControllerFixture("ns-a", "cell-a", nil, 3, map[string]string{cells.LabelCellPool: "lts"})
	if got := srv.resolveCellCap(ctx, "", withCap); got != 3 {
		t.Errorf("cell tier = %d, want 3", got)
	}

	pooled := newCellControllerFixture("ns-a", "cell-b", nil, 0, map[string]string{cells.LabelCellPool: "lts"})
	if got := srv.resolveCellCap(ctx, "", pooled); got != 4 {
		t.Errorf("pool tier = %d, want 4", got)
	}

	orphan := newCellControllerFixture("ns-a", "cell-c", nil, 0, map[string]string{cells.LabelCellPool: "gone"})
	if got := srv.resolveCellCap(ctx, "", orphan); got != defaultCellMaxTenants {
		t.Errorf("a gone pool must fall through to the compiled tier: got %d, want %d", got, defaultCellMaxTenants)
	}

	noCap := newCellControllerFixture("ns-a", "cell-d", nil, 0, nil)
	if got := srv.resolveCellCap(ctx, "", noCap); got != defaultCellMaxTenants {
		t.Errorf("compiled tier = %d, want %d", got, defaultCellMaxTenants)
	}

	srv.deps.CellMaxTenants = 7
	if got := srv.resolveCellCap(ctx, "", noCap); got != 7 {
		t.Errorf("chart tier = %d, want 7", got)
	}
	if got := srv.resolveCellCap(ctx, "", pooled); got != 4 {
		t.Errorf("pool tier must win over the chart tier: got %d, want 4", got)
	}
	if got := srv.resolveCellCap(ctx, "", withCap); got != 3 {
		t.Errorf("cell tier must win over the chart tier: got %d, want 3", got)
	}

	if got := cellFreeSlots(2, 5); got != 0 {
		t.Errorf("freeSlots = %d, want 0: the value never falls below zero", got)
	}
	if got := cellFreeSlots(5, 2); got != 3 {
		t.Errorf("freeSlots = %d, want 3", got)
	}
}

// --------------------------------------------------------------------------
// The cell grants read (add-operator-cell-views)
// --------------------------------------------------------------------------

// cellGrantsRoles returns the roles the grants tests bind. "cell-operator"
// holds both gates; "cell-viewer-only" holds the controller read alone.
func cellGrantsRoles() []*v1alpha1.VarroaRole {
	return []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "cell-operator"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				{Resources: []string{"rolebindings"}, Verbs: []string{"read"}},
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cell-viewer-only"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-developer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}}},
		}},
	}
}

var (
	cellOperatorClaims = &auth.Claims{Subject: "cell-operator-user"}
	cellViewerClaims   = &auth.Claims{Subject: "cell-viewer-user"}
)

// cellGrantsBindings returns the two gate bindings plus extra.
func cellGrantsBindings(extra ...*v1alpha1.VarroaRoleBinding) []*v1alpha1.VarroaRoleBinding {
	out := make([]*v1alpha1.VarroaRoleBinding, 0, 2+len(extra))
	out = append(out,
		&v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "aaa-gate-operator"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-operator-user"}},
			RoleRef:  "cell-operator",
		}},
		&v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "aab-gate-viewer"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-viewer-user"}},
			RoleRef:  "cell-viewer-only",
		}},
	)
	return append(out, extra...)
}

// cellGrantsServer wires a server whose authorizer resolves the given
// bindings, over the two-cell fixture client.
func cellGrantsServer(bindings []*v1alpha1.VarroaRoleBinding) *Server {
	client := twoCellClient()
	authorizer := NewAuthorizer(testResolver(cellGrantsRoles(), bindings), false)
	return cellsTestServer(client, authorizer, newFakeCellReader())
}

func decodeCellGrants(t *testing.T, rec *httptest.ResponseRecorder) CellGrants {
	t.Helper()
	var out CellGrants
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode cell grants: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func TestCellGrantsAnswersEachTenant(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-release"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "acme-devs"}},
			RoleRef:  "pipelines-developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "acme", Jobs: []string{"release/*"}}},
			},
		},
	}))

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeCellGrants(t, rec)
	if body.Cell != "cell-a" || body.Namespace != "ns-a" {
		t.Errorf("cell = %q/%q", body.Namespace, body.Cell)
	}
	if len(body.Tenants) != 2 {
		t.Fatalf("expected 2 tenant entries, got %d: %+v", len(body.Tenants), body.Tenants)
	}
	acme := body.Tenants[0]
	if acme.Tenant != "acme" {
		t.Fatalf("first entry = %q, want acme", acme.Tenant)
	}
	if acme.ItemRole != "varroa:tenant:acme" || acme.ItemPattern != "^acme($|/.*)$" {
		t.Errorf("item role = %q, pattern = %q", acme.ItemRole, acme.ItemPattern)
	}
	if acme.Truncated {
		t.Error("a short list reported truncation")
	}
	// The two gate bindings are cluster wide, so they reach every tenant
	// too. The narrowing grant comes first.
	if len(acme.Grants) != 3 {
		t.Fatalf("expected 3 grants, got %d: %+v", len(acme.Grants), acme.Grants)
	}
	g := acme.Grants[0]
	if g.Source != "tenantScopedBinding" || g.Binding != "acme-release" {
		t.Errorf("grant = %+v", g)
	}
	if len(g.Jobs) != 1 || g.Jobs[0] != "release/*" {
		t.Errorf("jobs = %v", g.Jobs)
	}
	if len(g.Subjects) != 1 || g.Subjects[0].Name != "acme-devs" {
		t.Errorf("subjects = %+v", g.Subjects)
	}
	if len(g.Verbs) != 2 {
		t.Errorf("verbs = %v, want read and write", g.Verbs)
	}
	// The second tenant is reached by nothing, so its list is empty and
	// its block still names the generated item role.
	beta := body.Tenants[1]
	if beta.Tenant != "beta" {
		t.Errorf("second entry = %q, want beta", beta.Tenant)
	}
	for _, bg := range beta.Grants {
		if bg.Source != "clusterWideBinding" {
			t.Errorf("the scoped binding reached beta: %+v", bg)
		}
	}
	if beta.ItemRole != "varroa:tenant:beta" {
		t.Errorf("beta item role = %q", beta.ItemRole)
	}
	// Acme has one narrowed write grant, so one member role holds its subject.
	if len(acme.MemberRoles) != 1 {
		t.Fatalf("acme member roles = %+v, want one narrowed role", acme.MemberRoles)
	}
	mr := acme.MemberRoles[0]
	if mr.Name != "varroa:tenant-member:acme:acme-release" || mr.Binding != "acme-release" {
		t.Errorf("member role = %+v", mr)
	}
	if want := rbac.TenantMemberNarrowedPattern("acme", []string{"release/*"}); mr.Pattern != want {
		t.Errorf("member role pattern = %q, want %q", mr.Pattern, want)
	}
	if len(mr.Subjects) != 1 || mr.Subjects[0] != (CellGrantSubject{Kind: "Group", Name: "acme-devs"}) {
		t.Errorf("member role subjects = %+v", mr.Subjects)
	}
	if len(mr.Permissions) == 0 {
		t.Error("member role carries no permissions")
	}
	if beta.MemberRoles == nil || len(beta.MemberRoles) != 0 {
		t.Errorf("beta member roles = %+v, want an empty list", beta.MemberRoles)
	}
}

func TestCellGrantsMemberRolesNeverNull(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings())
	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), `"memberRoles":[]`); n != 2 {
		t.Errorf("expected two empty memberRoles arrays, got %d in %s", n, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"memberRoles":null`) {
		t.Errorf("memberRoles must never be null: %s", rec.Body.String())
	}
}

func TestCellGrantsMemberRolesNotCapped(t *testing.T) {
	var extra []*v1alpha1.VarroaRoleBinding
	for i := 0; i < rbac.MaxTenantGrants+10; i++ {
		extra = append(extra, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wide-%03d", i)},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: fmt.Sprintf("tenant-%03d", i)}},
				RoleRef:  "pipelines-developer",
			},
		})
	}
	srv := cellGrantsServer(cellGrantsBindings(extra...))
	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	body := decodeCellGrants(t, rec)
	entry := body.Tenants[0]
	if !entry.Truncated {
		t.Error("expected truncated true")
	}
	if len(entry.Grants) != rbac.MaxTenantGrants {
		t.Errorf("grants = %d, want %d", len(entry.Grants), rbac.MaxTenantGrants)
	}
	var subtree *CellMemberRole
	for i := range entry.MemberRoles {
		if entry.MemberRoles[i].Binding == "" {
			subtree = &entry.MemberRoles[i]
		}
	}
	if subtree == nil {
		t.Fatalf("no subtree member role in %+v", entry.MemberRoles)
	}
	if len(subtree.Subjects) != rbac.MaxTenantGrants+10 {
		t.Errorf("subtree member role holds %d subjects, want %d", len(subtree.Subjects), rbac.MaxTenantGrants+10)
	}
}

func TestCellGrantsOmitsJobsForAnUnnarrowedGrant(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "zzz-fleet-wide"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
			RoleRef:  "pipelines-developer",
		},
	}))

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// An absent jobs field and an empty jobs field mean different things,
	// so the wire shape must carry no key at all.
	var raw struct {
		Tenants []struct {
			Grants []map[string]json.RawMessage `json:"grants"`
		} `json:"tenants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Tenants) == 0 || len(raw.Tenants[0].Grants) == 0 {
		t.Fatalf("no grant in the body: %s", rec.Body.String())
	}
	for _, grant := range raw.Tenants[0].Grants {
		if _, ok := grant["jobs"]; ok {
			t.Errorf("an unnarrowed grant carries a jobs key: %v", grant)
		}
	}
}

func TestCellGrantsOrdersNarrowingGrantsFirst(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings(
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "zzz-fleet-wide"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
				RoleRef:  "pipelines-developer",
			},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "mmm-acme-release"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "acme-devs"}},
				RoleRef:  "pipelines-developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "acme", Jobs: []string{"release/*"}}},
				},
			},
		},
	))

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	body := decodeCellGrants(t, rec)
	grants := body.Tenants[0].Grants
	if len(grants) != 4 {
		t.Fatalf("expected 4 grants, got %d: %+v", len(grants), grants)
	}
	if grants[0].Source != "tenantScopedBinding" {
		t.Fatalf("first source = %q, want tenantScopedBinding", grants[0].Source)
	}
	for _, g := range grants[1:] {
		if g.Source != "clusterWideBinding" {
			t.Errorf("source = %q, want clusterWideBinding", g.Source)
		}
	}
	// The three cluster-wide grants sort by binding name.
	if grants[1].Binding != "aaa-gate-operator" || grants[3].Binding != "zzz-fleet-wide" {
		t.Errorf("binding order = %q .. %q", grants[1].Binding, grants[3].Binding)
	}
}

func TestCellGrantsReportsTruncation(t *testing.T) {
	var extra []*v1alpha1.VarroaRoleBinding
	for i := 0; i < rbac.MaxTenantGrants+2; i++ {
		extra = append(extra, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wide-%03d", i)},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
				RoleRef:  "pipelines-developer",
			},
		})
	}
	srv := cellGrantsServer(cellGrantsBindings(extra...))

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	body := decodeCellGrants(t, rec)
	entry := body.Tenants[0]
	if !entry.Truncated {
		t.Error("expected truncated true")
	}
	if len(entry.Grants) != rbac.MaxTenantGrants {
		t.Errorf("grants = %d, want %d", len(entry.Grants), rbac.MaxTenantGrants)
	}
}

func TestCellGrantsRefusesAnUnreadableCell(t *testing.T) {
	// The gate bindings are cluster wide, so this test narrows the
	// controller read to one namespace instead.
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), newFakeCellReader())

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-b/cell-b/grants",
		map[string]string{"namespace": "ns-b", "name": "cell-b"}, cellUserClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCellGrantsRefusesAnOrdinaryController(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings())

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/plain/grants",
		map[string]string{"namespace": "ns-a", "name": "plain"}, cellOperatorClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCellGrantsRefusesAMissingController(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings())

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/absent/grants",
		map[string]string{"namespace": "ns-a", "name": "absent"}, cellOperatorClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCellGrantsRefusesACallerWithoutRoleBindingRead(t *testing.T) {
	srv := cellGrantsServer(cellGrantsBindings())

	// The caller passes the controller read check, so a 404 would hide
	// nothing. It holds no cluster-wide rolebindings:read.
	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellViewerClaims)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCellGrantsDeniesWithoutAuthorizer(t *testing.T) {
	srv := cellsTestServer(twoCellClient(), nil, newFakeCellReader())

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCellGrantsNeedsNoRecordStore(t *testing.T) {
	client := twoCellClient()
	authorizer := NewAuthorizer(testResolver(cellGrantsRoles(), cellGrantsBindings()), false)
	srv := cellsTestServer(client, authorizer, nil)

	// The read is resolver backed, so a disabled record store answers no
	// 503.
	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCellUsageCarriesAgentNamespaceAndPlacementGeneration asserts a
// member cell's usage read carries each tenant's agent namespace and
// placement generation from the record store, and omits an agent
// namespace no announcement named.
func TestCellUsageCarriesAgentNamespaceAndPlacementGeneration(t *testing.T) {
	reader := newFakeCellReader()
	reader.usage["member/ns-a/cell-a"] = recordstore.CellUsage{
		Cluster: "member", Namespace: "ns-a", Cell: "cell-a",
		Tenants: []recordstore.CellTenantUsage{
			{Tenant: "acme", Role: "slot 0", AgentNamespace: "acme-agents", PlacementGeneration: 4},
			{Tenant: "beta", Role: "slot 0", PlacementGeneration: 2},
		},
	}
	srv := cellsTestServer(twoCellClient(), cellReadAuthorizer("ns-a"), reader)
	rec := doCellRequest(t, srv.handleCellUsage, "/cells/ns-a/cell-a/usage?cluster=member",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellUserClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	usage := decodeCellUsage(t, rec)
	if len(usage.Tenants) != 2 {
		t.Fatalf("tenants = %+v, want two rows", usage.Tenants)
	}
	acme, beta := usage.Tenants[0], usage.Tenants[1]
	if acme.AgentNamespace != "acme-agents" || acme.PlacementGeneration != 4 {
		t.Errorf("acme = %+v, want acme-agents at generation 4", acme)
	}
	if beta.AgentNamespace != "" || beta.PlacementGeneration != 2 {
		t.Errorf("beta = %+v, want no agent namespace at generation 2", beta)
	}

	var raw struct {
		Tenants []map[string]json.RawMessage `json:"tenants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, present := raw.Tenants[1]["agentNamespace"]; present {
		t.Errorf("beta carries agentNamespace, want the field absent: %s", rec.Body.String())
	}
	if _, present := raw.Tenants[1]["placementGeneration"]; !present {
		t.Errorf("beta carries no placementGeneration: %s", rec.Body.String())
	}
}
