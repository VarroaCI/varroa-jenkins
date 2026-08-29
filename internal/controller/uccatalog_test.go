package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const testOperatorNS = "varroa-system"

// ucTestServer serves an inventory payload, or a status code when non-zero.
type ucTestServer struct {
	*httptest.Server
	status  int
	entries []inventoryEntry
	skipped []ucSkippedPack
	calls   int
}

// forceSync makes the next Reconcile due without touching status, so a test can
// assert that a failing pass leaves lastSyncTime and observedRevision alone.
func forceSync(src *v1alpha1.CatalogSource) {
	if src.Annotations == nil {
		src.Annotations = map[string]string{}
	}
	src.Annotations[syncRequestedAtAnno] = time.Now().Add(time.Hour).Format(time.RFC3339)
}

func newUCTestServer(t *testing.T) *ucTestServer {
	t.Helper()
	s := &ucTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		if r.URL.Path != "/api/v1/inventory" {
			http.NotFound(w, r)
			return
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		entries := s.entries
		if entries == nil {
			entries = []inventoryEntry{}
		}
		_ = json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: entries, SkippedPacks: s.skipped})
	}))
	t.Cleanup(s.Close)
	return s
}

func newUCReconciler(t *testing.T, tc *catalogTestClient, ucURL string) *CatalogReconciler {
	t.Helper()
	cloner := bundle.NewGitCloner()
	resolver := bundle.NewResolver(t.TempDir())
	return NewCatalogReconciler(tc, tc.store, cloner, resolver, nil, t.TempDir(),
		discardLogger(), testOperatorNS, ucURL, &http.Client{})
}

// reservedSource is the reserved CatalogSource as the operator asserts it.
func reservedSource(ns string) *v1alpha1.CatalogSource {
	return &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      updateCenterSourceName,
			Namespace: ns,
			UID:       types.UID("uc-source-uid"),
			Labels:    map[string]string{managedByLabel: managedByOperator},
		},
	}
}

// seedSingleton puts an UpdateCenter singleton in the store so the teardown
// backstop does not fire.
func seedSingleton(tc *catalogTestClient) {
	crdstore.MustSeed(tc.store, &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName, UID: types.UID("uc-uid")},
	})
}

func sourceStatus(t *testing.T, tc *catalogTestClient) *v1alpha1.CatalogSourceStatus {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogSource]()
	if err != nil {
		t.Fatal(err)
	}
	ps := tc.store.StatusPatches(gvr)
	if len(ps) == 0 {
		t.Fatal("no CatalogSource status patch recorded")
	}
	st, _ := ps[len(ps)-1].Status.(*v1alpha1.CatalogSourceStatus)
	return st
}

func derivedItems(t *testing.T, tc *catalogTestClient) map[string]*v1alpha1.CatalogItem {
	t.Helper()
	out := map[string]*v1alpha1.CatalogItem{}
	for k, v := range tc.storedItems(t) {
		if v.Spec.SourceRef == updateCenterSourceName {
			out[k] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 7.7 — dispatch and lifecycle
// ---------------------------------------------------------------------------

func TestUCDispatch_ThreeWay(t *testing.T) {
	t.Run("neither field and an ordinary name is still an error", func(t *testing.T) {
		tc := newCatalogTestClient()
		r := newUCReconciler(t, tc, "http://unused")
		src := &v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "typo", Namespace: "ns"}}
		crdstore.MustSeed(tc.store, src)
		r.Reconcile(context.Background(), src)

		st := sourceStatus(t, tc)
		if st.Phase != v1alpha1.CatalogSyncError || !strings.Contains(st.Message, "must set either repoURL or ociRef") {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("reserved name outside the operator namespace is rejected", func(t *testing.T) {
		tc := newCatalogTestClient()
		seedSingleton(tc)
		uc := newUCTestServer(t)
		r := newUCReconciler(t, tc, uc.URL)
		src := reservedSource("some-team")
		crdstore.MustSeed(tc.store, src)
		r.Reconcile(context.Background(), src)

		st := sourceStatus(t, tc)
		if st.Phase != v1alpha1.CatalogSyncError {
			t.Fatalf("phase = %q, want Error", st.Phase)
		}
		if !strings.Contains(st.Message, testOperatorNS) {
			t.Errorf("message should name the operator namespace: %q", st.Message)
		}
		if len(derivedItems(t, tc)) != 0 {
			t.Error("a reserved source outside the operator namespace must derive nothing")
		}
		if uc.calls != 0 {
			t.Error("it must not even reach the update center")
		}
	})

	t.Run("reserved name in the operator namespace derives", func(t *testing.T) {
		tc := newCatalogTestClient()
		seedSingleton(tc)
		uc := newUCTestServer(t)
		uc.entries = []inventoryEntry{
			{Name: "acme-widget", Version: "1.2.0", SHA256: "sha256:a", DisplayName: "Acme Widget", Tags: []string{"ui"}},
		}
		r := newUCReconciler(t, tc, uc.URL)
		src := reservedSource(testOperatorNS)
		crdstore.MustSeed(tc.store, src)
		r.Reconcile(context.Background(), src)

		st := sourceStatus(t, tc)
		if st.Phase != v1alpha1.CatalogSyncReady {
			t.Fatalf("phase = %q (%s)", st.Phase, st.Message)
		}
		items := derivedItems(t, tc)
		if len(items) != 1 {
			t.Fatalf("expected 1 derived item, got %d: %v", len(items), items)
		}
		for _, it := range items {
			if it.Spec.Type != v1alpha1.CatalogItemPlugin || it.Spec.Version != "1.2.0" {
				t.Errorf("item spec = %+v", it.Spec)
			}
			if it.Spec.DisplayName != "Acme Widget" {
				t.Errorf("displayName = %q", it.Spec.DisplayName)
			}
			if !containsString(it.Spec.Tags, "update-center") || !containsString(it.Spec.Tags, "ui") {
				t.Errorf("tags = %v", it.Spec.Tags)
			}
			if it.Labels[catalogSourceLabel] != updateCenterSourceName || it.Labels[catalogTypeLabel] != "plugin" {
				t.Errorf("labels = %v", it.Labels)
			}
			if it.Labels[pluginNameLabel] != "acme-widget" || it.Labels[pluginVersionLabel] != "1.2.0" {
				t.Errorf("plugin labels = %v", it.Labels)
			}
			if len(it.OwnerReferences) != 1 || it.OwnerReferences[0].UID != src.UID {
				t.Errorf("ownerRefs = %+v", it.OwnerReferences)
			}
			if it.Spec.Path != "uc://acme-widget@1.2.0" {
				t.Errorf("path = %q", it.Spec.Path)
			}
		}
		if st.ObservedRevision == "" {
			t.Error("observedRevision should carry the inventory digest")
		}
	})

	t.Run("update center not enabled", func(t *testing.T) {
		tc := newCatalogTestClient()
		seedSingleton(tc)
		r := newUCReconciler(t, tc, "")
		src := reservedSource(testOperatorNS)
		crdstore.MustSeed(tc.store, src)
		r.Reconcile(context.Background(), src)

		st := sourceStatus(t, tc)
		if st.Phase != v1alpha1.CatalogSyncError || !strings.Contains(st.Message, "not enabled") {
			t.Fatalf("status = %+v", st)
		}
	})
}

// TestUCReservedSource_Adoption covers a user having created the reserved
// source first: the operator overwrites the spec and stamps its labels rather
// than erroring. Converge, never wedge.
func TestUCReservedSource_Adoption(t *testing.T) {
	tc := newCatalogTestClient()
	uc := &v1alpha1.UpdateCenter{ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName, UID: types.UID("uc-uid")}}
	crdstore.MustSeed(tc.store, uc)
	// A user-created source, with a stray label and a source field that the
	// CRD would now reject but that an older object could still carry.
	crdstore.MustSeed(tc.store, &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      updateCenterSourceName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{"team": "platform"},
		},
		Spec: v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/x.git", Trusted: true},
	})

	r := &UpdateCenterReconciler{store: tc.store, client: tc, operatorNamespace: testOperatorNS, logger: discardLogger()}
	r.reconcileReservedCatalogSource(context.Background(), uc, discardLogger())

	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), tc.store, updateCenterSourceName, testOperatorNS)
	if err != nil {
		t.Fatalf("get reserved source: %v", err)
	}
	if got.Spec.RepoURL != "" || got.Spec.OCIRef != "" {
		t.Errorf("spec should be overwritten to the zero-source-field shape, got %+v", got.Spec)
	}
	if got.Spec.Trusted {
		t.Error("trusted must stay false: derived items are type plugin and carry no scripts")
	}
	if got.Labels[managedByLabel] != managedByOperator {
		t.Errorf("managed-by label not stamped: %v", got.Labels)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("user labels should survive: %v", got.Labels)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "UpdateCenter" {
		t.Errorf("ownerRefs = %+v", got.OwnerReferences)
	}
}

// TestUCReservedSource_RemovedWithoutTheUpdateCenterReconciler is the teardown
// backstop: the reserved source must go when the singleton is absent even
// though UpdateCenterReconciler is not registered at all in that configuration.
func TestUCReservedSource_RemovedWithoutTheUpdateCenterReconciler(t *testing.T) {
	tc := newCatalogTestClient()
	// Deliberately NO UpdateCenter singleton, and no update-center URL — the
	// exact shape of a disabled update center.
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	crdstore.MustSeed(tc.store, &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name: "uc-orphan", Namespace: testOperatorNS,
			Labels:          map[string]string{catalogSourceLabel: updateCenterSourceName},
			OwnerReferences: []metav1.OwnerReference{ownerRef(src)},
		},
		Spec: v1alpha1.CatalogItemSpec{SourceRef: updateCenterSourceName},
	})

	r := newUCReconciler(t, tc, "")
	r.Reconcile(context.Background(), src)

	if _, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), tc.store, updateCenterSourceName, testOperatorNS); err == nil {
		t.Fatal("the reserved source should have been deleted")
	}
	// Items are owner-referenced to the source, so real-cluster GC removes
	// them; no code path deletes items directly on teardown.
	if _, err := crdstore.Get[v1alpha1.CatalogItem](context.Background(), tc.store, "uc-orphan", testOperatorNS); err != nil {
		t.Error("teardown must not delete items directly; GC owns that")
	}
}

// ---------------------------------------------------------------------------
// 7.7a — prune safety
// ---------------------------------------------------------------------------

func TestUCPrune_OnPluginRemoval(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "keep", Version: "1.0", SHA256: "sha256:k"},
		{Name: "drop", Version: "1.0", SHA256: "sha256:d"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	if got := len(derivedItems(t, tc)); got != 2 {
		t.Fatalf("expected 2 items after the first sync, got %d", got)
	}

	uc.entries = uc.entries[:1]
	forceSync(src)
	r.Reconcile(context.Background(), src)

	items := derivedItems(t, tc)
	if len(items) != 1 {
		t.Fatalf("expected the removed plugin's item to be pruned, got %v", items)
	}
	for _, it := range items {
		if it.Spec.Version != "1.0" || !strings.Contains(it.Spec.Path, "keep") {
			t.Errorf("wrong item survived: %+v", it.Spec)
		}
	}
}

// TestUCPrune_NothingPrunedOnFetchFailure is the highest-consequence test in
// the change. Prune deletes every item not synced this pass, so pruning against
// an unreachable store would delete the entire derived catalog on one transient
// 503 and break every ComposedBundle referencing it.
func TestUCPrune_NothingPrunedOnFetchFailure(t *testing.T) {
	for _, tc2 := range []struct {
		name   string
		status int
	}{
		{"503 from a partial store scan", http.StatusServiceUnavailable},
		{"500", http.StatusInternalServerError},
	} {
		t.Run(tc2.name, func(t *testing.T) {
			tc := newCatalogTestClient()
			seedSingleton(tc)
			uc := newUCTestServer(t)
			uc.entries = []inventoryEntry{{Name: "keep", Version: "1.0", SHA256: "sha256:k"}}
			r := newUCReconciler(t, tc, uc.URL)
			src := reservedSource(testOperatorNS)
			crdstore.MustSeed(tc.store, src)

			r.Reconcile(context.Background(), src)
			before := derivedItems(t, tc)
			if len(before) != 1 {
				t.Fatalf("precondition: expected 1 item, got %d", len(before))
			}
			okStatus := sourceStatus(t, tc)
			revBefore, lastSyncBefore := okStatus.ObservedRevision, okStatus.LastSyncTime

			uc.status = tc2.status
			forceSync(src)
			r.Reconcile(context.Background(), src)

			after := derivedItems(t, tc)
			if len(after) != len(before) {
				t.Fatalf("a fetch failure must prune NOTHING: %d -> %d", len(before), len(after))
			}
			st := sourceStatus(t, tc)
			if st.Phase != v1alpha1.CatalogSyncError {
				t.Errorf("phase = %q, want Error", st.Phase)
			}
			if st.ObservedRevision != revBefore {
				t.Errorf("observedRevision must be untouched on failure: %q -> %q", revBefore, st.ObservedRevision)
			}
			if st.LastSyncTime == nil || !st.LastSyncTime.Equal(lastSyncBefore) {
				t.Errorf("lastSyncTime must be untouched on failure: %v -> %v", lastSyncBefore, st.LastSyncTime)
			}
		})
	}
}

// TestUCPrune_WithheldOnPartialInventory asserts the consumer-side
// requirement: a 200 inventory response that discloses skippedPacks is a
// LOWER BOUND, not a full listing. Pruning against it would delete every item
// backed solely by the unreadable pack, so prune must be withheld for that
// pass while the readable subset still syncs and the source stays Ready.
func TestUCPrune_WithheldOnPartialInventory(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "keep", Version: "1.0", SHA256: "sha256:k"},
		{Name: "at-risk", Version: "1.0", SHA256: "sha256:a"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	if got := len(derivedItems(t, tc)); got != 2 {
		t.Fatalf("expected 2 items after the first sync, got %d", got)
	}

	// "at-risk" drops out of the readable listing because its only pack
	// became unreadable, and the scan discloses that pack as skipped.
	uc.entries = uc.entries[:1]
	uc.skipped = []ucSkippedPack{{Ref: "pack:v1", Error: "boom"}}
	forceSync(src)
	r.Reconcile(context.Background(), src)

	items := derivedItems(t, tc)
	if len(items) != 2 {
		t.Fatalf("a partial inventory must prune NOTHING: got %d items, want 2", len(items))
	}

	st := sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncReady {
		t.Errorf("phase = %q, want Ready: a partial inventory still lets the readable subset sync", st.Phase)
	}
	if !strings.Contains(st.Message, "pack:v1") {
		t.Errorf("status message must disclose the skipped pack ref, got %q", st.Message)
	}
}

func TestUCPrune_EmptyButSuccessfulResponsePrunesEverything(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "gone", Version: "1.0", SHA256: "sha256:g"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	if len(derivedItems(t, tc)) != 1 {
		t.Fatal("precondition")
	}

	// A legitimate empty store. The distinction from a failure is transport
	// success, not payload size.
	uc.entries = []inventoryEntry{}
	forceSync(src)
	r.Reconcile(context.Background(), src)

	if got := len(derivedItems(t, tc)); got != 0 {
		t.Fatalf("an empty successful response must prune everything, %d left", got)
	}
	if st := sourceStatus(t, tc); st.Phase != v1alpha1.CatalogSyncReady {
		t.Errorf("phase = %q, want Ready", st.Phase)
	}
}

func TestUCConflictingEntries_InvalidateOneItemAndKeepTheSourceReady(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "conflicted", Version: "1.0", SHA256: "sha256:a"},
		{Name: "conflicted", Version: "1.0", SHA256: "sha256:b"},
		{Name: "clean", Version: "2.0", SHA256: "sha256:c"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	r.Reconcile(context.Background(), src)

	st := sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncReady {
		t.Fatalf("one bad plugin must not take the catalog offline: phase %q", st.Phase)
	}
	if !strings.Contains(st.Message, "conflicting store entries") {
		t.Errorf("message should count the conflicts: %q", st.Message)
	}

	gvr, _ := crdstore.GVRFor[v1alpha1.CatalogItem]()
	var sawInvalid, sawValid bool
	for _, p := range tc.store.StatusPatches(gvr) {
		s, _ := p.Status.(*v1alpha1.CatalogItemStatus)
		if s == nil {
			continue
		}
		if !s.Valid && strings.Contains(s.Message, "conflicting entries") {
			sawInvalid = true
		}
		if s.Valid && strings.Contains(s.Content, "clean") {
			sawValid = true
		}
	}
	if !sawInvalid {
		t.Error("the conflicted plugin's item should be invalid with an explanatory message")
	}
	if !sawValid {
		t.Error("every other plugin should derive normally")
	}
}

// TestUCProfileReadFailure_AbortsBeforeAnyWriteOrPrune: resolving against a
// partial profile set could establish unanimity that does not exist and flip
// items valid or invalid on a transient API error.
func TestUCProfileReadFailure_AbortsBeforeAnyWriteOrPrune(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	// An eligible profile whose lock ConfigMap is missing from the fake client.
	crdstore.MustSeed(tc.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "lts"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.555", ResolveVersion: "2.555.3", Channel: "lts"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "lts-pluginset",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue},
			},
		},
	})
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "would-be-pruned", Version: "1.0", SHA256: "sha256:x"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	// Seed an existing item that a prune would delete.
	crdstore.MustSeed(tc.store, &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name: "uc-existing", Namespace: testOperatorNS,
			Labels:          map[string]string{catalogSourceLabel: updateCenterSourceName},
			OwnerReferences: []metav1.OwnerReference{ownerRef(src)},
		},
		Spec: v1alpha1.CatalogItemSpec{SourceRef: updateCenterSourceName},
	})

	r.Reconcile(context.Background(), src)

	st := sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncError || !strings.Contains(st.Message, "version profiles") {
		t.Fatalf("status = %+v", st)
	}
	if _, err := crdstore.Get[v1alpha1.CatalogItem](context.Background(), tc.store, "uc-existing", testOperatorNS); err != nil {
		t.Error("a profile read failure must not prune")
	}
	if got := len(derivedItems(t, tc)); got != 1 {
		t.Errorf("a profile read failure must not write either: %d items", got)
	}
	gvr, _ := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if patches := tc.store.StatusPatches(gvr); len(patches) != 0 {
		t.Errorf("no item status should have been patched, got %d", len(patches))
	}
}

// TestUCProfileParseFailure_AbortsBeforeAnyWriteOrPrune covers the parse half.
func TestUCProfileParseFailure_AbortsBeforeAnyWriteOrPrune(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	tc.configMapData = map[string]map[string]string{
		"lts-pluginset": {"plugins.yaml": "plugins: [this is not: valid yaml"},
	}
	crdstore.MustSeed(tc.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "lts"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.555", Channel: "lts"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "lts-pluginset",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue},
			},
		},
	})
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "p", Version: "1.0", SHA256: "sha256:x"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	if st := sourceStatus(t, tc); st.Phase != v1alpha1.CatalogSyncError {
		t.Fatalf("phase = %q, want Error", st.Phase)
	}
	if len(derivedItems(t, tc)) != 0 {
		t.Error("a lock parse failure must abort before any write")
	}
}

// TestUCSync_PopulatesClosureCompatAndCondition is the end-to-end shape check.
func TestUCSync_PopulatesClosureCompatAndCondition(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	tc.configMapData = map[string]map[string]string{
		"lts-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"1.0\"\n"},
	}
	crdstore.MustSeed(tc.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "lts"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.555", ResolveVersion: "2.555.3", Channel: "lts"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "lts-pluginset",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue},
			},
		},
	})
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "acme-widget", Version: "1.2.0", SHA256: "sha256:a", RequiredCore: "2.479.1",
			Dependencies: []pluginDep{{Name: "mailer", Min: "2.0"}}},
		{Name: "mailer", Version: "2.0", SHA256: "sha256:m"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	r.Reconcile(context.Background(), src)

	gvr, _ := crdstore.GVRFor[v1alpha1.CatalogItem]()
	var widget *v1alpha1.CatalogItemStatus
	for _, p := range tc.store.StatusPatches(gvr) {
		s, _ := p.Status.(*v1alpha1.CatalogItemStatus)
		if s != nil && strings.Contains(s.Content, "acme-widget") {
			widget = s
		}
	}
	if widget == nil {
		t.Fatal("no status patch for the acme-widget item")
	}
	if !widget.Valid || len(widget.Closure) != 2 {
		t.Fatalf("status = %+v", widget)
	}
	if len(widget.Compat) != 1 || widget.Compat[0].Profile != "lts" {
		t.Fatalf("compat = %+v", widget.Compat)
	}
	// The profile's lock pins mailer at 1.0, below the effective minimum 2.0.
	if widget.Compat[0].Verdict != verdictLockTooOld {
		t.Errorf("verdict = %q, want lock-too-old", widget.Compat[0].Verdict)
	}
	if len(widget.Conditions) != 1 || widget.Conditions[0].Type != compatWarningCondition {
		t.Fatalf("conditions = %+v", widget.Conditions)
	}
	if widget.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("CompatWarning should be True: %+v", widget.Conditions[0])
	}
	// Advisory only: the item is still valid and still has content.
	if !widget.Valid || widget.Content == "" {
		t.Error("a verdict must never invalidate an item")
	}
}

// ---------------------------------------------------------------------------
// 6.6 — ownership guard
// ---------------------------------------------------------------------------

func foreignItem(name, ns string) *v1alpha1.CatalogItem {
	controller := true
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{catalogSourceLabel: "alpha"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "CatalogSource",
				Name: "alpha", UID: types.UID("alpha-uid"),
				Controller: &controller, BlockOwnerDeletion: &controller,
			}},
		},
		Spec:   v1alpha1.CatalogItemSpec{SourceRef: "alpha", Type: v1alpha1.CatalogItemJCasC},
		Status: v1alpha1.CatalogItemStatus{Valid: true, Content: "owned-by-alpha"},
	}
}

func TestOwnershipGuard_ForeignItemNotOverwrittenAndNotPruned(t *testing.T) {
	tc := newCatalogTestClient()
	beta := &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "ns", UID: types.UID("beta-uid")},
	}
	crdstore.MustSeed(tc.store, foreignItem("contended", "ns"))
	crdstore.MustSeed(tc.store, foreignItem("untouched", "ns"))

	r := newUCReconciler(t, tc, "")
	desired := map[string]bool{}
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name: "contended", Namespace: "ns",
			Labels:          map[string]string{catalogSourceLabel: "beta"},
			OwnerReferences: []metav1.OwnerReference{ownerRef(beta)},
		},
		Spec: v1alpha1.CatalogItemSpec{SourceRef: "beta"},
	}
	warnings, failed := r.writeCatalogItem(context.Background(), beta, item, desired)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "owned by another source") {
		t.Fatalf("warnings = %v", warnings)
	}
	// An ownership skip is permanent, not a retry-worthy write failure.
	if failed {
		t.Error("an ownership skip must not report a failed write")
	}
	live, err := crdstore.Get[v1alpha1.CatalogItem](context.Background(), tc.store, "contended", "ns")
	if err != nil {
		t.Fatal(err)
	}
	if live.Spec.SourceRef != "alpha" {
		t.Errorf("foreign item was overwritten: %+v", live.Spec)
	}
	// It is in desired, so it is not a prune candidate either …
	if !desired["contended"] {
		t.Error("a skipped item must still be recorded as desired, or prune deletes it")
	}
	// … and prune skips foreign items regardless.
	r.pruneCatalogItems(context.Background(), beta, map[string]bool{})
	for _, name := range []string{"contended", "untouched"} {
		if _, err := crdstore.Get[v1alpha1.CatalogItem](context.Background(), tc.store, name, "ns"); err != nil {
			t.Errorf("prune deleted the foreign item %q", name)
		}
	}
}

// TestOwnershipGuard_FailedWriteDoesNotDeleteItsOwnItem covers the pre-existing
// same-pass self-prune: desired was recorded only AFTER a successful apply, so
// a transient error made that very item a prune candidate in the same pass.
func TestOwnershipGuard_FailedWriteDoesNotDeleteItsOwnItem(t *testing.T) {
	tc := newCatalogTestClient()
	src := &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "ns", UID: types.UID("beta-uid")},
	}
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mine", Namespace: "ns",
			Labels:          map[string]string{catalogSourceLabel: "beta"},
			OwnerReferences: []metav1.OwnerReference{ownerRef(src)},
		},
		Spec: v1alpha1.CatalogItemSpec{SourceRef: "beta"},
	}
	crdstore.MustSeed(tc.store, item)

	gvr, _ := crdstore.GVRFor[v1alpha1.CatalogItem]()
	tc.store.FailAlways("update", gvr, errors.New("transient"))

	r := newUCReconciler(t, tc, "")
	desired := map[string]bool{}
	_, failed := r.writeCatalogItem(context.Background(), src, item, desired)
	if !failed {
		t.Error("a failed apply must report a retry-worthy write failure")
	}
	r.pruneCatalogItems(context.Background(), src, desired)

	if _, err := crdstore.Get[v1alpha1.CatalogItem](context.Background(), tc.store, "mine", "ns"); err != nil {
		t.Fatal("a failed write must not delete its own item in the same pass")
	}
}

func TestOwnedBy_RequiresBothLabelAndControllerUID(t *testing.T) {
	controller := true
	src := &v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "beta", UID: types.UID("beta-uid")}}
	good := []metav1.OwnerReference{{Name: "beta", UID: "beta-uid", Controller: &controller}}

	if !itemOwnedBy(map[string]string{catalogSourceLabel: "beta"}, good, src) {
		t.Error("both conjuncts satisfied should be owned")
	}
	if itemOwnedBy(map[string]string{catalogSourceLabel: "alpha"}, good, src) {
		t.Error("a foreign label must lose, even with a matching UID")
	}
	if itemOwnedBy(map[string]string{catalogSourceLabel: "beta"}, nil, src) {
		t.Error("the label alone is user-writable and must not suffice")
	}
	notController := false
	stale := []metav1.OwnerReference{{Name: "beta", UID: "old-uid", Controller: &controller}}
	if itemOwnedBy(map[string]string{catalogSourceLabel: "beta"}, stale, src) {
		t.Error("a stale UID must lose")
	}
	nonController := []metav1.OwnerReference{{Name: "beta", UID: "beta-uid", Controller: &notController}}
	if itemOwnedBy(map[string]string{catalogSourceLabel: "beta"}, nonController, src) {
		t.Error("a non-controller ownerRef must not count")
	}
}

func TestJoinWarnings_CapsAtFive(t *testing.T) {
	if got := joinWarnings(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
	ws := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := joinWarnings(ws)
	if !strings.HasPrefix(got, "a; b; c; d; e") || !strings.HasSuffix(got, "(+2 more)") {
		t.Errorf("joinWarnings = %q", got)
	}
}

func TestUCItemName_IsAPureFunctionAndCollisionFree(t *testing.T) {
	a := ucItemName("acme_widget", "1.0")
	b := ucItemName("acme-widget", "1.0")
	if a == b {
		t.Errorf("slugging must not collide two distinct plugins: %q", a)
	}
	if a != ucItemName("acme_widget", "1.0") {
		t.Error("derivation must be a pure function of (name, version)")
	}
	long := ucItemName(strings.Repeat("x", 400), strings.Repeat("y", 400))
	if len(long) > 253 {
		t.Errorf("name length %d exceeds 253", len(long))
	}
	if !strings.HasSuffix(long, ucItemName(strings.Repeat("x", 400), strings.Repeat("y", 400))[len(long)-10:]) {
		t.Error("the hash must be retained on truncation")
	}
}

// ---------------------------------------------------------------------------
// The redundant-derivation skip gate
// ---------------------------------------------------------------------------

// ucItemPatchCount is one status patch per derived item per full pass, so it
// distinguishes a pass that derived everything from one that skipped.
func ucItemPatchCount(t *testing.T, tc *catalogTestClient) int {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatal(err)
	}
	return len(tc.store.StatusPatches(gvr))
}

// elapseSync makes the next Reconcile due the way the clock does. The gate
// tests must not use forceSync: the annotation is a manual sync request, which
// deliberately bypasses the skip gate.
func elapseSync(src *v1alpha1.CatalogSource) {
	past := metav1.NewTime(time.Now().Add(-2 * time.Duration(defaultSyncIntervalSec) * time.Second))
	src.Status.LastSyncTime = &past
}

// ucReadyProfile is an eligible profile plus the lock ConfigMap it points at.
func ucReadyProfile(tc *catalogTestClient, lockYAML string) {
	if tc.configMapData == nil {
		tc.configMapData = map[string]map[string]string{}
	}
	tc.configMapData["lts-pluginset"] = map[string]string{"plugins.yaml": lockYAML}
	crdstore.MustSeed(tc.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "lts"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.555", Channel: "lts"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "lts-pluginset",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue},
			},
		},
	})
}

// TestUCSync_UnchangedInputsSkipDerivation asserts that deriving items must
// be skipped when nothing upstream moved. Deriving is this arm's whole cost —
// one apply and one status patch per plugin, through the single dynamic
// client that carries all of the operator's CRD traffic — and repeating it
// every pass stretches the catalog tick to minutes, starving the
// ComposedBundle reconciliation that runs behind the sources in that same
// tick.
func TestUCSync_UnchangedInputsSkipDerivation(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "a", Version: "1.0", SHA256: "sha256:a"},
		{Name: "b", Version: "2.0", SHA256: "sha256:b"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	afterFirst := ucItemPatchCount(t, tc)
	if afterFirst == 0 {
		t.Fatal("the first pass must derive items")
	}
	if got := len(derivedItems(t, tc)); got != 2 {
		t.Fatalf("expected 2 derived items, got %d", got)
	}

	elapseSync(src)
	r.Reconcile(context.Background(), src)

	if got := ucItemPatchCount(t, tc); got != afterFirst {
		t.Errorf("second pass rewrote %d items; an unchanged inventory must derive nothing", got-afterFirst)
	}
	if got := len(derivedItems(t, tc)); got != 2 {
		t.Errorf("skipping must leave existing items alone, got %d", got)
	}
	st := sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncReady {
		t.Errorf("phase = %q, want Ready", st.Phase)
	}
	if st.LastSyncTime == nil {
		t.Error("a skipped pass must still advance lastSyncTime; isSyncDue keys off it")
	}
}

// TestUCSync_ProfileChangeDefeatsTheSkipGate guards the reason ucSyncDigest
// covers profiles and not just the inventory. resolveClosure and
// evaluateCompat both read profiles, so a lock edit changes derived content
// while the inventory is byte-identical. Digesting the inventory alone would
// skip this pass and leave every item stale.
func TestUCSync_ProfileChangeDefeatsTheSkipGate(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	ucReadyProfile(tc, "plugins:\n  - artifactId: a\n    version: 1.0\n")
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(context.Background(), src)
	afterFirst := ucItemPatchCount(t, tc)
	if afterFirst == 0 {
		t.Fatal("the first pass must derive items")
	}

	// Same inventory, different pin.
	tc.configMapData["lts-pluginset"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: a\n    version: 2.0\n",
	}
	elapseSync(src)
	r.Reconcile(context.Background(), src)

	if got := ucItemPatchCount(t, tc); got == afterFirst {
		t.Error("a profile lock change must re-derive; the digest cannot cover the inventory alone")
	}
}

// TestUCSync_RepairPassAfterTheFullPassInterval covers what the digest cannot
// see. An item deleted out of band leaves the inventory and profiles
// untouched, so the gate holds and the item stays missing — until the bounded
// repair pass runs regardless of the digest.
func TestUCSync_RepairPassAfterTheFullPassInterval(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(ctx, src)
	items := derivedItems(t, tc)
	if len(items) != 1 {
		t.Fatalf("expected 1 derived item, got %d", len(items))
	}
	var name string
	for _, it := range items {
		name = it.Name
	}

	if err := crdstore.Delete[v1alpha1.CatalogItem](ctx, tc.store, name, testOperatorNS); err != nil {
		t.Fatalf("delete derived item: %v", err)
	}
	if got := len(derivedItems(t, tc)); got != 0 {
		t.Fatalf("precondition: the delete did not take, %d items remain", got)
	}

	// Digest unchanged, repair not yet due: the gate holds.
	elapseSync(src)
	r.Reconcile(ctx, src)
	if got := len(derivedItems(t, tc)); got != 0 {
		t.Fatalf("expected the gate to hold while the digest is unchanged, got %d items", got)
	}

	// Once the repair window lapses the pass runs regardless of the digest.
	r.ucLastFullPass = time.Now().Add(-ucFullPassInterval - time.Minute)
	elapseSync(src)
	r.Reconcile(ctx, src)
	if got := len(derivedItems(t, tc)); got != 1 {
		t.Errorf("the repair pass must restore the out-of-band deletion, got %d items", got)
	}
}

// TestUCSync_ManualRequestBypassesTheSkipGate protects the sync button. The
// REST surface requests a sync by stamping syncRequestedAtAnno, and someone
// pressing it is usually chasing exactly the out-of-band drift the digest
// cannot see — so the gate must not answer with a no-op.
func TestUCSync_ManualRequestBypassesTheSkipGate(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(ctx, src)
	items := derivedItems(t, tc)
	if len(items) != 1 {
		t.Fatalf("expected 1 derived item, got %d", len(items))
	}
	var name string
	for _, it := range items {
		name = it.Name
	}
	if err := crdstore.Delete[v1alpha1.CatalogItem](ctx, tc.store, name, testOperatorNS); err != nil {
		t.Fatalf("delete derived item: %v", err)
	}

	// Digest unchanged and the repair window is nowhere near due, so only the
	// manual request can drive this pass.
	forceSync(src)
	r.Reconcile(ctx, src)

	if got := len(derivedItems(t, tc)); got != 1 {
		t.Errorf("a requested sync must derive regardless of the digest, got %d items", got)
	}
}

// TestInventoryDigest_FieldBoundariesAreUnambiguous guards the encoding. These
// two inventories are genuinely different — the same text splits differently
// across displayName and description — but a digest that joins fields with a
// separator renders both as "p@q@r" and calls them equal. Since the digest
// decides whether reconcileUpdateCenterSource derives anything, a collision is
// a silently missed update.
func TestInventoryDigest_FieldBoundariesAreUnambiguous(t *testing.T) {
	a := []inventoryEntry{{
		Name: "n", Version: "1.0", SHA256: "sha256:x",
		DisplayName: "p@q", Description: "r",
	}}
	b := []inventoryEntry{{
		Name: "n", Version: "1.0", SHA256: "sha256:x",
		DisplayName: "p", Description: "q@r",
	}}
	if inventoryDigest(a) == inventoryDigest(b) {
		t.Error("distinct inventories produced the same digest; field boundaries are ambiguous")
	}
}

// TestInventoryDigest_StableAcrossDependencyOrder is the other half: a
// reordered dependency listing is the same content and must not force a
// needless derive.
func TestInventoryDigest_StableAcrossDependencyOrder(t *testing.T) {
	a := []inventoryEntry{{
		Name: "n", Version: "1.0", SHA256: "sha256:x",
		Dependencies: []pluginDep{{Name: "b", Min: "2.0"}, {Name: "a", Min: "1.0"}},
	}}
	b := []inventoryEntry{{
		Name: "n", Version: "1.0", SHA256: "sha256:x",
		Dependencies: []pluginDep{{Name: "a", Min: "1.0"}, {Name: "b", Min: "2.0"}},
	}}
	if inventoryDigest(a) != inventoryDigest(b) {
		t.Error("dependency order changed the digest; reordering is not a content change")
	}
}

// TestInventoryDigest_MetadataChangeMovesTheDigest is the finding that started
// this: buildUCItem, resolveClosure and evaluateCompat all read entry metadata,
// so a pack republished with corrected dependencies at an unchanged
// (name, version, sha256) must still be seen.
func TestInventoryDigest_MetadataChangeMovesTheDigest(t *testing.T) {
	base := inventoryEntry{Name: "n", Version: "1.0", SHA256: "sha256:x"}
	withDep := base
	withDep.Dependencies = []pluginDep{{Name: "a", Min: "1.0"}}
	withCore := base
	withCore.RequiredCore = "2.555"

	if inventoryDigest([]inventoryEntry{base}) == inventoryDigest([]inventoryEntry{withDep}) {
		t.Error("a dependency change must move the digest")
	}
	if inventoryDigest([]inventoryEntry{base}) == inventoryDigest([]inventoryEntry{withCore}) {
		t.Error("a requiredCore change must move the digest")
	}
}

// TestUCSync_FailedWriteDoesNotArmTheSkipGate keeps the gate from swallowing a
// retry: writeCatalogItem must surface an apply failure to the caller, or a
// transiently-failed item waits for the 30-minute repair pass instead of the
// next tick.
func TestUCSync_FailedWriteDoesNotArmTheSkipGate(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "a", Version: "1.0", SHA256: "sha256:a"},
		{Name: "b", Version: "2.0", SHA256: "sha256:b"},
	}
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatal(err)
	}
	tc.store.FailNext("patchstatus", gvr, errors.New("transient"))

	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(ctx, src)
	afterFirst := ucItemPatchCount(t, tc)

	// Digest is unchanged, so only the un-armed gate can drive this pass.
	elapseSync(src)
	r.Reconcile(ctx, src)

	if ucItemPatchCount(t, tc) == afterFirst {
		t.Error("a pass with a failed write must not arm the gate; the next tick has to retry")
	}
	if got := len(derivedItems(t, tc)); got != 2 {
		t.Errorf("the retry should have landed both items, got %d", got)
	}
}

// TestUCSync_SkipAfterErrorClearsTheStaleMessage: setError leaves its text in
// status.message, and the skip path sets Ready without deriving, so the two
// would otherwise be reported together until the repair pass.
func TestUCSync_SkipAfterErrorClearsTheStaleMessage(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	r.Reconcile(ctx, src)
	if st := sourceStatus(t, tc); st.Phase != v1alpha1.CatalogSyncReady {
		t.Fatalf("first pass phase = %q, want Ready", st.Phase)
	}

	// A transient inventory failure records an error and leaves the digest.
	uc.status = http.StatusServiceUnavailable
	elapseSync(src)
	r.Reconcile(ctx, src)
	st := sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncError || st.Message == "" {
		t.Fatalf("expected an error pass, got %+v", st)
	}

	// Recovery with an unchanged digest takes the skip path.
	uc.status = 0
	elapseSync(src)
	r.Reconcile(ctx, src)

	st = sourceStatus(t, tc)
	if st.Phase != v1alpha1.CatalogSyncReady {
		t.Errorf("phase = %q, want Ready", st.Phase)
	}
	if st.Message != "" {
		t.Errorf("Ready still carries the superseded error text: %q", st.Message)
	}
}

// TestUCSync_PartialFailureDisarmsAnEarlierArm: setReady records the current
// revision's digest even when a write failed, so an earlier successful
// revision's timestamp must not be left standing — the next pass would skip on
// a digest that was never fully applied.
func TestUCSync_PartialFailureDisarmsAnEarlierArm(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)

	// Revision A lands cleanly and arms the gate.
	r.Reconcile(ctx, src)
	if r.ucLastFullPass.IsZero() {
		t.Fatal("a clean pass should arm the gate")
	}

	// Revision B moves the digest and partially fails.
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatal(err)
	}
	uc.entries = append(uc.entries, inventoryEntry{Name: "b", Version: "2.0", SHA256: "sha256:b"})
	tc.store.FailNext("patchstatus", gvr, errors.New("transient"))
	elapseSync(src)
	r.Reconcile(ctx, src)

	if !r.ucLastFullPass.IsZero() {
		t.Error("a partially-failed pass must disarm the gate, not inherit the previous arm")
	}

	// Same digest as B: only a disarmed gate can drive this pass.
	afterB := ucItemPatchCount(t, tc)
	elapseSync(src)
	r.Reconcile(ctx, src)
	if ucItemPatchCount(t, tc) == afterB {
		t.Error("the pass after a partial failure must re-derive")
	}
}

// TestUCSync_PruneFailureBlocksTheGate: an incomplete GC leaves a stale item
// selectable, so it is retried on the next tick like a failed write.
func TestUCSync_PruneFailureBlocksTheGate(t *testing.T) {
	ctx := context.Background()
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		{Name: "keep", Version: "1.0", SHA256: "sha256:k"},
		{Name: "drop", Version: "1.0", SHA256: "sha256:d"},
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	r.Reconcile(ctx, src)

	// Removing a plugin makes the next pass prune — and that delete fails.
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatal(err)
	}
	uc.entries = uc.entries[:1]
	tc.store.FailNext("delete", gvr, errors.New("transient"))
	elapseSync(src)
	r.Reconcile(ctx, src)

	if !r.ucLastFullPass.IsZero() {
		t.Error("a failed prune must leave the gate disarmed so the GC is retried next tick")
	}
}

// TestUCSyncDigest_SkippedPacksParticipate: skippedPacks decides whether a pass
// prunes and what warning it carries, so a pack becoming readable again is a
// change even when the readable plugin set is byte-identical.
func TestUCSyncDigest_SkippedPacksParticipate(t *testing.T) {
	entries := []inventoryEntry{{Name: "a", Version: "1.0", SHA256: "sha256:a"}}
	clean := ucSyncDigest(entries, nil, nil)
	degraded := ucSyncDigest(entries, []ucSkippedPack{{Ref: "oci://packs/x:1"}}, nil)
	if clean == degraded {
		t.Error("an unreadable pack must move the digest even with an identical readable set")
	}
}
