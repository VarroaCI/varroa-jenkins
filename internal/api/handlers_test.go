package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellSummaryRows lists the controllers and returns one row for each name.
func cellSummaryRows(t *testing.T, srv *Server) map[string]map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	rows := map[string]map[string]interface{}{}
	for _, item := range envelope.Items {
		name, _ := item["name"].(string)
		rows[name] = item
	}
	return rows
}

func TestControllerSummary_CellFields(t *testing.T) {
	srv, client := newRoutingTestServer()
	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	unready := metav1.NewTime(time.Date(2026, 1, 3, 4, 5, 6, 0, time.UTC))

	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cell-a",
			Namespace:         "team-a",
			CreationTimestamp: created,
			Labels:            map[string]string{cells.LabelCellPool: "lts-2-555"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Cell: &v1alpha1.CellStatus{
				Tenants:        []string{"payments", "search"},
				AppliedTenants: []string{"payments"},
				Warm:           false,
				UnreadySince:   &unready,
			},
		},
	}
	client.controllers["cell-a"] = cell
	crdstore.MustSeed(client.crdStore, cell)

	stateful := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "team-a", CreationTimestamp: created},
	}
	client.controllers["plain"] = stateful
	crdstore.MustSeed(client.crdStore, stateful)

	rows := cellSummaryRows(t, srv)

	row := rows["cell-a"]
	if row == nil {
		t.Fatal("cell-a row missing")
	}
	if row["kind"] != "cell" {
		t.Errorf("kind: want cell, got %v", row["kind"])
	}
	if row["cellPool"] != "lts-2-555" {
		t.Errorf("cellPool: want lts-2-555, got %v", row["cellPool"])
	}
	if row["tenantCount"] != float64(2) {
		t.Errorf("tenantCount: want 2, got %v", row["tenantCount"])
	}
	if row["maxTenants"] != float64(10) {
		t.Errorf("maxTenants: want 10, got %v", row["maxTenants"])
	}
	if row["warm"] != false {
		t.Errorf("warm: want false, got %v", row["warm"])
	}
	if row["createdAt"] != "2026-01-02T03:04:05Z" {
		t.Errorf("createdAt: want 2026-01-02T03:04:05Z, got %v", row["createdAt"])
	}
	if row["unreadySince"] != "2026-01-03T04:05:06Z" {
		t.Errorf("unreadySince: want 2026-01-03T04:05:06Z, got %v", row["unreadySince"])
	}

	plain := rows["plain"]
	if plain == nil {
		t.Fatal("plain row missing")
	}
	if plain["kind"] != "stateful" {
		t.Errorf("kind: want stateful, got %v", plain["kind"])
	}
	if plain["createdAt"] != "2026-01-02T03:04:05Z" {
		t.Errorf("createdAt: want 2026-01-02T03:04:05Z, got %v", plain["createdAt"])
	}
	for _, key := range []string{"cellPool", "tenantCount", "maxTenants", "warm", "unreadySince"} {
		if _, ok := plain[key]; ok {
			t.Errorf("stateful row carries %q", key)
		}
	}
}

func TestControllerSummary_WarmCellReportsFalseTenantCount(t *testing.T) {
	srv, client := newRoutingTestServer()
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-cell", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status:     v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{Warm: true}},
	}
	client.controllers["warm-cell"] = cell
	crdstore.MustSeed(client.crdStore, cell)

	row := cellSummaryRows(t, srv)["warm-cell"]
	if row == nil {
		t.Fatal("warm-cell row missing")
	}
	// A zero tenant count and a false warm flag are real states, so both
	// fields are present rather than dropped by omitempty.
	if row["tenantCount"] != float64(0) {
		t.Errorf("tenantCount: want 0, got %v", row["tenantCount"])
	}
	if row["warm"] != true {
		t.Errorf("warm: want true, got %v", row["warm"])
	}
	// spec.cell.maxTenants is zero and the deps carry no chart value, so
	// the cap chain falls to the compiled default.
	if row["maxTenants"] != float64(defaultCellMaxTenants) {
		t.Errorf("maxTenants: want %d, got %v", defaultCellMaxTenants, row["maxTenants"])
	}
	if _, ok := row["unreadySince"]; ok {
		t.Error("a ready cell carries unreadySince")
	}
}
