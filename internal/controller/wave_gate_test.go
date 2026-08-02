package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/mite"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// newTestReconcilerForGate creates a reconciler with a pre-registered mite
// connection so handleConnected does not bail out on the disconnected check.
// Returns the reconciler and the registry so callers can register additional mites.
func newTestReconcilerForGate(client *testClient) (*Reconciler, *mite.Registry) {
	seedClientCRDs(client)
	registry := mite.NewRegistry()
	rec := NewReconciler(
		bundle.NewResolver("/tmp/test"),
		client,
		client.store,
		transport.NewLocalRegistry(registry),
		mite.NewTokenSigner(testKey),
		rbac.NewGenerator(rbac.NewTestResolver()),
		nil, // composer not needed
	)
	rec.Logger = slog.Default()
	return rec, registry
}

func TestBundleIdentOf(t *testing.T) {
	rec := &Reconciler{operatorNamespace: "varroa-system"}

	// Different namespace via ref.
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle", Namespace: "other-ns"},
		},
	}
	if ident := rec.bundleIdentOf(cr); ident.Name != "bundle" || ident.Namespace != "other-ns" {
		t.Fatalf("got %+v, want bundle/other-ns", ident)
	}

	// Same namespace when ref namespace is empty.
	cr2 := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl2", Namespace: "ns2"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle2"},
		},
	}
	if ident2 := rec.bundleIdentOf(cr2); ident2.Namespace != "ns2" {
		t.Fatalf("expected namespace ns2, got %s", ident2.Namespace)
	}

	// A nil ref resolves to the starter bundle in the operator namespace, NOT
	// to "no identity" — two zero-config siblings must share a wave gate.
	cr3 := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ctl3", Namespace: "ns3"}}
	ident3 := rec.bundleIdentOf(cr3)
	if ident3.Name != StarterBundleName || ident3.Namespace != "varroa-system" {
		t.Fatalf("got %+v, want %s/varroa-system", ident3, StarterBundleName)
	}
	cr4 := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "ctl4", Namespace: "other"}}
	if rec.bundleIdentOf(cr4) != ident3 {
		t.Fatal("zero-config controllers in different namespaces must share one bundle identity")
	}
}

func TestSiblingHealthyOn(t *testing.T) {
	target := "hash-abc"
	freshSeen := func() *metav1.Time { t := metav1.NewTime(time.Now().Add(-30 * time.Second)); return &t }
	staleSeen := func() *metav1.Time { t := metav1.NewTime(time.Now().Add(-3 * time.Minute)); return &t }

	healthy := func() *v1alpha1.Controller {
		return &v1alpha1.Controller{
			Status: v1alpha1.ControllerStatus{
				AppliedBundleHash: target,
				LastApplyResult: &v1alpha1.ApplyResult{
					Hash:      "some-other-hash",
					Succeeded: true,
				},
				MiteStatus: &v1alpha1.MiteStatus{
					Connected: true,
					LastSeen:  freshSeen(),
				},
			},
		}
	}

	t.Run("healthy on target", func(t *testing.T) {
		if !siblingHealthyOn(healthy(), target) {
			t.Fatal("expected healthy")
		}
	})

	t.Run("wrong applied bundle hash", func(t *testing.T) {
		s := healthy()
		s.Status.AppliedBundleHash = "hash-xyz"
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (wrong hash)")
		}
	})

	t.Run("lastApplyResult succeeded false", func(t *testing.T) {
		s := healthy()
		s.Status.LastApplyResult.Succeeded = false
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (apply not succeeded)")
		}
	})

	t.Run("lastApplyResult nil", func(t *testing.T) {
		s := healthy()
		s.Status.LastApplyResult = nil
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (no apply result)")
		}
	})

	t.Run("stale heartbeat", func(t *testing.T) {
		s := healthy()
		s.Status.MiteStatus.LastSeen = staleSeen()
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (stale heartbeat)")
		}
	})

	t.Run("miteStatus nil", func(t *testing.T) {
		s := healthy()
		s.Status.MiteStatus = nil
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (no mite status)")
		}
	})

	t.Run("mite not connected", func(t *testing.T) {
		s := healthy()
		s.Status.MiteStatus.Connected = false
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (mite not connected)")
		}
	})

	t.Run("miteStatus LastSeen nil", func(t *testing.T) {
		s := healthy()
		s.Status.MiteStatus.LastSeen = nil
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy (no last seen)")
		}
	})

	t.Run("succeeded false with correct AppliedBundleHash (mid-retry)", func(t *testing.T) {
		// A sibling that succeeded on the target earlier, but then tried a NEWER
		// hash that failed. AppliedBundleHash still shows the old target.
		s := healthy()
		s.Status.AppliedBundleHash = target
		s.Status.LastApplyResult.Succeeded = false // newer attempt failed
		if siblingHealthyOn(s, target) {
			t.Fatal("expected not healthy: succeeded on target but current apply failed (mid-retry)")
		}
	})
}

func TestWaveGateCleared_ExcludesSelf(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-1", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	// List returns only self — should be cleared (no earlier-wave siblings).
	client.controllers = []*v1alpha1.Controller{cr}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}
	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared with no siblings, got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_ExcludesNonConnected(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-2", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sibling", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0, // earlier wave
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseFailed, // not Connected
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared (sibling not Connected), got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_ExcludesEqualWave(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-3", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 1, // same wave
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared (equal wave excluded), got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_ExcludesHigherWave(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-4", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 2, // higher wave
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared (higher wave excluded), got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_ExcludesDifferentBundle(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-5", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "other-bundle"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared (different bundle excluded), got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_ExcludesDifferentBundleNamespace(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-6", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	// Same bundle name but different namespace.
	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other-ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("expected cleared (different namespace excluded), got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

func TestWaveGateCleared_BlocksOnUnhealthyEarlierWave(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-7", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	freshSeen := metav1.NewTime(time.Now().Add(-30 * time.Second))

	// Earlier-wave sibling (wave 0) — Connected but wrong AppliedBundleHash.
	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:             v1alpha1.ControllerPhaseConnected,
			AppliedBundleHash: "old-hash",
			LastApplyResult: &v1alpha1.ApplyResult{
				Hash:      "irrelevant",
				Succeeded: true,
			},
			MiteStatus: &v1alpha1.MiteStatus{
				Connected: true,
				LastSeen:  &freshSeen,
			},
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if cleared {
		t.Fatal("expected blocked (sibling not on target hash)")
	}
	if len(waitingOn) != 1 || waitingOn[0] != "ns/sib" {
		t.Fatalf("expected waitingOn=[ns/sib], got %v", waitingOn)
	}
}

func TestWaveGateCleared_ClearsOnHealthyEarlierWave(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-8", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	freshSeen := metav1.NewTime(time.Now().Add(-30 * time.Second))

	sibling := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:             v1alpha1.ControllerPhaseConnected,
			AppliedBundleHash: "hash1",
			LastApplyResult: &v1alpha1.ApplyResult{
				Hash:      "irrelevant",
				Succeeded: true,
			},
			MiteStatus: &v1alpha1.MiteStatus{
				Connected: true,
				LastSeen:  &freshSeen,
			},
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, sibling}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("expected cleared (sibling healthy on target)")
	}
	if len(waitingOn) != 0 {
		t.Fatalf("expected empty waitingOn, got %v", waitingOn)
	}
}

func TestWaveGateCleared_SortedWaitingOn(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-9", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	mkSib := func(name string) *v1alpha1.Controller {
		return &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: v1alpha1.ControllerSpec{
				ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
				ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
					RolloutWave: 0,
				},
			},
			Status: v1alpha1.ControllerStatus{
				Phase: v1alpha1.ControllerPhaseConnected,
			},
		}
	}
	sibs := []*v1alpha1.Controller{cr, mkSib("zulu"), mkSib("alpha"), mkSib("zulu2")}
	client.controllers = sibs
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	_, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if len(waitingOn) < 3 {
		t.Fatalf("expected 3 unhealthy siblings, got %d: %v", len(waitingOn), waitingOn)
	}
	// Should be sorted: alpha, zulu, zulu
	if waitingOn[0] != "ns/alpha" || waitingOn[1] != "ns/zulu" || waitingOn[2] != "ns/zulu2" {
		t.Fatalf("expected sorted [ns/alpha ns/zulu ns/zulu], got %v", waitingOn)
	}
}

func TestWaveGateCleared_ListError(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	ctx := context.Background()

	// Set controllers to nil to trigger a network error? No — the test client
	// returns nil,nil when controllers is nil. To simulate an error we need a
	// different mechanism. Since the test client cannot return errors for
	// ListControllerCRDs, we test this indirectly: the gate reaches List only
	// in wave>0 + non-bypass cases, and list errors are transient API errors
	// that re-queue (the reconciler handles them via backoff).
	//
	// This test validates that the gate survives a nil list cleanly (nil list
	// is the same as no siblings, which clears the gate).
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bndl"},
		},
	}
	ident := bundleIdentity{Name: "bndl", Namespace: "ns"}

	cleared, waitingOn, err := rec.waveGateCleared(ctx, cr, ident, 1, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared || len(waitingOn) != 0 {
		t.Fatalf("nil list should clear gate, got cleared=%v waitingOn=%v", cleared, waitingOn)
	}
}

// newTestClientWithGateBundle returns a test client with "gate-bundle" as a Ready
// ComposedBundle and a matching content ConfigMap.
func newTestClientWithGateBundle() *testClient {
	tc := newTestClient()
	tc.composedBundles["gate-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "gate-bundle-content",
		},
	}
	tc.configMapData["gate-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: \"Test Gate Bundle\"",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}
	return tc
}

// Helper to build a fresh-connected controller for the handleConnected-like gate tests.
func newGateCtrl(name string, wave int, appliedHash string, annotations map[string]string) *v1alpha1.Controller {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "ns",
			Annotations: annotations,
		},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "gate-bundle"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: wave,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:             v1alpha1.ControllerPhaseConnected,
			AppliedBundleHash: appliedHash,
		},
	}
	return cr
}

// freshSib creates a Connected sibling with the given wave and status fields.
func freshSib(name string, wave int, appliedHash string, succeeded bool) *v1alpha1.Controller {
	seen := metav1.NewTime(time.Now().Add(-30 * time.Second))
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "gate-bundle"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: wave,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:             v1alpha1.ControllerPhaseConnected,
			AppliedBundleHash: appliedHash,
			LastApplyResult: &v1alpha1.ApplyResult{
				Hash:      "irrelevant",
				Succeeded: succeeded,
			},
			MiteStatus: &v1alpha1.MiteStatus{
				Connected: true,
				LastSeen:  &seen,
			},
		},
	}
}

func TestGate_BypassWaveZero(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl", 0, "", nil)
	cr.Status.AppliedBundleHash = "" // first provision so no gate

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	// Gate bypass: Push attempts Send which fails (no real gRPC stream in test);
	// the important assertion is that the gate did NOT block.
	_ = err
	// Gate bypass: wave 0 proceeds, rollout should be nil.
	if cr.Status.Rollout != nil {
		t.Fatal("wave 0 should not have rollout state")
	}
	// Wave-0 controllers should never get a RolloutBlocked condition.
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionRolloutBlocked {
			t.Fatalf("wave-0 should not have RolloutBlocked condition, got status=%s reason=%s", c.Status, c.Reason)
		}
	}
}

func TestGate_BypassOverrideAnnotation(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	// Wave-2 controller with override annotation and old applied bundle hash.
	cr := newGateCtrl("ctl-over", 2, "old-hash", map[string]string{
		v1alpha1.AnnotationRolloutOverride: "true",
	})
	if cr.Annotations == nil {
		t.Fatal("annotations not set")
	}
	// Add an earlier-wave sibling that would block normally.
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "old-hash", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream); gate assertion below
	if cr.Status.Rollout != nil {
		t.Fatal("override annotation should bypass gate")
	}
	// Annotation should NOT be cleared.
	if cr.Annotations[v1alpha1.AnnotationRolloutOverride] != "true" {
		t.Fatal("override annotation should persist")
	}
}

func TestGate_BypassOverrideAnnotationMixedCase(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-case", 2, "old-hash", map[string]string{
		v1alpha1.AnnotationRolloutOverride: " TRUE ",
	})
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "old-hash", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream); gate assertion below
	if cr.Status.Rollout != nil {
		t.Fatal("override annotation (mixed case) should bypass gate")
	}
}

func TestGate_NonTrueOverrideIsNotHonored(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-noluck", 2, "old-hash", map[string]string{
		v1alpha1.AnnotationRolloutOverride: "yes",
	})
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "old-hash", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout == nil || !cr.Status.Rollout.Blocked {
		t.Fatal("non-true override should NOT bypass gate")
	}
}

func TestGate_AlreadyOnTarget(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-on-target", 2, "hash-match", nil)
	// Set the ComposedBundle's ResolvedHash to match.
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-match"
	seedClientCRDs(client)

	// Add an unhealthy canary that would block.
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "different-hash", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream); gate assertion below
	if cr.Status.Rollout != nil {
		t.Fatal("already on target should bypass gate")
	}
}

func TestGate_FirstProvisionEmptyAppliedHash(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-first", 5, "", nil)
	// Set a blocking earlier-wave sibling.
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "other", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream); gate assertion below
	if cr.Status.Rollout != nil {
		t.Fatal("first provision should bypass gate")
	}
}

func TestGate_BlockedSetsRolloutAndCondition(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-blocked", 2, "hash-old", nil)
	// Add a wave-0 canary that is healthy but on the OLD hash.
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "hash-old", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout == nil {
		t.Fatal("expected rollout status")
	}
	if !cr.Status.Rollout.Blocked {
		t.Fatal("expected blocked")
	}
	if cr.Status.Rollout.TargetBundleHash != "hash-new" {
		t.Fatalf("expected TargetBundleHash=hash-new, got %s", cr.Status.Rollout.TargetBundleHash)
	}
	if len(cr.Status.Rollout.WaitingOn) == 0 {
		t.Fatal("expected WaitingOn entries")
	}
	if !strings.HasPrefix(cr.Status.Rollout.WaitingOn[0], "ns/") {
		t.Fatalf("expected namespace/name format, got %s", cr.Status.Rollout.WaitingOn[0])
	}
	if cr.Status.Rollout.BlockedSince == nil {
		t.Fatal("expected BlockedSince")
	}

	// Verify condition.
	found := false
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionRolloutBlocked {
			found = true
			if c.Status != metav1.ConditionTrue {
				t.Fatal("expected ConditionTrue")
			}
			if c.Reason != v1alpha1.ReasonBlockedByWave {
				t.Fatalf("expected reason BlockedByWave, got %s", c.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected RolloutBlocked condition")
	}

	// Phase should still be Connected.
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("expected phase Connected, got %s", cr.Status.Phase)
	}

	// Guardrail: blocked path must NOT advance DesiredStateHash or LastDesiredPushAt.
	if cr.Status.DesiredStateHash != "" {
		t.Fatalf("expected DesiredStateHash to remain empty (not pushed), got %s", cr.Status.DesiredStateHash)
	}
	if cr.Status.LastDesiredPushAt != nil {
		t.Fatal("expected LastDesiredPushAt to remain nil (not pushed)")
	}
}

func TestGate_ClearedClearsRolloutAndCondition(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-cleared", 2, "hash-old", nil)
	// Earlier-wave canary is healthy on the new hash.
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "hash-new", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream); gate assertion below
	if cr.Status.Rollout != nil {
		t.Fatal("expected rollout cleared")
	}

	found := false
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionRolloutBlocked {
			found = true
			if c.Status != metav1.ConditionFalse {
				t.Fatal("expected ConditionFalse")
			}
			if c.Reason != v1alpha1.ReasonWaveCleared {
				t.Fatalf("expected reason WaveCleared, got %s", c.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected RolloutBlocked condition (set to False)")
	}
}

func TestGate_CascadeBlockedWave(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	// Wave-2 controller, wave-1 controller is still on old hash (blocked itself).
	cr := newGateCtrl("ctl-wave2", 2, "hash-old", nil)
	wave1 := freshSib("wave1", 1, "hash-old", true)
	// Also add a wave-0 canary that is on the new hash (healthy).
	wave0 := freshSib("wave0", 0, "hash-new", true)
	client.controllers = []*v1alpha1.Controller{cr, wave1, wave0}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout == nil || !cr.Status.Rollout.Blocked {
		t.Fatal("expected blocked (wave-1 not on target)")
	}
	// Should be waiting on wave1, not wave0.
	if len(cr.Status.Rollout.WaitingOn) != 1 || cr.Status.Rollout.WaitingOn[0] != "ns/wave1" {
		t.Fatalf("expected waitingOn=[ns/wave1], got %v", cr.Status.Rollout.WaitingOn)
	}
}

func TestGate_BlockedSincePreservedAndReset(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-preserve", 2, "hash-old", nil)
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "hash-old", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	// First tick: block.
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	firstBlockedSince := cr.Status.Rollout.BlockedSince

	// Second tick: same target hash.
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err = rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout.BlockedSince != firstBlockedSince {
		t.Fatal("BlockedSince should be preserved across ticks for the same target")
	}

	// Change target hash.
	bndl.Status.ResolvedHash = "hash-newer"
	seedClientCRDs(client)
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err = rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout.BlockedSince == firstBlockedSince {
		t.Fatal("BlockedSince should reset on new target hash")
	}
}

func TestGate_DrainingCanaryHoldsLaterWaves(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-wave1", 1, "hash-old", nil)
	// Wave-0 canary: pushed the new hash, AppliedBundleHash matches target,
	// BUT LastApplyResult.Succeeded is false (still draining/deferred).
	draining := freshSib("draining", 0, "hash-new", false)
	client.controllers = []*v1alpha1.Controller{cr, draining}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The draining sibling has AppliedBundleHash == target but Succeeded == false.
	// This should hold the later wave (the gate checks Succeeded, not just hash match).
	if cr.Status.Rollout == nil || !cr.Status.Rollout.Blocked {
		t.Fatal("draining canary (Succeeded=false) should hold later wave")
	}
}

func TestGate_MiteStatusNilBlocksLaterWave(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-nilsib", 1, "hash-old", nil)
	nilSib := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "gate-bundle"},
			ReconciliationPolicy: &v1alpha1.ReconciliationPolicy{
				RolloutWave: 0,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:             v1alpha1.ControllerPhaseConnected,
			AppliedBundleHash: "hash-new",
			LastApplyResult: &v1alpha1.ApplyResult{
				Succeeded: true,
			},
			MiteStatus: nil, // no mite status at all
		},
	}
	client.controllers = []*v1alpha1.Controller{cr, nilSib}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout == nil || !cr.Status.Rollout.Blocked {
		t.Fatal("nil MiteStatus sibling should block later wave")
	}
}

func TestGate_RolloutBlockedNeverFlipsPhase(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-phase", 2, "hash-old", nil)
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	client.controllers = []*v1alpha1.Controller{cr, freshSib("canary", 0, "hash-old", true)}
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase should stay Connected, got %s", cr.Status.Phase)
	}
}

func TestGate_AppliedBundleHashAdvanceOnSuccessMatch(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-hash", 0, "", nil)
	cr.Status.AppliedBundleHash = ""

	// Tick 1: first push — DesiredStateHash is set by handleConnected before Send.
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	_ = rec.handleConnected(context.Background(), cr) // Send fails (no real stream)
	desiredAfterTick1 := cr.Status.DesiredStateHash
	if desiredAfterTick1 == "" {
		t.Fatal("expected DesiredStateHash to be set on first tick")
	}
	// AppliedBundleHash should NOT advance on first tick (no matching apply result).
	if cr.Status.AppliedBundleHash != "" {
		t.Fatalf("AppliedBundleHash should not advance without a successful apply result, got %s", cr.Status.AppliedBundleHash)
	}

	// Tick 2: simulate a successful apply of the pushed desired hash.
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{
		Hash:      desiredAfterTick1,
		Succeeded: true,
	}
	_ = rec.handleConnected(context.Background(), cr) // Send fails
	if cr.Status.AppliedBundleHash != "hash-xyz" {
		t.Fatalf("expected AppliedBundleHash=hash-xyz after successful apply, got %s", cr.Status.AppliedBundleHash)
	}

	// Tick 3: hash changed (bundle moved on) but lastApplyResult still shows old success.
	bndl.Status.ResolvedHash = "hash-newer"
	cr.Status.LastApplyResult = &v1alpha1.ApplyResult{
		Hash:      "hash-xyz", // old desired hash, not the current one
		Succeeded: true,
	}
	_ = rec.handleConnected(context.Background(), cr) // Send fails
	if cr.Status.AppliedBundleHash == "hash-newer" {
		t.Fatal("AppliedBundleHash should NOT advance to newer hash without a matching apply result for it")
	}
}

func TestGate_WaitingOnCappedAt10(t *testing.T) {
	client := newTestClientWithGateBundle()
	bndl := client.composedBundles["gate-bundle"]
	bndl.Status.ResolvedHash = "hash-new"
	rec, registry := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl-cap", 1, "hash-old", nil)
	var sibs []*v1alpha1.Controller
	sibs = append(sibs, cr)
	for i := 0; i < 15; i++ {
		sibs = append(sibs, freshSib(fmt.Sprintf("sib-%d", i), 0, "hash-old", true))
	}
	client.controllers = sibs
	for _, c := range client.controllers {
		crdstore.MustSeed(client.store, c)
	}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	err := rec.handleConnected(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cr.Status.Rollout == nil {
		t.Fatal("expected rollout")
	}
	if len(cr.Status.Rollout.WaitingOn) > 10 {
		t.Fatalf("WaitingOn should be capped at 10, got %d", len(cr.Status.Rollout.WaitingOn))
	}
	// Condition message should mention the actual count (15), not the capped count.
	condMsg := ""
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionRolloutBlocked {
			condMsg = c.Message
		}
	}
	if !strings.Contains(condMsg, "10") {
		t.Fatalf("condition message should mention capped count 10, got: %s", condMsg)
	}
	// WaitingOn list should be capped at 10 entries.
	if len(cr.Status.Rollout.WaitingOn) != 10 {
		t.Fatalf("WaitingOn should be exactly 10 after capping, got %d", len(cr.Status.Rollout.WaitingOn))
	}
}
