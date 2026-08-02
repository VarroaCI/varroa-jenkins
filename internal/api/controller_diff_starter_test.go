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

// A Controller with no composedBundleRef runs the built-in starter bundle. The
// diff handler used to skip bundle resolution entirely for a nil ref, so the
// Config Pipeline UI showed an empty incoming configuration for a controller
// that was in fact converging on the starter — the diff claimed Varroa intended
// to apply nothing.
func TestControllerDiff_ResolvesStarterBundleForNilRef(t *testing.T) {
	client := newBundleTestClient()
	client.configMaps = map[string]map[string]string{
		"varroa-starter-content": {
			"jenkins.yaml": "jenkins:\n  systemMessage: \"starter\"\n",
			"items.yaml":   "items:\n  - kind: pipeline\n    name: hello-varroa\n",
			"plugins.yaml": "",
		},
	}
	store := storeFromBundleClient(client)

	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "team-a"},
	})
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.StarterBundleName, Namespace: "varroa-system"},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "varroa-starter-content",
		},
	})

	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             store,
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/bare/diff", nil)
	srv.handleControllerDiff(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)),
		"core", "team-a", "bare")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Incoming struct {
			Jcasc string `json:"jcasc"`
			Items string `json:"items"`
		} `json:"incoming"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	if !strings.Contains(resp.Incoming.Jcasc, "systemMessage") {
		t.Errorf("incoming jcasc should carry the starter content, got %q", resp.Incoming.Jcasc)
	}
	if !strings.Contains(resp.Incoming.Items, "hello-varroa") {
		t.Errorf("incoming items should carry the starter content, got %q", resp.Incoming.Items)
	}
}

// The dashboard PATCHes composedBundleRef to attach and detach bundles. If the
// resolved value were written into that field, the next save would materialize
// an explicit reference to the starter — pinning a zero-config controller
// behind the user's back. effectiveBundle exists so the UI can report what is
// running without that side effect.
func TestControllerDTO_EffectiveBundleIsSeparateFromSpecRef(t *testing.T) {
	client := newBundleTestClient()
	store := storeFromBundleClient(client)
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             store,
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})

	bare := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "team-a"}}
	eff := srv.effectiveBundleFor(bare)
	if eff == nil {
		t.Fatal("a zero-config controller must report an effective bundle")
	}
	if eff.Name != v1alpha1.StarterBundleName || eff.Namespace != "varroa-system" {
		t.Errorf("got %s/%s, want varroa-system/%s", eff.Namespace, eff.Name, v1alpha1.StarterBundleName)
	}
	if !eff.BuiltIn {
		t.Error("builtIn must be true so the UI can say 'built-in starter' rather than 'no bundle'")
	}
	// The spec field itself must stay untouched — it is what gets written back.
	if bare.Spec.ComposedBundleRef != nil {
		t.Error("resolving must not mutate the controller spec")
	}

	named := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "named", Namespace: "team-a"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "platform-baseline"},
		},
	}
	eff = srv.effectiveBundleFor(named)
	if eff == nil || eff.Name != "platform-baseline" || eff.Namespace != "team-a" {
		t.Fatalf("named ref should resolve to team-a/platform-baseline, got %+v", eff)
	}
	if eff.BuiltIn {
		t.Error("builtIn must be false for a controller that names its own bundle")
	}
}
