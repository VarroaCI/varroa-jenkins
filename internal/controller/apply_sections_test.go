package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// applyResultRefusing answers an apply result for hash in which Jenkins
// refused each named section with error text. Every other section is ok.
func applyResultRefusing(hash string, refused ...string) *v1alpha1.ApplyResult {
	ar := &v1alpha1.ApplyResult{Hash: hash, Succeeded: len(refused) == 0}
	for _, name := range []string{"config", "rbac", "plugins", "items"} {
		s := v1alpha1.ApplySectionResult{Name: name, OK: true}
		for _, r := range refused {
			if r == name {
				s = v1alpha1.ApplySectionResult{Name: name, Error: "reload failed: refused; rolled back to last-good"}
			}
		}
		ar.Sections = append(ar.Sections, s)
	}
	return ar
}

func TestBundleSectionsApplied(t *testing.T) {
	cases := []struct {
		name string
		ar   *v1alpha1.ApplyResult
		want bool
	}{
		{"no result", nil, false},
		{"every section ok", applyResultRefusing("h"), true},
		{"succeeded with no sections", &v1alpha1.ApplyResult{Succeeded: true}, true},
		{"failed with no sections", &v1alpha1.ApplyResult{}, false},
		{"rbac refused alone", applyResultRefusing("h", "rbac"), true},
		{"config refused", applyResultRefusing("h", "config"), false},
		{"items refused", applyResultRefusing("h", "items"), false},
		{"config and rbac refused", applyResultRefusing("h", "config", "rbac"), false},
	}
	for _, tc := range cases {
		if got := bundleSectionsApplied(tc.ar); got != tc.want {
			t.Errorf("%s: bundleSectionsApplied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRbacRefusedFor(t *testing.T) {
	notReady := applyResultRefusing("h", "rbac")
	notReady.Sections[1].Error = ""
	cases := []struct {
		name string
		ar   *v1alpha1.ApplyResult
		hash string
		want bool
	}{
		{"no result", nil, "h", false},
		{"rbac refused for this hash", applyResultRefusing("h", "rbac"), "h", true},
		{"rbac refused for another hash", applyResultRefusing("h0", "rbac"), "h", false},
		{"no desired hash", applyResultRefusing("", "rbac"), "", false},
		{"rbac accepted", applyResultRefusing("h", "config"), "h", false},
		{"rbac failed with no error text", notReady, "h", false},
	}
	for _, tc := range cases {
		if got := rbacRefusedFor(tc.ar, tc.hash); got != tc.want {
			t.Errorf("%s: rbacRefusedFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHandleConnected_RefusedRBACWaitsForTheDriftWindow drives a push whose
// rbac.yaml Jenkins refuses. The snapshot keeps the last accepted RBAC, so
// the refusal verdict alone stops a push on every tick. The drift window, a
// changed RBAC document, a changed bundle, and a new mite epoch still push.
func TestHandleConnected_RefusedRBACWaitsForTheDriftWindow(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	tr := &scriptedSendTransport{Transport: &captureTransport{}}
	rec.miteTransport = tr
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now()),
			Annotations: map[string]string{"varroa.dev/allow-admin-lockout": "true"},
		},
		Spec: v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}
	const rbacRefusal = "reload failed: Unclosed counted closure; rolled back to last-good"
	tick := func(results ...*mitev1.CommandResult) int {
		t.Helper()
		before := tr.sends
		tr.results = results
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return tr.sends - before
	}
	refuse := func() *mitev1.CommandResult {
		return mkDSR(true, false, true, true, "", rbacRefusal, "", "", tr.last.DesiredStateHash)
	}

	if n := tick(); n != 1 || tr.last.RbacYaml == "" {
		t.Fatalf("first tick: %d pushes, want one push with rbac.yaml", n)
	}
	if n := tick(refuse()); n != 0 {
		t.Fatalf("refusal tick: %d pushes, want none: the verdict covers this desired state", n)
	}
	if n := tick(); n != 0 {
		t.Fatalf("tick in the drift window: %d pushes, want none", n)
	}

	old := metav1.NewTime(time.Now().Add(-6 * time.Minute))
	cr.Status.LastDesiredPushAt = &old
	if n := tick(); n != 1 {
		t.Fatalf("tick after the drift window: %d pushes, want one", n)
	}
	tick(refuse())

	rbacBefore := tr.last.RbacYaml
	rec.rbacGenerator = rbac.NewGenerator(seedResolverWithHumanAdmin())
	if n := tick(); n != 1 || tr.last.RbacYaml == rbacBefore {
		t.Fatalf("changed rbac.yaml: %d pushes, want one push with the new document", n)
	}
	tick(refuse())

	client.configMapData["test-bundle-content"]["jenkins.yaml"] = "jenkins:\n  systemMessage: \"Changed\""
	if n := tick(); n != 1 {
		t.Fatalf("changed bundle: %d pushes, want one", n)
	}
	tick(refuse())

	tr.epoch = 7
	if n := tick(); n != 1 {
		t.Fatalf("new mite epoch: %d pushes, want one", n)
	}
}
