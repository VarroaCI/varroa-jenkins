package api

import (
	"encoding/json"
	"fmt"
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
	"cluster":      true,
	"tenant":       true,
	"jobPath":      true,
	"state":        true,
	"runId":        true,
	"runLink":      true,
	"waitingSince": true,
	"acquiredAt":   true,
}

// lockTenantClient seeds an unplaced Tenant "ops" whose two namespaces hold
// one controller each, so the tenant resolves to the tenants ns-a/ctrl-a
// and ns-b/ctrl-b.
func lockTenantClient(t *testing.T) *fakeResourceClient {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
	}
	seedTenant(t, client.crdStore, unplacedTenantObject("ops", "ns-a", "ns-b"))
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

// TestHandleListTenantFleetLocks_TwoTenantMerge asserts a tenant that
// resolves to two tenants answers both tenants' locks in one items
// list, each row carrying the link of the run's own page, as a run
// search result carries it: /pipelines/{tenant}/{jobPath}/{runId}, with
// the tenant and the job path's own slashes escaped. The tenant segment is
// the resolved ref's tenant, which for an unplaced tenant's controller
// tenant is the controller name.
func TestHandleListTenantFleetLocks_TwoTenantMerge(t *testing.T) {
	client := lockTenantClient(t)
	reader := newFakeRecordReader()
	reader.locksByTenant["core/ns-a/ctrl-a"] = []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-a", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase),
	}
	reader.locksByTenant["core/ns-b/ctrl-b"] = []recordstore.FleetLock{
		lockRow("ns-b/ctrl-b/deploy", "run-b", "ns-b/ctrl-b", "ns-b/ctrl-b/main", "waiting", lockTestBase.Add(time.Minute)),
	}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b"), reader, nil)

	rec := doPipelinesRequest(t, srv.handleListTenantFleetLocks, "/pipelines/ops/locks", map[string]string{"tenant": "ops"})
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

// TestHandleListTenantFleetLocks_ForeignRunLinkFollowsTheGrant asserts a
// lock row of another tenant always carries its tenant and job path, and
// carries its run link only when the caller's own grant reads that
// tenant. A granted link is the run's own page path, built from the
// granted tenant's own tenant.
func TestHandleListTenantFleetLocks_ForeignRunLinkFollowsTheGrant(t *testing.T) {
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
			client := lockTenantClient(t)
			reader := newFakeRecordReader()
			reader.locksByTenant["core/ns-a/ctrl-a"] = []recordstore.FleetLock{own, foreign}
			srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, tc.tenants...), reader, nil)

			rec := doPipelinesRequest(t, srv.handleListTenantFleetLocks, "/pipelines/ops/locks", map[string]string{"tenant": "ops"})
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
// rows of every cluster for a caller holding it, each row with its own
// cluster.
func TestHandleListAllFleetLocks_OperatorRoute(t *testing.T) {
	member := lockRow("ns-b/ctrl-b/gate", "run-c", "ns-b/ctrl-b", "ns-b/ctrl-b/release", "releasing", lockTestBase.Add(2*time.Minute))
	member.Cluster = "member"
	rows := []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-a", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase),
		lockRow("ns-b/ctrl-b/deploy", "run-b", "ns-b/ctrl-b", "ns-b/ctrl-b/main", "waiting", lockTestBase.Add(time.Minute)),
		member,
	}

	t.Run("a caller without the fleet-lock read is denied", func(t *testing.T) {
		client := lockTenantClient(t)
		reader := newFakeRecordReader()
		reader.allLocks = rows
		srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b"), reader, nil)

		rec := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an operator reads every tenant's rows of every cluster", func(t *testing.T) {
		client := lockTenantClient(t)
		reader := newFakeRecordReader()
		reader.allLocks = rows
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
		wantLinks := map[string]string{
			"run-a": "/pipelines/ctrl-a/ns-a%2Fctrl-a%2Fmain/run-a",
			"run-b": "/pipelines/ctrl-b/ns-b%2Fctrl-b%2Fmain/run-b",
			"run-c": "/pipelines/ctrl-b/ns-b%2Fctrl-b%2Frelease/run-c",
		}
		wantClusters := map[string]string{"run-a": "core", "run-b": "core", "run-c": "member"}
		for _, item := range items {
			if item.RunLink == nil || *item.RunLink != wantLinks[item.RunID] {
				t.Errorf("row %s runLink = %v, want %s", item.RunID, item.RunLink, wantLinks[item.RunID])
			}
			if item.Cluster != wantClusters[item.RunID] {
				t.Errorf("row %s cluster = %q, want %q", item.RunID, item.Cluster, wantClusters[item.RunID])
			}
		}
	})

	t.Run("an empty store answers an empty items list", func(t *testing.T) {
		client := lockTenantClient(t)
		srv := pipelinesTestServer(client, fleetLockAuthorizer(
			v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}},
		), newFakeRecordReader(), nil)

		rec := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if body := strings.TrimSpace(rec.Body.String()); body != `{"items":[]}` {
			t.Errorf("empty body = %s, want {\"items\":[]}", body)
		}
	})
}

// TestHandleListAllFleetLocks_GroupsHolderAcrossClusters asserts a lock
// whose holder runs on one cluster and whose waiter runs on another
// answers both rows in one list, the holder first, so a consumer that
// groups by name finds the holder of the waiter.
func TestHandleListAllFleetLocks_GroupsHolderAcrossClusters(t *testing.T) {
	holder := lockRow("ns-a/ctrl-a/deploy", "run-holder", "ns-a/ctrl-a", "ns-a/ctrl-a/main", "held", lockTestBase)
	holder.Cluster = "member"
	waiter := lockRow("ns-a/ctrl-a/deploy", "run-waiter", "ns-b/ctrl-b", "ns-b/ctrl-b/main", "waiting", lockTestBase.Add(time.Minute))
	reader := newFakeRecordReader()
	reader.allLocks = []recordstore.FleetLock{holder, waiter}
	srv := pipelinesTestServer(lockTenantClient(t), fleetLockAuthorizer(
		v1alpha1.APIRule{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}},
	), reader, nil)

	rec := doPipelinesRequest(t, srv.handleListAllFleetLocks, "/locks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	items := decodeLockItems(t, rec.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2: %s", len(items), rec.Body.String())
	}
	if items[0].RunID != "run-holder" || items[0].Cluster != "member" || items[0].State != "held" {
		t.Errorf("first row = %+v, want the member holder", items[0])
	}
	if items[1].RunID != "run-waiter" || items[1].Cluster != "core" || items[1].State != "waiting" {
		t.Errorf("second row = %+v, want the core waiter", items[1])
	}
	if items[0].Name != items[1].Name {
		t.Errorf("names = %q and %q, want one lock name", items[0].Name, items[1].Name)
	}
}

// TestHandleListTenantFleetLocks_UnreachableCarriesTheCode asserts a store
// that does not answer gives 503 with the unreachable code. The Locks card
// tells that case apart from a store that is off, so the code must reach
// the body.
func TestHandleListTenantFleetLocks_UnreachableCarriesTheCode(t *testing.T) {
	client := lockTenantClient(t)
	reader := newFakeRecordReader()
	reader.locksErr = fmt.Errorf("%w: list fleet locks: connection refused", recordstore.ErrUnavailable)
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a"), reader, nil)

	rec := doPipelinesRequest(t, srv.handleListTenantFleetLocks, "/pipelines/ops/locks", map[string]string{"tenant": "ops"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != codeRecordStoreUnreachable {
		t.Fatalf("code = %q, want %q (body %s)", body["code"], codeRecordStoreUnreachable, rec.Body.String())
	}
}

// TestHandleListTenantFleetLocks_EmptyJobPathCarriesNoRunLink asserts a
// row whose jobPath is empty carries no runLink, even under a grant that
// reads its tenant and would carry the link of a named job path.
func TestHandleListTenantFleetLocks_EmptyJobPathCarriesNoRunLink(t *testing.T) {
	client := lockTenantClient(t)
	reader := newFakeRecordReader()
	reader.locksByTenant["core/ns-a/ctrl-a"] = []recordstore.FleetLock{
		lockRow("ns-a/ctrl-a/deploy", "run-empty", "ns-a/ctrl-a", "", "held", lockTestBase),
		lockRow("ns-a/ctrl-a/gate", "run-named", "ns-a/ctrl-a", "ns-a/ctrl-a/release", "held", lockTestBase.Add(time.Minute)),
	}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a"), reader, nil)

	rec := doPipelinesRequest(t, srv.handleListTenantFleetLocks, "/pipelines/ops/locks", map[string]string{"tenant": "ops"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	byRun := map[string]fleetLockJSON{}
	for _, item := range decodeLockItems(t, rec.Body.Bytes()) {
		byRun[item.RunID] = item
	}
	if len(byRun) != 2 {
		t.Fatalf("rows = %d, want 2: %s", len(byRun), rec.Body.String())
	}

	empty, ok := byRun["run-empty"]
	if !ok {
		t.Fatalf("the row with no job path is absent: %s", rec.Body.String())
	}
	if empty.JobPath != "" {
		t.Errorf("jobPath = %q, want the empty value the row stores", empty.JobPath)
	}
	if empty.RunLink != nil {
		t.Errorf("runLink = %q, want no link for an empty jobPath", *empty.RunLink)
	}

	// The grant would carry a link for a row that names its job path, so
	// the absence above is the empty path's own rule.
	named, ok := byRun["run-named"]
	if !ok {
		t.Fatalf("the row with a job path is absent: %s", rec.Body.String())
	}
	wantNamed := "/pipelines/ctrl-a/ns-a%2Fctrl-a%2Frelease/run-named"
	if named.RunLink == nil || *named.RunLink != wantNamed {
		t.Errorf("runLink = %v, want %s", named.RunLink, wantNamed)
	}
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
			name:    "tenant",
			path:    "/pipelines/ops/locks",
			pick:    func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListTenantFleetLocks },
			pathVal: map[string]string{"tenant": "ops"},
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
			client := lockTenantClient(t)
			reader := newFakeRecordReader()
			reader.locksByTenant["core/ns-a/ctrl-a"] = rows
			reader.allLocks = rows
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
				for _, key := range []string{"name", "cluster", "tenant", "jobPath", "state", "runId"} {
					if _, present := item[key]; !present {
						t.Errorf("row %d carries no %s", i, key)
					}
				}
				var state string
				if err := json.Unmarshal(item["state"], &state); err != nil {
					t.Fatalf("row %d state: %v", i, err)
				}
				_, hasWaiting := item["waitingSince"]
				_, hasAcquired := item["acquiredAt"]
				switch state {
				case "waiting":
					if !hasWaiting || hasAcquired {
						t.Errorf("waiting row %d: waitingSince present = %t, acquiredAt present = %t, want true, false", i, hasWaiting, hasAcquired)
					}
				default:
					if hasWaiting || !hasAcquired {
						t.Errorf("%s row %d: waitingSince present = %t, acquiredAt present = %t, want false, true", state, i, hasWaiting, hasAcquired)
					}
				}
			}
		})
	}
}
