package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// guardVersions derives an unsafe (one minor below baseline) and a safe (well
// above baseline) version from the embedded plugin-lock baseline.
func guardVersions(t *testing.T) (unsafe, safe string) {
	t.Helper()
	segs, ok := jenkinsver.Core(pluginlock.Baseline())
	if !ok || len(segs) < 2 {
		t.Fatalf("unexpected baseline %q", pluginlock.Baseline())
	}
	return fmt.Sprintf("%d.%d", segs[0], segs[1]-1), fmt.Sprintf("%d.%d.1", segs[0], segs[1]+9)
}

// Task 8.3(a): an update that CHANGES the version to core<baseline (no profile)
// is blocked with the compat checks in the body.
func TestUpdateController_BlocksVersionChangeBelowBaseline(t *testing.T) {
	unsafe, safe := guardVersions(t)
	srv, client := newRoutingTestServer()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: safe},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	w := patchController(t, srv, fmt.Sprintf(`{"spec":{"version":%q}}`, unsafe))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "preflight failed") {
		t.Errorf("body should say preflight failed, got %s", body)
	}
	if !strings.Contains(body, "pluginCoreCompat") {
		t.Errorf("body should list the pluginCoreCompat check, got %s", body)
	}
}

// Task 8.3(b): an update that does NOT change a grandfathered unsafe version is
// allowed (ForUpdate leniency degrades the version fails to warn).
func TestUpdateController_AllowsUnchangedGrandfatheredVersion(t *testing.T) {
	unsafe, _ := guardVersions(t)
	srv, client := newRoutingTestServer()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: unsafe},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	// Patch an unrelated spec field; version stays the (already unsafe) prior value.
	w := patchController(t, srv, `{"spec":{"namespace":"test"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unchanged grandfathered version, got %d: %s", w.Code, w.Body.String())
	}
}

// Task 8.3(c): the self-name collision no longer fails under ForUpdate — a safe
// update to an existing controller succeeds even though a controller of that
// name already exists.
func TestUpdateController_SelfNameCollisionPasses(t *testing.T) {
	_, safe := guardVersions(t)
	srv, client := newRoutingTestServer()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: safe},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	w := patchController(t, srv, `{"spec":{"namespace":"test"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Task: create with an unsafe version (no profile) is blocked (create handler is
// behaviorally unchanged — it already blocks on any fail and picks up the new
// checks automatically).
func TestCreateController_BlocksVersionBelowBaseline(t *testing.T) {
	unsafe, _ := guardVersions(t)
	srv, _ := newRoutingTestServer()
	w := postCreateController(t, srv, fmt.Sprintf(`{"metadata":{"name":"ci"},"spec":{"version":%q}}`, unsafe))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pluginCoreCompat") {
		t.Errorf("body should list the pluginCoreCompat check, got %s", w.Body.String())
	}
}
