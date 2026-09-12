package mcp

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// list_activity must read through deps.Backfill — the source the REST
// handler uses. Backfill is nil when the record store is absent, so this
// tool returns an empty list rather than reading any other dependency.

// fakeSliceBackfill is a minimal activity.Backfill over a fixed slice, for
// tests that only need the filter/predicate contract, not real paging.
type fakeSliceBackfill struct{ events []activity.Event }

func (f *fakeSliceBackfill) Recent(_ context.Context, _ string, limit int) ([]activity.Event, error) {
	if limit <= 0 || limit > len(f.events) {
		limit = len(f.events)
	}
	return f.events[:limit], nil
}

func (f *fakeSliceBackfill) Retention() (string, int) { return "on", 30 }

func (f *fakeSliceBackfill) Query(_ context.Context, q activity.Query) (activity.Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	out := make([]activity.Event, 0, len(f.events))
	for _, e := range f.events {
		if q.Authorize != nil && !q.Authorize(e) {
			continue
		}
		if !activity.Matches(e, q) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return activity.Page{Items: out, RetentionMode: "on", RetentionDays: 30}, nil
}

func backfillDeps(events ...activity.Event) *api.Dependencies {
	deps := newTestDeps()
	deps.Backfill = &fakeSliceBackfill{events: events}
	deps.Authorizer = guardAuthorizer()
	return deps
}

func callListActivity(t *testing.T, deps *api.Dependencies, args map[string]interface{}) []activity.Event {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_activity",
		"arguments": args,
	}, guardClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_activity returned error: %v", tr.Content)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("list_activity returned no content")
	}
	var out struct {
		Count int              `json:"count"`
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &out); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	return out.Items
}

func TestListActivityReadsBackfill(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa", Message: "created"},
		activity.Event{Type: "controller.deleted", Controller: "other", Namespace: "varroa", Message: "deleted"},
	)
	events := callListActivity(t, deps, map[string]interface{}{})
	if len(events) != 2 {
		t.Fatalf("expected 2 events from backfill, got %d", len(events))
	}
}

func TestListActivityControllerFilter(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa", Message: "created"},
		activity.Event{Type: "controller.deleted", Controller: "other", Namespace: "varroa", Message: "deleted"},
	)
	events := callListActivity(t, deps, map[string]interface{}{"controller": "auditboi"})
	if len(events) != 1 || events[0].Controller != "auditboi" {
		t.Fatalf("expected only auditboi events, got %+v", events)
	}
}

func TestListActivityRequiresClaimsWhenBackfillPresent(t *testing.T) {
	deps := backfillDeps(
		activity.Event{Type: "controller.created", Controller: "auditboi", Namespace: "varroa"},
	)
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_activity",
		"arguments": map[string]interface{}{},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected authentication error without claims, got %v", tr.Content)
	}
}

// searchTestBrood is a minimal single-cluster api.Brood backed directly by
// a crdstore.Fake, so the search tool's shared api.Server.Search can list
// controllers without a real multi-cluster Brood.
type searchTestBrood struct {
	api.Brood
	store   *crdstore.Fake
	cluster string
}

func (b *searchTestBrood) LocalCluster() string { return b.cluster }

func (b *searchTestBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]api.ClusterController, []api.ClusterFanoutStatus, error) {
	items, err := crdstore.List[v1alpha1.Controller](ctx, b.store, ns, "")
	if err != nil {
		return nil, []api.ClusterFanoutStatus{{Name: b.cluster, OK: false, Error: err.Error()}}, err
	}
	cc := make([]api.ClusterController, len(items))
	for i, cr := range items {
		cc[i] = api.ClusterController{Cluster: b.cluster, CR: cr}
	}
	return cc, []api.ClusterFanoutStatus{{Name: b.cluster, OK: true}}, nil
}

func TestSearchMatchesControllersBySubstring(t *testing.T) {
	deps := newTestDeps()
	fake := deps.Store.(*crdstore.Fake)
	if err := fake.Seed(
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "auditboi", Namespace: "varroa"}},
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "testcore", Namespace: "varroa"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deps.Authorizer = guardAuthorizer()
	deps.Brood = &searchTestBrood{store: fake, cluster: "core"}
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "audit", "namespace": "varroa"},
	}, guardClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	var page struct {
		Count int                      `json:"count"`
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &page); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0]["name"] != "auditboi" || page.Items[0]["type"] != "controller" {
		t.Fatalf("expected exactly one controller result named auditboi, got %+v", page.Items)
	}

	resp = mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "no-such-controller", "namespace": "varroa"},
	}, guardClaims)
	tr = parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &page); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected no hits, got %+v", page.Items)
	}
}

// With an Authorizer configured, search must refuse unauthenticated callers —
// treating missing claims as "no filter" would let them enumerate controller
// names across the brood.
func TestSearchRequiresClaimsWithAuthorizer(t *testing.T) {
	deps := newTestDeps()
	deps.Authorizer = guardAuthorizer()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "anything"},
	}, nil)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected authentication error without claims, got %v", tr.Content)
	}
}

// scopedSearchReader is a fake recordstore.Reader that filters its fixed
// rows by the received SearchQuery's Scope and Namespace, the way the
// Postgres reader pushes both into its WHERE clause (Namespace as an
// AND'd exact match on top of the Scope/Tenants admission clause), and
// records the last SearchQuery it received. A test uses it to prove a
// namespace filter reaches the query instead of running as a post-filter
// on the response.
type scopedSearchReader struct {
	recordstore.Reader
	jobs      []recordstore.JobSearchResult
	runs      []recordstore.RunSearchResult
	activity  []activity.Event
	lastQuery recordstore.SearchQuery
}

func (s *scopedSearchReader) SearchRecords(_ context.Context, q recordstore.SearchQuery) (recordstore.SearchResults, error) {
	s.lastQuery = q
	var out recordstore.SearchResults
	for _, j := range s.jobs {
		if q.Namespace != "" && j.Namespace != q.Namespace {
			continue
		}
		if jobRunNamespaceAdmits(q, j.Namespace) {
			out.Jobs = append(out.Jobs, j)
		}
	}
	for _, r := range s.runs {
		if q.Namespace != "" && r.Namespace != q.Namespace {
			continue
		}
		if jobRunNamespaceAdmits(q, r.Namespace) {
			out.Runs = append(out.Runs, r)
		}
	}
	for _, e := range s.activity {
		if q.Namespace != "" && e.Namespace != q.Namespace {
			continue
		}
		if q.Scope.Admits(e) {
			out.Activity = append(out.Activity, e)
		}
	}
	return out, nil
}

// jobRunNamespaceAdmits mirrors jobRunScopeClause's SQL fragment for the
// "job" and "run" kinds: q.AllTenants admits every namespace, otherwise
// only a namespace in q.JobRunNamespaces admits (this fake seeds no
// tenanted row, so q.Tenants never needs a check here).
func jobRunNamespaceAdmits(q recordstore.SearchQuery, ns string) bool {
	if q.AllTenants {
		return true
	}
	for _, n := range q.JobRunNamespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// TestSearchNamespaceFilterNarrowsEveryRecordKind proves the namespace
// filter narrows the record-backed job, run, and activity kinds, the
// same way it already narrows the controller kind — and that the
// narrowing reaches the record-store query rather than running as a
// post-filter on the response: five job rows in another namespace that
// would outrank a caller's single matching row must not push that row
// out of the five-per-kind response cap.
func TestSearchNamespaceFilterNarrowsEveryRecordKind(t *testing.T) {
	deps := newTestDeps()
	deps.Authorizer = guardAuthorizer()
	deps.Brood = &searchTestBrood{store: deps.Store.(*crdstore.Fake), cluster: "core"}
	reader := &scopedSearchReader{
		jobs: []recordstore.JobSearchResult{
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build-1"},
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build-2"},
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build-3"},
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build-4"},
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build-5"},
			{Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a", JobPath: "ns-a/build"},
		},
		runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a", JobPath: "ns-a/build"},
			{RunID: "r-b", Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build"},
		},
		activity: []activity.Event{
			{Cluster: "core", Namespace: "ns-a", Controller: "cell-a", Message: "a-event"},
			{Cluster: "core", Namespace: "ns-b", Controller: "cell-b", Message: "b-event"},
		},
	}
	deps.RecordStore = reader
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "search",
		"arguments": map[string]interface{}{"query": "anything", "namespace": "ns-a"},
	}, guardClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
	var page struct {
		Items []api.SearchResult `json:"items"`
	}
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &page); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}

	byType := map[string][]api.SearchResult{}
	for _, r := range page.Items {
		byType[r.Type] = append(byType[r.Type], r)
	}
	if len(byType["job"]) != 1 || byType["job"][0].Namespace != "ns-a" {
		t.Fatalf("jobs = %+v, want the single ns-a row, not pushed out by the five ns-b rows", byType["job"])
	}
	if len(byType["run"]) != 1 || byType["run"][0].Namespace != "ns-a" {
		t.Fatalf("runs = %+v, want one row in ns-a only", byType["run"])
	}
	if len(byType["activity"]) != 1 || byType["activity"][0].Namespace != "ns-a" {
		t.Fatalf("activity = %+v, want one row in ns-a only", byType["activity"])
	}

	// The namespace filter must have reached the reader's query, not
	// just the response, so the record store applies it in SQL rather
	// than fetching every readable namespace and dropping the rest
	// after the per-kind fetch cap.
	if got := reader.lastQuery.Namespace; got != "ns-a" {
		t.Fatalf("reader.lastQuery.Namespace = %q, want %q", got, "ns-a")
	}
}
