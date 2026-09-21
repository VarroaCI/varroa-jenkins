package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// lockTestBase is the instant every lock route test stamps its rows from.
var lockTestBase = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// lockRouteKeySet is every key either lock route may answer on one row.
// The response carries no queue position and no holder.
var lockRouteKeySet = map[string]bool{
	"name":         true,
	"tenant":       true,
	"jobPath":      true,
	"state":        true,
	"runId":        true,
	"runLink":      true,
	"waitingSince": true,
	"acquiredAt":   true,
}

// lockTeamClient seeds an unplaced Team "ops" whose two namespaces hold
// one controller each, so the team resolves to the tenants ns-a/ctrl-a
// and ns-b/ctrl-b.
func lockTeamClient(t *testing.T) *fakeResourceClient {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
	}
	seedTeam(t, client.crdStore, unplacedTeamObject("ops", "ns-a", "ns-b"))
	return client
}

// lockRow builds one record-store lock row for the fake reader. A held
// or releasing row carries its own acquiredAt, which a waiting row
// leaves unset.
func lockRow(name, runID, tenant, jobPath, state string, waitingSince time.Time) recordstore.FleetLock {
	row := recordstore.FleetLock{
		Name: name, RunID: runID, Cluster: "core", Tenant: tenant, JobPath: jobPath,
		State: state, WaitingSince: waitingSince,
	}
	if state == "held" || state == "releasing" {
		at := waitingSince.Add(30 * time.Second)
		row.AcquiredAt = &at
	}
	return row
}

// fleetLockAuthorizer grants rules to user "u" through one cluster-wide
// binding: no namespace allowlist and no job glob, so it satisfies the
// fleet-wide gate and reads every tenant.
func fleetLockAuthorizer(rules ...v1alpha1.APIRule) *Authorizer {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-op"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: rules},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-op-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "fleet-op",
		},
	}
	return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
}

// decodeLockItems decodes either lock route's own body into the response
// shape.
func decodeLockItems(t *testing.T, body []byte) []fleetLockJSON {
	t.Helper()
	var resp struct {
		Items []fleetLockJSON `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode lock list: %v", err)
	}
	return resp.Items
}

// TestHandleListTeamFleetLocks_TwoTenantMerge asserts a team that
// resolves to two tenants answers both tenants' locks in one items
// list, each row carrying the link of the run's own page, as a run
// search result carries it: /pipelines/{team}/{jobPath}/{runId}, with
// the team and the job path's own slashes escaped. The team segment is
// the resolved ref's team, which for an unplaced team's controller
// tenant is the controller name.
func TestHandleListTeamFleetLocks_TwoTenantMerge(t *testing.T) {
	client := lockTeamClient(t)
	reader := newFakeRecordReader()
	reader.locksByTenant["core/ns-a/ctrl-a"] = []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-a", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase),
	}
	reader.locksByTenant["core/ns-b/ctrl-b"] = []recordstore.FleetLock{
		lockRow("ns-b/ctrl-b/deploy", "run-b", "ns-b/ctrl-b", "ns-b/ctrl-b/main", "waiting", lockTestBase.Add(time.Minute)),
	}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b"), reader, nil)

	rec := doPipelinesRequest(t, srv.handleListTeamFleetLocks, "/pipelines/ops/locks", map[string]string{"team": "ops"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	items := decodeLockItems(t, rec.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 merged rows: %s", len(items), rec.Body.String())
	}
	if items[0].RunID != "run-a" || items[1].RunID != "run-b" {
		t.Fatalf("items = %+v, want ns-a's lock first by name", items)
	}
	byRun := map[string]fleetLockJSON{}
	for _, item := range items {
		byRun[item.RunID] = item
	}
	for _, tc := range []struct {
		runID, want string
	}{
		{"run-a", "/pipelines/ctrl-a/ns-a%2Fctrl-a%2Fmain/run-a"},
		{"run-b", "/pipelines/ctrl-b/ns-b%2Fctrl-b%2Fmain/run-b"},
	} {
		item, ok := byRun[tc.runID]
		if !ok {
			t.Fatalf("row %s is absent: %s", tc.runID, rec.Body.String())
		}
		if item.RunLink == nil || *item.RunLink != tc.want {
			t.Errorf("row %s runLink = %v, want %s", tc.runID, item.RunLink, tc.want)
		}
	}
	if items[0].AcquiredAt == nil {
		t.Error("the held row carries no acquiredAt")
	}
	if items[1].AcquiredAt != nil {
		t.Error("the waiting row carries an acquiredAt")
	}
}

// TestHandleListTeamFleetLocks_ForeignRunLinkFollowsTheGrant asserts a
// lock row of another tenant always carries its tenant and job path, and
// carries its run link only when the caller's own grant reads that
// tenant. A granted link is the run's own page path, built from the
// granted tenant's own team.
func TestHandleListTeamFleetLocks_ForeignRunLinkFollowsTheGrant(t *testing.T) {
	own := lockRow("ns-a/ctrl-a/deploy", "run-own", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase)
	foreign := lockRow("ns-a/ctrl-a/gate", "run-foreign", "ns-z/ctrl-z", "ns-z/ctrl-z/release", "held", lockTestBase.Add(time.Minute))

	cases := []struct {
		name string
		// tenants is the set of tenants the caller's grant reads.
		tenants []string
		// wantLink is the foreign row's exact run link, or "" when the
		// row must carry none at all.
		wantLink string
	}{
		{"a foreign tenant the caller may not read", []string{"ns-a/ctrl-a"}, ""},
		{"a foreign tenant the caller may read", []string{"ns-a/ctrl-a", "ns-z/ctrl-z"}, "/pipelines/ctrl-z/ns-z%2Fctrl-z%2Frelease/run-foreign"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := lockTeamClient(t)
			reader := newFakeRecordReader()
			reader.locksByTenant["core/ns-a/ctrl-a"] = []recordstore.FleetLock{own, foreign}
			srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, tc.tenants...), reader, nil)

			rec := doPipelinesRequest(t, srv.handleListTeamFleetLocks, "/pipelines/ops/locks", map[string]string{"team": "ops"})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			byRun := map[string]fleetLockJSON{}
			for _, item := range decodeLockItems(t, rec.Body.Bytes()) {
				byRun[item.RunID] = item
			}

			got, ok := byRun["run-foreign"]
			if !ok {
				t.Fatalf("the foreign row is absent: %s", rec.Body.String())
			}
			if got.Tenant != "ns-z/ctrl-z" || got.JobPath != "ns-z/ctrl-z/release" {
				t.Errorf("foreign row = %+v, want its own tenant and job path", got)
			}
			if tc.wantLink == "" {
				if got.RunLink != nil {
					t.Errorf("foreign row runLink = %q, want no link at all", *got.RunLink)
				}
			} else if got.RunLink == nil || *got.RunLink != tc.wantLink {
				t.Errorf("foreign row runLink = %v, want %s", got.RunLink, tc.wantLink)
			}
			wantOwn := "/pipelines/ctrl-a/ns-a%2Fctrl-a%2Fmain/run-own"
			if own, ok := byRun["run-own"]; !ok || own.RunLink == nil || *own.RunLink != wantOwn {
				t.Errorf("the caller's own row = %+v, want the run link %s", own, wantOwn)
			}
		})
	}
}

// TestHandleListAllFleetLocks_OperatorRoute asserts the fleet-wide route
// denies a caller without fleetlocks:read, and answers every tenant's
// rows for a caller holding it.
func TestHandleListAllFleetLocks_OperatorRoute(t *testing.T) {
	rows := []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-a", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase),
		lockRow("ns-b/ctrl-b/deploy", "run-b", "ns-b/ctrl-b", "ns-b/ctrl-b/main", "waiting", lockTestBase.Add(time.Minute)),
		lockRow("ns-b/ctrl-b/gate", "run-c", "ns-b/ctrl-b", "ns-b/ctrl-b/release", "releasing", lockTestBase.Add(2*time.Minute)),
	}

	t.Run("a caller without the fleet-lock read is denied", func(t *testing.T) {
		client := lockTeamClient(t)
		reader := newFakeRecordReader()
		reader.locksByCluster["core"] = rows
		srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b"), reader, nil)

		rec := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an operator reads every tenant's rows", func(t *testing.T) {
		client := lockTeamClient(t)
		reader := newFakeRecordReader()
		reader.locksByCluster["core"] = rows
		auth := fleetLockAuthorizer(
			v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}},
			v1alpha1.APIRule{Resources: []string{"pipelines"}, Verbs: []string{"read"}},
		)
		srv := pipelinesTestServer(client, auth, reader, nil)

		rec := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		items := decodeLockItems(t, rec.Body.Bytes())
		if len(items) != len(rows) {
			t.Fatalf("items = %d, want %d: %s", len(items), len(rows), rec.Body.String())
		}
		tenants := map[string]int{}
		wantLinks := map[string]string{
			"run-a": "/pipelines/ctrl-a/ns-a%2Fctrl-a%2Fmain/run-a",
			"run-b": "/pipelines/ctrl-b/ns-b%2Fctrl-b%2Fmain/run-b",
			"run-c": "/pipelines/ctrl-b/ns-b%2Fctrl-b%2Frelease/run-c",
		}
		for _, item := range items {
			tenants[item.Tenant]++
			if item.RunLink == nil || *item.RunLink != wantLinks[item.RunID] {
				t.Errorf("row %s runLink = %v, want %s", item.RunID, item.RunLink, wantLinks[item.RunID])
			}
		}
		if tenants["ns-a/ctrl-a"] != 1 || tenants["ns-b/ctrl-b"] != 2 {
			t.Fatalf("tenant row counts = %v, want one row for ns-a/ctrl-a and two for ns-b/ctrl-b", tenants)
		}

		// The optional cluster parameter names the read, and a cluster
		// the store holds no row for answers an empty list.
		other := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks?cluster=other", nil)
		if other.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", other.Code, other.Body.String())
		}
		if got := decodeLockItems(t, other.Body.Bytes()); len(got) != 0 {
			t.Fatalf("rows for another cluster = %+v, want none", got)
		}
		if body := strings.TrimSpace(other.Body.String()); body != `{"items":[]}` {
			t.Errorf("empty body = %s, want {\"items\":[]}", body)
		}
	})
}

// TestLockRoutes_CarryNoQueuePositionOrHolderField asserts neither route
// answers a field beyond the frozen row shape: no queue position, and no
// derived holder.
func TestLockRoutes_CarryNoQueuePositionOrHolderField(t *testing.T) {
	rows := []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-a", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase),
		lockRow("ns-a/ctrl-a/deploy", "run-b", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "waiting", lockTestBase.Add(time.Minute)),
	}

	routes := []struct {
		name    string
		path    string
		pick    func(*Server) func(http.ResponseWriter, *http.Request)
		pathVal map[string]string
		auth    *Authorizer
	}{
		{
			name:    "team",
			path:    "/pipelines/ops/locks",
			pick:    func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListTeamFleetLocks },
			pathVal: map[string]string{"team": "ops"},
			auth:    pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b"),
		},
		{
			name: "operator",
			path: "/locks",
			pick: func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListAllFleetLocks },
			auth: fleetLockAuthorizer(v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}}),
		},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			client := lockTeamClient(t)
			reader := newFakeRecordReader()
			reader.locksByTenant["core/ns-a/ctrl-a"] = rows
			reader.locksByCluster["core"] = rows
			srv := pipelinesTestServer(client, route.auth, reader, nil)

			rec := doPipelinesRequest(t, route.pick(srv), route.path, route.pathVal)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			var resp struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Items) != len(rows) {
				t.Fatalf("items = %d, want %d", len(resp.Items), len(rows))
			}
			for i, item := range resp.Items {
				for key := range item {
					if !lockRouteKeySet[key] {
						t.Errorf("row %d carries the field %q, which the frozen row shape does not name", i, key)
					}
				}
				for _, key := range []string{"name", "tenant", "jobPath", "state", "runId", "waitingSince"} {
					if _, present := item[key]; !present {
						t.Errorf("row %d carries no %s", i, key)
					}
				}
			}
		})
	}
}
