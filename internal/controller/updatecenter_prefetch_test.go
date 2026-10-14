package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// prefetchTestUC is a fake update center for the prefetch pass. It serves an empty
// inventory, a servability answer that marks every plugin servable except those in
// unservable, and a prefetch answer that fails the keys in failKeys.
type prefetchTestUC struct {
	mu       sync.Mutex
	unserv   map[string]string // name@version -> servability reason
	latest   map[string]string // name -> resolved version for a latest entry
	failKeys map[string]bool   // name@version -> prefetch fails
	// prefetchStatus, when non-zero, answers every prefetch with that code.
	prefetchStatus int
	posted         []string // name@version of each posted prefetch entry
	auth           []string // Authorization header of each prefetch call
}

func (f *prefetchTestUC) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/inventory":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: []inventoryEntry{}})
		case "/api/v1/servability":
			var req ucServabilityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resp := ucServabilityResponse{}
			for _, p := range req.Plugins {
				key := p.Name + "@" + p.Version
				res := ucServabilityResult{Name: p.Name, Version: p.Version, Servable: true, Via: "metadata"}
				if reason, ok := f.unserv[key]; ok {
					res = ucServabilityResult{Name: p.Name, Version: p.Version, Reason: reason}
				} else if v, ok := f.latest[p.Name]; ok && p.Version == "latest" {
					res.ResolvedVersion = v
				}
				resp.Plugins = append(resp.Plugins, res)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/v1/prefetch":
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			if f.prefetchStatus != 0 {
				w.WriteHeader(f.prefetchStatus)
				return
			}
			var req ucPrefetchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resp := ucPrefetchResponse{}
			for _, p := range req.Plugins {
				key := p.Name + "@" + p.Version
				f.posted = append(f.posted, key)
				res := ucPrefetchResult{Name: p.Name, Version: p.Version, Outcome: "fetched"}
				if f.failKeys[key] {
					res.Outcome = "failed"
					res.Reason = "download-failed"
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

// prefetchPlugins builds a plugins.yaml body that declares name@version pins.
func prefetchPlugins(pins ...string) string {
	var b strings.Builder
	b.WriteString("core:\n  - 2.479.1\nplugins:\n")
	for _, pin := range pins {
		name, version, _ := strings.Cut(pin, "@")
		fmt.Fprintf(&b, "  - artifactId: %s\n    version: \"%s\"\n", name, version)
	}
	return b.String()
}

// setupPrefetchClient seeds a client with a prefetch-enabled singleton and one profile.
func setupPrefetchClient(t *testing.T, prefetch bool, pins ...string) *testClient {
	t.Helper()
	client := newTestClient()
	uc := ucWithPullThrough(true)
	uc.Spec.PullThrough.Prefetch = prefetch
	client.updateCenter = uc
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	importTokenSecret(client)
	addProfile(client, "prof-prefetch", "2.479.1")
	client.configMapData["prof-prefetch-content"]["plugins.yaml"] = prefetchPlugins(pins...)
	return client
}

func reconcilePrefetchOnce(t *testing.T, client *testClient, ts *httptest.Server) v1alpha1.UpdateCenterStatus {
	t.Helper()
	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	return reconcileWithPrefetch(t, rec, client)
}

// reconcileWithPrefetch runs one reconcile on rec and returns the patched status.
func reconcileWithPrefetch(t *testing.T, rec *UpdateCenterReconciler, client *testClient) v1alpha1.UpdateCenterStatus {
	t.Helper()
	if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return lastUCStatus(t, client)
}

func prefetchCond(t *testing.T, status v1alpha1.UpdateCenterStatus) *v1alpha1.UpdateCenterCondition {
	t.Helper()
	return findUCCondition(status.Conditions, condTypePluginsPrefetched)
}

func testPrefetchDisabledRemovesCondition(t *testing.T) {
	client := setupPrefetchClient(t, false, "foo@1.0")
	client.updateCenter.Status.Conditions = []v1alpha1.UpdateCenterCondition{{
		Type: condTypePluginsPrefetched, Status: metav1.ConditionTrue, Reason: reasonPrefetchComplete,
	}}
	fake := &prefetchTestUC{}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	if c := prefetchCond(t, status); c != nil {
		t.Fatalf("PluginsPrefetched should be removed when prefetch is off, got %+v", c)
	}
	if len(fake.posted) != 0 {
		t.Fatalf("prefetch posted %v with prefetch off", fake.posted)
	}
}

func testPrefetchPostsServableGapsOnly(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0", "bad@1.0")
	fake := &prefetchTestUC{unserv: map[string]string{"bad@1.0": "not-listed"}}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	if len(fake.posted) != 1 || fake.posted[0] != "foo@1.0" {
		t.Fatalf("posted = %v, want [foo@1.0]", fake.posted)
	}
	if want := "Bearer test-token"; len(fake.auth) != 1 || fake.auth[0] != want {
		t.Fatalf("Authorization = %v, want %q", fake.auth, want)
	}
	c := prefetchCond(t, status)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonPrefetchComplete {
		t.Fatalf("PluginsPrefetched = %+v, want True/PrefetchComplete", c)
	}
}

func testPrefetchCapsBatchAt20(t *testing.T) {
	pins := make([]string, 25)
	for i := range pins {
		pins[i] = fmt.Sprintf("p%02d@1.0", i)
	}
	client := setupPrefetchClient(t, true, pins...)
	fake := &prefetchTestUC{}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	if len(fake.posted) != prefetchPerTick {
		t.Fatalf("posted %d plugins, want %d", len(fake.posted), prefetchPerTick)
	}
	c := prefetchCond(t, status)
	if c == nil || c.Reason != reasonPrefetchInProgress || c.Status != metav1.ConditionFalse {
		t.Fatalf("PluginsPrefetched = %+v, want False/PrefetchInProgress", c)
	}
}

func testPrefetchTokenMissingIsStatusReason(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0")
	delete(client.existingSecrets, updateCenterImportTokenSecret)
	fake := &prefetchTestUC{}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	c := prefetchCond(t, status)
	if c == nil || c.Reason != reasonTokenUnavailable || c.Status != metav1.ConditionFalse {
		t.Fatalf("PluginsPrefetched = %+v, want False/TokenUnavailable", c)
	}
	if len(fake.posted) != 0 {
		t.Fatalf("prefetch posted without a token: %v", fake.posted)
	}
}

func testPrefetchFailureDoesNotGateReady(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0")
	fake := &prefetchTestUC{failKeys: map[string]bool{"foo@1.0": true}}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	c := prefetchCond(t, status)
	if c == nil || c.Reason != reasonPrefetchFailed || c.Status != metav1.ConditionFalse {
		t.Fatalf("PluginsPrefetched = %+v, want False/PrefetchFailed", c)
	}
	if ps := findUCCondition(status.Conditions, condTypePluginsServable); ps == nil || ps.Status != metav1.ConditionTrue {
		t.Fatalf("PluginsServable = %+v, want True", ps)
	}
	if rd := findUCCondition(status.Conditions, condTypeReady); rd == nil || rd.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", rd)
	}
}

func testPrefetchFailureBackoff(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0")
	fake := &prefetchTestUC{failKeys: map[string]bool{"foo@1.0": true}}
	ts := fake.serve(t)

	clock := time.Now()
	rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
	rec.now = func() time.Time { return clock }

	reconcileWithPrefetch(t, rec, client)
	if len(fake.posted) != 1 {
		t.Fatalf("first pass posted %v, want one entry", fake.posted)
	}

	// Inside the retry window the failed key is left out of the batch.
	clock = clock.Add(5 * time.Minute)
	status := reconcileWithPrefetch(t, rec, client)
	if len(fake.posted) != 1 {
		t.Fatalf("pass inside the retry window posted again: %v", fake.posted)
	}
	if c := prefetchCond(t, status); c == nil || c.Reason != reasonPrefetchFailed {
		t.Fatalf("PluginsPrefetched = %+v, want PrefetchFailed", c)
	}

	// After the window the key is posted again.
	clock = clock.Add(6 * time.Minute)
	reconcileWithPrefetch(t, rec, client)
	if len(fake.posted) != 2 {
		t.Fatalf("pass after the retry window posted %v, want a second post", fake.posted)
	}
}

func testPrefetchLatestPostsResolvedVersion(t *testing.T) {
	client := setupPrefetchClient(t, true, "bar@latest")
	fake := &prefetchTestUC{latest: map[string]string{"bar": "3.2.1"}}
	ts := fake.serve(t)

	reconcilePrefetchOnce(t, client, ts)
	if len(fake.posted) != 1 || fake.posted[0] != "bar@3.2.1" {
		t.Fatalf("posted = %v, want [bar@3.2.1]", fake.posted)
	}
}

func testPrefetchUnsupported(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0")
	fake := &prefetchTestUC{prefetchStatus: http.StatusNotFound}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	c := prefetchCond(t, status)
	if c == nil || c.Reason != reasonPrefetchUnsupported || c.Status != metav1.ConditionFalse {
		t.Fatalf("PluginsPrefetched = %+v, want False/PrefetchUnsupported", c)
	}
}
