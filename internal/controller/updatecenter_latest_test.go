package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// gapTestUCServerInventory serves inventory from /api/v1/inventory and answers servability
// for every posted plugin. unservable maps "name@version" to a reason; every posted plugin
// not in the map is servable. Each posted plugin is appended to posted.
func gapTestUCServerInventory(t *testing.T, inventory []inventoryEntry, unservable map[string]string, posted *[]ucServabilityPlugin) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: inventory})
		case "/api/v1/servability":
			var req ucServabilityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resp := ucServabilityResponse{}
			for _, p := range req.Plugins {
				*posted = append(*posted, p)
				res := ucServabilityResult{Name: p.Name, Version: p.Version, Servable: true, Via: "metadata"}
				if reason, ok := unservable[p.Name+"@"+p.Version]; ok {
					res = ucServabilityResult{Name: p.Name, Version: p.Version, Reason: reason}
				}
				resp.Plugins = append(resp.Plugins, res)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// testLatestPinStoredAnyVersionNoGap: a latest pin counts as stored when the store holds
// any version of the plugin, so it is neither a gap nor a servability call.
func testLatestPinStoredAnyVersionNoGap(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	addProfile(client, "prof-latest-stored", "2.479.1")
	client.configMapData["prof-latest-stored-content"]["plugins.yaml"] =
		"core:\n  - 2.479.1\nplugins:\n  - artifactId: foo\n    version: latest\n"

	var posted []ucServabilityPlugin
	ts := gapTestUCServerInventory(t, []inventoryEntry{{Name: "foo", Version: "2.0"}}, nil, &posted)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status := lastUCStatus(t, client)

	if status.GapCount != 0 {
		t.Errorf("gapCount = %d, want 0: a latest pin is stored when any version is", status.GapCount)
	}
	if len(posted) != 0 {
		t.Errorf("servability posted %+v, want no post", posted)
	}
	if cc := findUCCondition(status.Conditions, condTypeCoverageComplete); cc == nil || cc.Status != metav1.ConditionTrue {
		t.Errorf("CoverageComplete = %+v, want True", cc)
	}
	if rd := findUCCondition(status.Conditions, condTypeReady); rd == nil || rd.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", rd)
	}
}

// testLatestPinUnstoredPostedAsLatest: with no stored version, the gap keeps version latest
// and the servability post carries latest. A servable answer makes the update center Ready.
func testLatestPinUnstoredPostedAsLatest(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	addProfile(client, "prof-latest-unstored", "2.479.1")
	client.configMapData["prof-latest-unstored-content"]["plugins.yaml"] =
		"core:\n  - 2.479.1\nplugins:\n  - artifactId: foo\n    version: latest\n"

	var posted []ucServabilityPlugin
	ts := gapTestUCServerInventory(t, []inventoryEntry{}, nil, &posted)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status := lastUCStatus(t, client)

	if status.GapCount != 1 || len(status.Gaps) != 1 || status.Gaps[0].Plugin != "foo" || status.Gaps[0].Version != "latest" {
		t.Fatalf("gaps = %d %+v, want one foo@latest gap", status.GapCount, status.Gaps)
	}
	if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "foo", Version: "latest", Core: "2.479.1"}) {
		t.Errorf("posted = %+v, want foo@latest", posted)
	}
	if rd := findUCCondition(status.Conditions, condTypeReady); rd == nil || rd.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", rd)
	}
}

// testLatestPinUnstoredNotListedUnready: a latest pin that no metadata source lists
// reports NotListed and holds Ready False.
func testLatestPinUnstoredNotListedUnready(t *testing.T) {
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()

	addProfile(client, "prof-latest-ghost", "2.479.1")
	client.configMapData["prof-latest-ghost-content"]["plugins.yaml"] =
		"core:\n  - 2.479.1\nplugins:\n  - artifactId: foo\n    version: latest\n"

	var posted []ucServabilityPlugin
	ts := gapTestUCServerInventory(t, []inventoryEntry{}, map[string]string{"foo@latest": "not-listed"}, &posted)

	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status := lastUCStatus(t, client)

	if status.UnservableCount != 1 || len(status.Unservable) != 1 {
		t.Fatalf("unservable = %d %+v, want one entry", status.UnservableCount, status.Unservable)
	}
	if got := status.Unservable[0]; got.Plugin != "foo" || got.Version != "latest" || got.Reason != unservableNotListed {
		t.Errorf("unservable[0] = %+v, want foo latest NotListed", got)
	}
	if rd := findUCCondition(status.Conditions, condTypeReady); rd == nil || rd.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v, want False", rd)
	}
	if status.Phase != v1alpha1.UpdateCenterPhase("Degraded") {
		t.Errorf("phase = %s, want Degraded", status.Phase)
	}
}
