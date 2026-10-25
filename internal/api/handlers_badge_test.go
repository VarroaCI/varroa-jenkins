package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

const badgeTestToken = "0123456789abcdef0123456789abcdef"

// badgeTenant builds the typed Tenant "payments" for the badge tests. An empty
// cell leaves the tenant unplaced.
func badgeTenant(cell string, badge *v1alpha1.TenantBadgeSpec, token string) *v1alpha1.Tenant {
	tm := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"test-ns"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "p",
				AgentNamespace: "test-ns",
				Badge:          badge,
			},
		},
		Status: v1alpha1.TenantStatus{
			Pipelines: &v1alpha1.TenantPipelinesStatus{BadgeToken: token},
		},
	}
	if cell != "" {
		tm.Status.Placement = &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: cell, CellNamespace: "test-ns"}
	}
	return tm
}

func badgeOn() *v1alpha1.TenantBadgeSpec { return &v1alpha1.TenantBadgeSpec{Enabled: true} }

// badgeDoor serves the host-root badge route for one seeded tenant. A nil
// reader leaves the record store unwired.
func badgeDoor(t *testing.T, tm *v1alpha1.Tenant, reader recordstore.Reader) *http.ServeMux {
	t.Helper()
	store := crdstore.NewFake()
	if tm != nil {
		crdstore.MustSeed(store, tm)
	}
	srv := NewServer(&Dependencies{Store: store, RecordStore: reader, Logger: slog.Default()})
	mux := http.NewServeMux()
	mux.HandleFunc(JobBadgePattern, srv.HandleJobBadge)
	return mux
}

func badgeGet(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func badgeRun(jobPath, status string, result *string) recordstore.Run {
	return recordstore.Run{
		RunID: "r1", Cluster: "core", Tenant: "payments", JobPath: jobPath,
		Status: status, Result: result, CreatedAt: time.Now().UTC(),
	}
}

func TestJobBadge_ServesLastCompletedResult(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(badgeRun("payments/checkout-api", "COMPLETED", badgeStrPtr("FAILURE")))
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">failing<") || !strings.Contains(body, badgeColorFailing) {
		t.Errorf("badge does not show the failing result: %s", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; style-src 'unsafe-inline'" {
		t.Errorf("Content-Security-Policy = %q", got)
	}
}

// TestJobBadge_RefusalsAnswerIdenticalBytes asserts that every refusal answers
// the same body and the same no-store header.
func TestJobBadge_RefusalsAnswerIdenticalBytes(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(badgeRun("payments/checkout-api", "COMPLETED", badgeStrPtr("SUCCESS")))
	off := badgeTenant("ctrl-a", &v1alpha1.TenantBadgeSpec{Enabled: false}, badgeTestToken)
	noBadge := badgeTenant("ctrl-a", nil, badgeTestToken)
	emptyToken := badgeTenant("ctrl-a", badgeOn(), "")
	on := badgeTenant("ctrl-a", badgeOn(), badgeTestToken)
	cases := []struct {
		name string
		tm   *v1alpha1.Tenant
		path string
	}{
		{"unknown tenant", on, "/badges/nope/" + badgeTestToken + "/checkout-api.svg"},
		{"switch off", off, "/badges/payments/" + badgeTestToken + "/checkout-api.svg"},
		{"badge block absent", noBadge, "/badges/payments/" + badgeTestToken + "/checkout-api.svg"},
		{"wrong token", on, "/badges/payments/ffffffffffffffffffffffffffffffff/checkout-api.svg"},
		{"empty stored token", emptyToken, "/badges/payments/" + badgeTestToken + "/checkout-api.svg"},
		{"no svg suffix", on, "/badges/payments/" + badgeTestToken + "/checkout-api"},
		{"leading slash", on, "/badges/payments/" + badgeTestToken + "/%2Fcheckout-api.svg"},
	}
	var want string
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := badgeGet(badgeDoor(t, tc.tm, reader), tc.path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if i == 0 {
				want = rec.Body.String()
			} else if rec.Body.String() != want {
				t.Errorf("body = %q, want %q", rec.Body.String(), want)
			}
		})
	}
	if want != badgeNotFoundBody {
		t.Errorf("refusal body = %q, want %q", want, badgeNotFoundBody)
	}
}

// TestBadgeRelPathOK covers the segment check directly. The mux cleans a
// literal dot segment before the handler runs, so the route cannot reach it.
func TestBadgeRelPathOK(t *testing.T) {
	cases := map[string]bool{
		"checkout-api": true,
		"app/main":     true,
		"":             false,
		"/app":         false,
		"app//main":    false,
		"app/./main":   false,
		"app/../main":  false,
		"..":           false,
	}
	for rel, want := range cases {
		if got := badgeRelPathOK(rel); got != want {
			t.Errorf("badgeRelPathOK(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestJobBadge_NilRecordStoreRefuses(t *testing.T) {
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), nil)
	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusNotFound || rec.Body.String() != badgeNotFoundBody {
		t.Errorf("status = %d, body = %q, want the refusal", rec.Code, rec.Body.String())
	}
}

// TestJobBadge_PagesPastTriggerFailures asserts a run of trigger failures does
// not hide the last build that reached Jenkins. The newest ten completed runs
// are all TRIGGER_FAILED, and the deciding SUCCESS sits on the next page.
func TestJobBadge_PagesPastTriggerFailures(t *testing.T) {
	reader := newFakeRecordReader()
	base := time.Now().UTC()
	success := badgeRun("payments/checkout-api", "COMPLETED", badgeStrPtr("SUCCESS"))
	success.RunID = "old-success"
	success.CreatedAt = base.Add(-time.Hour)
	reader.addRun(success)
	for i := range badgeRecentCompleted {
		failed := badgeRun("payments/checkout-api", "COMPLETED", badgeStrPtr("TRIGGER_FAILED"))
		failed.RunID = "tf-" + strconv.Itoa(i)
		failed.CreatedAt = base.Add(-time.Duration(i) * time.Minute)
		reader.addRun(failed)
	}
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if !strings.Contains(rec.Body.String(), ">passing<") {
		t.Errorf("badge hid the deciding run behind trigger failures: %s", rec.Body.String())
	}
}

func TestJobBadge_BranchQueryResolvesHead(t *testing.T) {
	reader := newFakeRecordReader()
	seedBranchInventory(reader, time.Now().UTC())
	reader.addRun(badgeRun("payments/mb/main", "COMPLETED", badgeStrPtr("SUCCESS")))
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/mb.svg?branch=main")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, ">passing<") {
		t.Errorf("status = %d, body = %s", rec.Code, body)
	}
	if !strings.Contains(body, "mb/main") {
		t.Errorf("label does not name the branch head: %s", body)
	}
}

func TestJobBadge_UnknownBranchAnswersUnknown(t *testing.T) {
	reader := newFakeRecordReader()
	seedBranchInventory(reader, time.Now().UTC())
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	for _, branch := range []string{"nope", "PR-9"} {
		rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/mb.svg?branch="+branch)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ">unknown<") {
			t.Errorf("branch %q: status = %d, body = %s", branch, rec.Code, rec.Body.String())
		}
	}
}

func TestJobBadge_EncodedChildPathIsOneSegment(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(badgeRun("payments/app/main", "COMPLETED", badgeStrPtr("SUCCESS")))
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/app%2Fmain.svg")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, ">passing<") || !strings.Contains(body, "app/main") {
		t.Errorf("status = %d, body = %s", rec.Code, body)
	}
}

// TestJobBadge_StaysInsideTenantFolder asserts a run of another folder never
// decides the badge of this tenant.
func TestJobBadge_StaysInsideTenantFolder(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(badgeRun("other/x", "COMPLETED", badgeStrPtr("SUCCESS")))
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/x.svg")
	if !strings.Contains(rec.Body.String(), ">no runs<") {
		t.Errorf("badge read a job outside the tenant folder: %s", rec.Body.String())
	}
}

func TestJobBadge_RunningWhenNoCompletedRun(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(badgeRun("payments/checkout-api", "RUNNING", nil))
	mux := badgeDoor(t, badgeTenant("ctrl-a", badgeOn(), badgeTestToken), reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if !strings.Contains(rec.Body.String(), ">running<") {
		t.Errorf("badge does not show a running build: %s", rec.Body.String())
	}
}

func TestJobBadge_NoPlacementAnswersNoRuns(t *testing.T) {
	mux := badgeDoor(t, badgeTenant("", badgeOn(), badgeTestToken), newFakeRecordReader())

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ">no runs<") {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func seedPlacedBadgeTenant(client *fakeResourceClient, badge *v1alpha1.TenantBadgeSpec, token string) {
	crdstore.MustSeed(client.crdStore, badgeTenant("ctrl-a", badge, token))
}

func TestGetJobBadge_ReturnsPathWhenEnabled(t *testing.T) {
	client := newFakeResourceClient()
	seedPlacedBadgeTenant(client, badgeOn(), badgeTestToken)
	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		pipelineBinding("badge", "payments"),
	}), false)
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleGetJobBadge, "/pipelines/payments/jobs/payments%2Fapp%2Fmain/badge",
		map[string]string{"tenant": "payments", "job": "payments/app/main"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out jobBadgeJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "/badges/payments/" + badgeTestToken + "/app%2Fmain.svg"
	if !out.Enabled || out.Path != want {
		t.Errorf("got %+v, want enabled with path %q", out, want)
	}
}

func TestGetJobBadge_DisabledOmitsPath(t *testing.T) {
	client := newFakeResourceClient()
	seedPlacedBadgeTenant(client, nil, badgeTestToken)
	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		pipelineBinding("badge", "payments"),
	}), false)
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleGetJobBadge, "/pipelines/payments/jobs/payments%2Fapp/badge",
		map[string]string{"tenant": "payments", "job": "payments/app"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "path") || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("disabled badge body = %s", rec.Body.String())
	}
}

func TestGetJobBadge_UnreadableJobIs404(t *testing.T) {
	client := newFakeResourceClient()
	seedPlacedBadgeTenant(client, badgeOn(), badgeTestToken)
	authz := NewAuthorizer(testResolver(pipelinesRoles(), nil), false)
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleGetJobBadge, "/pipelines/payments/jobs/payments%2Fapp/badge",
		map[string]string{"tenant": "payments", "job": "payments/app"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
