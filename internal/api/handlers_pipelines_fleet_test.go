package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fleetServer builds a Server with the given local Tenants and record-store
// reader, for the fleet run routes.
func fleetServer(t *testing.T, authorizer *Authorizer, reader recordstore.Reader, tenants ...*v1alpha1.Tenant) *Server {
	t.Helper()
	store := crdstore.NewFake()
	for _, tenant := range tenants {
		crdstore.MustSeed(store, tenant)
	}
	client := newFakeResourceClient()
	return NewServer(&Dependencies{
		Store:       store,
		Client:      client,
		Brood:       newFakeBrood(client),
		Authorizer:  authorizer,
		RecordStore: reader,
		Logger:      slog.Default(),
	})
}

// getFleetRuns serves GET /pipelines/runs with the given raw query.
func getFleetRuns(t *testing.T, srv *Server, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/pipelines/runs?"+rawQuery, nil)
	req = req.WithContext(contextWithClaims(req.Context(), resolverClaims))
	rec := httptest.NewRecorder()
	srv.handleListFleetRuns(rec, req)
	return rec
}

// decodeFleetPage decodes a 200 body and fails on any other status.
func decodeFleetPage(t *testing.T, rec *httptest.ResponseRecorder) FleetRunPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var page FleetRunPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

// fleetRun builds a stored run of tenant pay with an optional payload.
func fleetRun(job string, payload string) recordstore.Run {
	result := "SUCCESS"
	run := recordstore.Run{
		RunID: uuid.New().String(), Cluster: "core", Namespace: "cell-ns", Cell: "c1",
		Tenant: "pay", JobPath: job, Status: "COMPLETED", Result: &result,
		CreatedAt: time.Now().UTC(),
	}
	if payload != "" {
		run.Payload = json.RawMessage(payload)
	}
	return run
}

// cronPayload is a run payload that the trigger map classifies as cron.
const cronPayload = `{"run.started":{"payload":{"cause_types":["TimerTriggerCause"]}}}`

func TestFleetRunsUnreadableTenantFilterAnswersNothing(t *testing.T) {
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{fleetRun("pay/a", "")}}}}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"),
		pipelinesTenant("secret", true, "core", "cell-ns", "c2"))
	page := decodeFleetPage(t, getFleetRuns(t, srv, "tenant=secret"))
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("items = %v; want an empty list", page.Items)
	}
	if len(reader.acrossQueries) != 0 {
		t.Fatalf("store queries = %d; want none", len(reader.acrossQueries))
	}
}

func TestFleetRunsPushesJobPatterns(t *testing.T) {
	reader := &fakeRecordReader{}
	srv := fleetServer(t, pipelineJobGrantAuthorizer([]string{"read"}, "release/*"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	decodeFleetPage(t, getFleetRuns(t, srv, ""))
	if len(reader.acrossQueries) != 1 {
		t.Fatalf("store queries = %d; want 1", len(reader.acrossQueries))
	}
	scopes := reader.acrossQueries[0].Scopes
	if len(scopes) != 1 || scopes[0].Cluster != "core" || scopes[0].Tenant != "pay" ||
		len(scopes[0].JobPatterns) != 1 || scopes[0].JobPatterns[0] != "^pay/(?:release/[^/]*)$" {
		t.Fatalf("scopes = %+v; want core/pay with the release pattern", scopes)
	}
}

func TestFleetRunsTriggerRefillBudget(t *testing.T) {
	var pages []recordstore.RunPage
	for i := 0; i < 6; i++ {
		pages = append(pages, recordstore.RunPage{
			Runs:       []recordstore.Run{fleetRun("pay/a", ""), fleetRun("pay/b", "")},
			NextCursor: "more" + string(rune('a'+i)),
		})
	}
	reader := &fakeRecordReader{acrossPages: pages}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	page := decodeFleetPage(t, getFleetRuns(t, srv, "trigger=cron&limit=2"))
	if len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("page = %+v; want no items and a cursor", page)
	}
	if len(reader.acrossQueries) != maxTriggerRefillPages {
		t.Fatalf("store queries = %d; want %d", len(reader.acrossQueries), maxTriggerRefillPages)
	}
}

func TestFleetRunsTriggerRefillCursorAfterLastReturned(t *testing.T) {
	cronA := fleetRun("pay/a", cronPayload)
	manualB := fleetRun("pay/b", "")
	cronC := fleetRun("pay/c", cronPayload)
	cronD := fleetRun("pay/d", cronPayload)
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{
		{Runs: []recordstore.Run{cronA, manualB}, NextCursor: "p1"},
		{Runs: []recordstore.Run{cronC, cronD}, NextCursor: "p2"},
	}}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	page := decodeFleetPage(t, getFleetRuns(t, srv, "trigger=cron&limit=2"))
	if len(page.Items) != 2 || page.Items[0].RunID != cronA.RunID || page.Items[1].RunID != cronC.RunID {
		t.Fatalf("items = %+v; want the cron runs A and C", page.Items)
	}
	want := recordstore.EncodeRunsAcrossCursor(recordstore.RunSortStarted, false, cronC)
	if page.NextCursor != want {
		t.Fatalf("nextCursor = %q; want the cursor after C", page.NextCursor)
	}
}

func TestFleetRunsBadParams(t *testing.T) {
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), &fakeRecordReader{},
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	for _, raw := range []string{"sort=size", "order=up", "status=DONE", "trigger=bot", "since=yesterday", "limit=0"} {
		rec := getFleetRuns(t, srv, raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400", raw, rec.Code)
		}
	}
}

func TestFleetRunsInvalidCursor(t *testing.T) {
	reader := &fakeRecordReader{acrossErr: recordstore.ErrInvalidCursor}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	rec := getFleetRuns(t, srv, "cursor=bogus")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid cursor") {
		t.Fatalf("status = %d body %s; want 400 invalid cursor", rec.Code, rec.Body.String())
	}
}

func TestFleetRunsCanWrite(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "writer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"write"}}}}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "read"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}}, RoleRef: "reader",
			Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "pay"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "write"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}}, RoleRef: "writer",
			Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "pay", Jobs: []string{"deploy"}}}}}},
	}
	authorizer := NewAuthorizer(testResolver(roles, bindings), false)
	deploy := fleetRun("pay/deploy", "")
	test := fleetRun("pay/test", "")
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{deploy, test}}}}
	srv := fleetServer(t, authorizer, reader, pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	page := decodeFleetPage(t, getFleetRuns(t, srv, ""))
	writable := map[string]bool{}
	for _, row := range page.Items {
		writable[row.JobPath] = row.CanWrite
	}
	if !writable["pay/deploy"] || writable["pay/test"] {
		t.Fatalf("canWrite = %v; want pay/deploy true and pay/test false", writable)
	}
}

func TestFleetRunsNoRecordStore(t *testing.T) {
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), nil,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	rec := getFleetRuns(t, srv, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
}
