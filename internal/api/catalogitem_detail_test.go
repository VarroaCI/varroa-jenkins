package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// detailBrood serves one raw CatalogItem from the get route.
type detailBrood struct {
	scopeTestConfigBrood
	item json.RawMessage
}

func (d *detailBrood) GetCatalogItem(_ context.Context, _, _, _ string) (json.RawMessage, error) {
	return d.item, nil
}

func derivedTestItem() *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "uc-acme-widget-1-2-0-abc", Namespace: "varroa-system"},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef: updateCenterSourceRef,
			Type:      v1alpha1.CatalogItemPlugin,
			Version:   "1.2.0",
			Path:      "uc://acme-widget@1.2.0",
		},
		Status: v1alpha1.CatalogItemStatus{
			Valid: true,
			Closure: []v1alpha1.CatalogItemClosureEntry{
				{ArtifactID: "acme-widget", Version: "1.2.0", Direct: true, Provenance: "store"},
				{ArtifactID: "mailer", Version: "2.0", Provenance: "store", Minimum: "2.0"},
			},
		},
	}
}

func readyProfile(name, contentRef string) *v1alpha1.JenkinsVersionProfile {
	return &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.555", Channel: "lts"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: contentRef,
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue},
			},
		},
	}
}

func detailServer(t *testing.T, item *v1alpha1.CatalogItem, profiles []*v1alpha1.JenkinsVersionProfile,
	configMaps map[string]map[string]string, configMapErrs map[string]error) *Server {
	t.Helper()
	client := newBundleTestClient()
	client.configMaps = configMaps
	client.configMapErrs = configMapErrs
	store := storeFromBundleClient(client)
	for _, p := range profiles {
		crdstore.MustSeed(store, p)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(&Dependencies{
		Client:            client,
		Store:             store,
		Authorizer:        adminTestAuthorizer(),
		ConfigBrood:       &detailBrood{item: raw},
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
}

func getCatalogItemDetail(t *testing.T, srv *Server) CatalogItemDetailResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/catalogitems/varroa-system/uc-acme-widget-1-2-0-abc", nil)
	srv.dispatchCatalogItems(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)),
		"core", []string{"varroa-system", "uc-acme-widget-1-2-0-abc"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp CatalogItemDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	return resp
}

// TestCatalogItemDetail_LockPinsPresentForDerivedItem also pins the "no key
// means the lock does not mention it" rule, which is distinct from an equal pin.
func TestCatalogItemDetail_LockPinsPresentForDerivedItem(t *testing.T) {
	srv := detailServer(t, derivedTestItem(),
		[]*v1alpha1.JenkinsVersionProfile{readyProfile("lts", "lts-pluginset")},
		map[string]map[string]string{
			// The lock pins mailer but says nothing about acme-widget, and
			// carries an unrelated plugin the closure does not contain.
			"lts-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"1.0\"\n  - artifactId: unrelated\n    version: \"9.9\"\n"},
		}, nil)

	resp := getCatalogItemDetail(t, srv)
	if resp.Item.Name != "uc-acme-widget-1-2-0-abc" {
		t.Fatalf("item not carried under `item`: %+v", resp.Item.ObjectMeta)
	}
	if len(resp.LockPins) != 1 || resp.LockPins[0].Profile != "lts" {
		t.Fatalf("lockPins = %+v", resp.LockPins)
	}
	pins := resp.LockPins[0].Pins
	if pins["mailer"] != "1.0" {
		t.Errorf("mailer pin = %q, want 1.0", pins["mailer"])
	}
	if _, ok := pins["acme-widget"]; ok {
		t.Error("a plugin the lock does not mention must have NO key, not an empty one")
	}
	if _, ok := pins["unrelated"]; ok {
		t.Error("pins must be scoped to the item's closure")
	}
}

func TestCatalogItemDetail_LockPinsAbsentForGitSourcedItem(t *testing.T) {
	item := derivedTestItem()
	item.Spec.SourceRef = "platform-catalog"
	srv := detailServer(t, item,
		[]*v1alpha1.JenkinsVersionProfile{readyProfile("lts", "lts-pluginset")},
		map[string]map[string]string{
			"lts-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"1.0\"\n"},
		}, nil)

	resp := getCatalogItemDetail(t, srv)
	if len(resp.LockPins) != 0 {
		t.Fatalf("a git-sourced item must carry no lock-pin projection: %+v", resp.LockPins)
	}
}

func TestCatalogItemDetail_EmptyClosureCarriesNoLockPins(t *testing.T) {
	item := derivedTestItem()
	item.Status.Closure = nil
	srv := detailServer(t, item,
		[]*v1alpha1.JenkinsVersionProfile{readyProfile("lts", "lts-pluginset")},
		map[string]map[string]string{
			"lts-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"1.0\"\n"},
		}, nil)

	if resp := getCatalogItemDetail(t, srv); len(resp.LockPins) != 0 {
		t.Fatalf("lockPins = %+v", resp.LockPins)
	}
}

// TestCatalogItemDetail_UnreadableLockOmitsOnlyThatProfile: a detail view is a
// read-only convenience and must not fail because one profile is unhealthy.
// This is deliberately NOT the fail-before-write rule the sync loop applies.
func TestCatalogItemDetail_UnreadableLockOmitsOnlyThatProfile(t *testing.T) {
	srv := detailServer(t, derivedTestItem(),
		[]*v1alpha1.JenkinsVersionProfile{
			readyProfile("healthy", "healthy-pluginset"),
			readyProfile("broken", "broken-pluginset"),
		},
		map[string]map[string]string{
			"healthy-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"2.0\"\n"},
		},
		map[string]error{"broken-pluginset": errors.New("configmap unreachable")})

	resp := getCatalogItemDetail(t, srv)
	if len(resp.LockPins) != 1 || resp.LockPins[0].Profile != "healthy" {
		t.Fatalf("only the healthy profile should appear: %+v", resp.LockPins)
	}
}

// TestCatalogItemDetail_IneligibleProfileOmitted: showing pins from a stale lock
// is exactly the misinformation the verdict evaluator refuses to turn into a
// judgement, so the projection applies the same eligibility rule.
func TestCatalogItemDetail_IneligibleProfileOmitted(t *testing.T) {
	notReady := readyProfile("notready", "notready-pluginset")
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	noRef := readyProfile("noref", "")

	srv := detailServer(t, derivedTestItem(),
		[]*v1alpha1.JenkinsVersionProfile{notReady, noRef, readyProfile("ok", "ok-pluginset")},
		map[string]map[string]string{
			"notready-pluginset": {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"0.1\"\n"},
			"ok-pluginset":       {"plugins.yaml": "plugins:\n  - artifactId: mailer\n    version: \"2.0\"\n"},
		}, nil)

	resp := getCatalogItemDetail(t, srv)
	if len(resp.LockPins) != 1 || resp.LockPins[0].Profile != "ok" {
		t.Fatalf("ineligible profiles must be omitted entirely: %+v", resp.LockPins)
	}
}

// ---------------------------------------------------------------------------
// 6.7 — catalog items stay operator-written only
// ---------------------------------------------------------------------------

// TestCatalogItemRoutes_AreReadOnly is the enforcement point for the sole-writer
// seam. The reconciler is the only production writer of CatalogItem; if the BFF
// ever grew a write verb, the derivation loop would fight a second writer.
func TestCatalogItemRoutes_AreReadOnly(t *testing.T) {
	srv := detailServer(t, derivedTestItem(), nil, nil, nil)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, segs := range [][]string{{}, {"varroa-system", "uc-acme-widget-1-2-0-abc"}} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/clusters/core/catalogitems", nil)
			srv.dispatchCatalogItems(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", segs)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s with segments %v = %d, want 405 — catalog items must stay read-only",
					method, segs, w.Code)
			}
		}
	}

	// The get route still works, and returns the wrapper. The assertion above
	// is about write verbs, not the response shape.
	if resp := getCatalogItemDetail(t, srv); resp.Item.Name == "" {
		t.Error("the read route must still serve the item")
	}
}
