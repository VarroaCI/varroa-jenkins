package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestControllerDiff_HeadlessInjectsDoorLocationURL asserts that a headless
// controller's diff carries the dashboard door URL as location.url when the
// operator has a dashboard origin. The same controller without an origin
// keeps the in-cluster Service URL.
func TestControllerDiff_HeadlessInjectsDoorLocationURL(t *testing.T) {
	const wantDoor = "https://varroa.example.com/headless/core/team-a/cell/"

	run := func(t *testing.T, dashboardURL string) string {
		t.Helper()
		client := newBundleTestClient()
		client.configMaps = map[string]map[string]string{
			"headless-content": {
				"jenkins.yaml": "jenkins:\n  systemMessage: \"headless\"\n" +
					"unclassified:\n  location:\n    url: \"${varroa_controller_external_url}\"\n",
				"items.yaml":   "",
				"plugins.yaml": "",
			},
		}
		store := storeFromBundleClient(client)
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "cell", Namespace: "team-a", UID: "54c62aff-0000-0000-0000-000000000000"},
			Spec: v1alpha1.ControllerSpec{
				Headless:          true,
				ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "headless-bundle"},
			},
		}
		crdstore.MustSeed(store, cr)
		crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "headless-bundle", Namespace: "team-a"},
			Status: v1alpha1.ComposedBundleStatus{
				Phase:      v1alpha1.ComposedBundleReady,
				ContentRef: "headless-content",
			},
		})
		srv := NewServer(&Dependencies{
			Client:            client,
			Store:             store,
			Authorizer:        adminTestAuthorizer(),
			OperatorNamespace: "varroa-system",
			Logger:            slog.Default(),
			DashboardURL:      dashboardURL,
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/cell/diff", nil)
		srv.handleControllerDiff(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)),
			"core", "team-a", "cell")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Incoming struct {
				Jcasc string `json:"jcasc"`
			} `json:"incoming"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (body %s)", err, w.Body.String())
		}
		return resp.Incoming.Jcasc
	}

	t.Run("dashboard origin set", func(t *testing.T) {
		jcasc := run(t, "https://varroa.example.com")
		if !strings.Contains(jcasc, wantDoor) {
			t.Errorf("incoming jcasc should carry the door url %q, got %q", wantDoor, jcasc)
		}
	})

	t.Run("no dashboard origin", func(t *testing.T) {
		jcasc := run(t, "")
		if strings.Contains(jcasc, "/headless/") {
			t.Errorf("incoming jcasc carries a door url without a dashboard origin: %q", jcasc)
		}
		if !strings.Contains(jcasc, "svc.cluster.local:8080/") {
			t.Errorf("incoming jcasc should carry the in-cluster url, got %q", jcasc)
		}
	})
}
