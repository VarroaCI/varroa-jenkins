package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// runningCtrl builds a Running-phase controller wired to the gate bundle so the
// handleRunning machinery (checkPluginRollFailed → resolveBundleForController)
// runs cleanly under the test client.
func runningCtrl(name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "gate-bundle"},
		},
		Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseRunning},
	}
}

// TestHandleRunning_ProlongedDisconnectResetsToPending is the #302 Fix C
// regression: a controller observed in Running whose mite never (re)connects
// must, after the grace period, reset to Pending so the Pending→Provisioning
// pass runs a full reprovision (rolling the mite container's CA env) instead of
// waiting out the 5-minute provisioning timeout.
func TestHandleRunning_ProlongedDisconnectResetsToPending(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("orphan")

	// No mite registered → Info reports disconnected. Ticks 1 and 2 are the
	// grace period; the controller stays in Running.
	for i := 1; i <= 2; i++ {
		if err := rec.handleRunning(context.Background(), cr); err != nil {
			t.Fatalf("tick %d: handleRunning error: %v", i, err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
			t.Fatalf("tick %d: expected phase Running during grace, got %s", i, cr.Status.Phase)
		}
	}

	// Tick 3 crosses the grace threshold → reset to Pending.
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("tick 3: handleRunning error: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhasePending {
		t.Fatalf("tick 3: expected phase Pending after prolonged disconnect, got %s", cr.Status.Phase)
	}

	// Ready=False / MiteDisconnected condition surfaced.
	var ready *v1alpha1.ControllerCondition
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == v1alpha1.ConditionReady {
			ready = &cr.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatal("expected a Ready condition after prolonged disconnect")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != "MiteDisconnected" {
		t.Errorf("expected Ready=False/MiteDisconnected, got status=%s reason=%s", ready.Status, ready.Reason)
	}
}

// TestHandleRunning_ReconnectClearsDisconnectTicks verifies the grace counter is
// cleared when the mite (re)connects, so a later disconnect gets a fresh grace
// window, and the controller advances to Connected.
func TestHandleRunning_ReconnectClearsDisconnectTicks(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := runningCtrl("flap")

	// One disconnected tick (within grace).
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("disconnected tick: %v", err)
	}
	if got := rec.disconnectedTicks["ns/flap"]; got != 1 {
		t.Fatalf("expected 1 disconnected tick, got %d", got)
	}

	// Mite connects → tick counter cleared and phase advances to Connected.
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("connected tick: %v", err)
	}
	if got := rec.disconnectedTicks["ns/flap"]; got != 0 {
		t.Errorf("expected disconnected ticks cleared on reconnect, got %d", got)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Errorf("expected phase Connected after reconnect, got %s", cr.Status.Phase)
	}
}

// TestHandleRunning_ForcePendingClearsGraceWindow is the PR #308 Copilot
// regression: when handleRunning forces Pending after the grace period it must
// clear the disconnected-tick counter, so a Provisioning→Running bounce-back
// (common while the pod restarts before the mite reconnects) gets a fresh grace
// window instead of immediately re-forcing Pending — otherwise the controller
// thrashes Pending↔Provisioning↔Running every tick.
func TestHandleRunning_ForcePendingClearsGraceWindow(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("thrash")

	// Three disconnected ticks → Pending.
	for i := 1; i <= 3; i++ {
		if err := rec.handleRunning(context.Background(), cr); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if cr.Status.Phase != v1alpha1.ControllerPhasePending {
		t.Fatalf("expected Pending after 3 ticks, got %s", cr.Status.Phase)
	}
	if got := rec.disconnectedTicks["ns/thrash"]; got != 0 {
		t.Fatalf("expected disconnected ticks cleared when forcing Pending, got %d", got)
	}

	// Simulate the Provisioning→Running bounce-back before the mite reconnects:
	// the next tick must re-enter the grace window (stay Running), not re-Pend.
	cr.Status.Phase = v1alpha1.ControllerPhaseRunning
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("bounce-back tick: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
		t.Fatalf("expected a fresh grace window (Running) after bounce-back, got %s", cr.Status.Phase)
	}
	if got := rec.disconnectedTicks["ns/thrash"]; got != 1 {
		t.Errorf("expected 1 tick in the fresh grace window, got %d", got)
	}
}
