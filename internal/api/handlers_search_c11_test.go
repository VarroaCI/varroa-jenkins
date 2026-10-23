package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// namespaceScopedAuthorizer returns an Authorizer that grants
// controllers:read on namespace "ns-a", matching the shape D5's Scope
// pushdown gates on, plus pipelines:read on tenant "ns-a", for the
// record-row search tests below.
func namespaceScopedAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ns-user"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-a-pipelines"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ns-user"}},
			RoleRef:  "pipelines-viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "ns-a"}}},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

var nsScopedClaims = &auth.Claims{Subject: "ns-user"}

// syntheticTenantAuthorizer grants pipelines:read namespace-scoped on
// "ns-a", the shape that authorizes a synthetic (single-controller) tenant
// such as "ns-a/cell-a": effectiveControllerTenantGrants admits a
// namespace-scoped binding, unlike effectiveTenantTenantGrants' tenant-scoped
// path that namespaceScopedAuthorizer uses for the Tenant-kind tests above.
func syntheticTenantAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-a-pipelines-ns-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ns-user"}},
			RoleRef:  "pipelines-viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// --- 11.4: link shapes ---

func TestSearchLinkShapes(t *testing.T) {
	// searchJobLink and searchRunLink take the already-resolved tenant
	// segment, not the raw tenant column; this asserts only their
	// percent-encoding. TestSearchRecords_LinkUsesResolvedTenantSegment
	// below asserts that searchRecords supplies the resolved tenant.
	if got, want := searchJobLink("tenant one", "svc/main"), "/pipelines/tenant%20one/svc%2Fmain"; got != want {
		t.Fatalf("job link = %q, want %q", got, want)
	}
	if got, want := searchRunLink("tenant one", "svc/main", "r 1"), "/pipelines/tenant%20one/svc%2Fmain/r%201"; got != want {
		t.Fatalf("run link = %q, want %q", got, want)
	}
	got := searchActivityLink(activity.Event{Cluster: "core", Namespace: "ns-a", Controller: "ctrl-a", Type: "controller.created", Actor: "nb"})
	want := "/activity?actor=nb&cluster=core&controller=ns-a%2Fctrl-a&type=controller.created"
	if got != want {
		t.Fatalf("activity link = %q, want %q", got, want)
	}
	// A global event (no controller) filters by namespace instead.
	got = searchActivityLink(activity.Event{Cluster: "core", Namespace: "", Controller: "", Type: "settings.updated"})
	want = "/activity?cluster=core&type=settings.updated"
	if got != want {
		t.Fatalf("global activity link = %q, want %q", got, want)
	}
}

// --- 11.4: the four live kinds keep their positions ---

func TestRankSearchResultsKeepsLiveKindPositionsAfterRecordKinds(t *testing.T) {
	items := []SearchResult{
		{Type: "activity", Name: "query"},
		{Type: "run", Name: "query"},
		{Type: "job", Name: "query"},
		{Type: "catalogitem", Name: "query"},
		{Type: "group", Name: "query"},
		{Type: "namespace", Name: "query"},
		{Type: "controller", Name: "query"},
	}
	got := rankSearchResults(items, "query")
	wantOrder := []string{"controller", "namespace", "group", "catalogitem", "job", "run", "activity"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d results, want %d", len(got), len(wantOrder))
	}
	for i, kind := range wantOrder {
		if got[i].Type != kind {
			t.Fatalf("position %d = %q, want %q (full order: %v)", i, got[i].Type, kind, got)
		}
	}
}

// --- 11.5: record-store failure is logged and omitted, not 503 ---

// erroringSearchReader implements recordstore.Reader by embedding a nil
// interface and overriding only SearchRecords, which every test below
// needs; every other method panics if called, catching an accidental
// dependency on one this test does not expect.
type erroringSearchReader struct{ recordstore.Reader }

func (erroringSearchReader) SearchRecords(ctx context.Context, q recordstore.SearchQuery) (recordstore.SearchResults, error) {
	return recordstore.SearchResults{}, errors.New("record store unavailable")
}

// erroringBrood implements Brood by embedding a nil interface and
// overriding only ListAll, for the total-source-failure test.
type erroringBrood struct{ Brood }

func (erroringBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return nil, nil, errors.New("brood fan-out unavailable")
}

func TestSearch_RecordStoreFailureIsLoggedNotUnavailable(t *testing.T) {
	server := NewServer(&Dependencies{
		Logger:      slog.Default(),
		Authorizer:  namespaceScopedAuthorizer(),
		Brood:       &stubBrood{localCluster: "core"},
		RecordStore: erroringSearchReader{},
	})
	resp, err := server.Search(context.Background(), nsScopedClaims, "query", "")
	if err != nil {
		t.Fatalf("Search returned %v, want nil (a record-store failure must not be errSearchUnavailable)", err)
	}
	for _, r := range resp.Items {
		if r.Type == "job" || r.Type == "run" || r.Type == "activity" {
			t.Fatalf("got a %q result after a record-store failure, want the three kinds omitted", r.Type)
		}
	}
}

func TestSearch_TotalBroodFailureIsUnavailable(t *testing.T) {
	server := NewServer(&Dependencies{
		Logger:     slog.Default(),
		Authorizer: namespaceScopedAuthorizer(),
		Brood:      erroringBrood{},
	})
	_, err := server.Search(context.Background(), nsScopedClaims, "query", "")
	if !errors.Is(err, errSearchUnavailable) {
		t.Fatalf("Search error = %v, want errSearchUnavailable", err)
	}
}

// --- 11.3/11.3b: authorization runs per row before a record result is appended ---

// fixedSearchReader returns a fixed SearchResults for every query, so a
// test can assert searchRecords' authorization and link-building without
// a live database. lastQuery records the SearchQuery the handler built,
// for a test that asserts what reached the reader (e.g. Tenants), rather
// than only what the reader returned.
type fixedSearchReader struct {
	recordstore.Reader
	results   recordstore.SearchResults
	lastQuery recordstore.SearchQuery
}

func (f *fixedSearchReader) SearchRecords(ctx context.Context, q recordstore.SearchQuery) (recordstore.SearchResults, error) {
	f.lastQuery = q
	return f.results, nil
}

func TestSearchRecords_AppliesAuthorizationPerRow(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Jobs: []recordstore.JobSearchResult{
			{Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a", JobPath: "ns-a/build"},
			{Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build"},
		},
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a", JobPath: "ns-a/build"},
			{RunID: "r-b", Cluster: "core", Namespace: "ns-b", Cell: "cell-b", Tenant: "ns-b", JobPath: "ns-b/build"},
		},
		Activity: []activity.Event{
			{Cluster: "core", Namespace: "ns-a", Controller: "cell-a", Message: "readable"},
			{Cluster: "core", Namespace: "", Controller: "", Message: "global row"},
		},
	}}
	server := NewServer(&Dependencies{
		Logger:      slog.Default(),
		Authorizer:  namespaceScopedAuthorizer(),
		RecordStore: reader,
	})

	got := server.searchRecords(context.Background(), nsScopedClaims, "query", "")

	byType := map[string][]SearchResult{}
	for _, r := range got {
		byType[r.Type] = append(byType[r.Type], r)
	}
	if len(byType["job"]) != 1 || byType["job"][0].Namespace != "ns-a" {
		t.Fatalf("jobs = %+v, want one row in ns-a only", byType["job"])
	}
	if len(byType["run"]) != 1 || byType["run"][0].Namespace != "ns-a" {
		t.Fatalf("runs = %+v, want one row in ns-a only", byType["run"])
	}
	if len(byType["activity"]) != 1 || byType["activity"][0].Name != "readable" {
		t.Fatalf("activity = %+v, want the namespace-scoped row only, not the global row", byType["activity"])
	}
}

// --- 8.12: a job row and a run row carry the resolved tenant segment, not
// the raw tenant column ---

func TestSearchRecords_LinkUsesResolvedTenantSegment(t *testing.T) {
	// Tenant "ns-a/cell-a" is a synthetic (single-controller) tenant.
	// TenantRefFromRow parses it as TenantController, whose tenant segment
	// is the controller name "cell-a", not the raw tenant string.
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Jobs: []recordstore.JobSearchResult{
			{Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a/cell-a", JobPath: "build"},
		},
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a/cell-a", JobPath: "build"},
		},
	}}
	server := NewServer(&Dependencies{
		Logger:      slog.Default(),
		Authorizer:  syntheticTenantAuthorizer(),
		RecordStore: reader,
	})

	got := server.searchRecords(context.Background(), nsScopedClaims, "query", "")

	var jobLink, runLink string
	for _, r := range got {
		switch r.Type {
		case "job":
			jobLink = r.Link
		case "run":
			runLink = r.Link
		}
	}
	if want := "/pipelines/cell-a/build"; jobLink != want {
		t.Fatalf("job link = %q, want %q", jobLink, want)
	}
	if want := "/pipelines/cell-a/build/r-a"; runLink != want {
		t.Fatalf("run link = %q, want %q", runLink, want)
	}
}
