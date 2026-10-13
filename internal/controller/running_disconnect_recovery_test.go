package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/mite"
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

// streamLostCond returns a MiteConnected=False/StreamLost condition that
// transitioned at the given time.
func streamLostCond(at time.Time) v1alpha1.ControllerCondition {
	return v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionMiteConnected,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonStreamLost,
		LastTransitionTime: metav1.NewTime(at),
	}
}

// TestHandleRunning_DisconnectHoldsRunning asserts that a disconnected mite
// never moves a Running controller back to Pending. A forced Pending resets
// ProvisioningStartedAt and reprovisions an unchanged pod every few seconds.
func TestHandleRunning_DisconnectHoldsRunning(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("orphan")
	started := metav1.NewTime(time.Now())
	cr.Status.ProvisioningStartedAt = &started

	for i := 1; i <= 12; i++ {
		if err := rec.handleRunning(context.Background(), cr); err != nil {
			t.Fatalf("tick %d: handleRunning error: %v", i, err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
			t.Fatalf("tick %d: phase = %s, want Running", i, cr.Status.Phase)
		}
	}
	if !cr.Status.ProvisioningStartedAt.Equal(&started) {
		t.Errorf("ProvisioningStartedAt moved from %v to %v", started, cr.Status.ProvisioningStartedAt)
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected); c != nil {
		t.Errorf("a never-connected mite must carry no MiteConnected condition, got %+v", *c)
	}
}

// TestHandleRunning_StreamLostSkipsProvisioningTimeout asserts that a
// controller that lost a live stream stays Running past provisioningTimeout.
// Its ProvisioningStartedAt dates from the last provisioning run, long before
// the loss, so the guard would fail it on the first Running tick.
func TestHandleRunning_StreamLostSkipsProvisioningTimeout(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("lost")
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	cr.Status.ProvisioningStartedAt = &started
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now().Add(-10 * time.Minute))}

	for i := 1; i <= 12; i++ {
		if err := rec.handleRunning(context.Background(), cr); err != nil {
			t.Fatalf("tick %d: handleRunning error: %v", i, err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseRunning {
			t.Fatalf("tick %d: phase = %s, want Running", i, cr.Status.Phase)
		}
	}
	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonStreamLost {
		t.Fatalf("MiteConnected = %+v, want False/StreamLost kept", c)
	}
}

// TestHandleRunning_TimeoutAppliesAfterReprovision asserts the timeout guard
// still fails a controller whose provisioning run started after the stream
// loss. A drift roll that brings up a pod whose mite never connects must not
// wait forever.
func TestHandleRunning_TimeoutAppliesAfterReprovision(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("rolled")
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	cr.Status.ProvisioningStartedAt = &started
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now().Add(-2 * time.Hour))}

	_ = rec.handleRunning(context.Background(), cr)
	if cr.Status.Phase != v1alpha1.ControllerPhaseFailed {
		t.Fatalf("phase = %s, want Failed", cr.Status.Phase)
	}
}

// TestHandleRunning_ReconnectAfterStreamLostSetsRestored asserts that a
// reconnect returns the controller to Connected and records StreamRestored.
func TestHandleRunning_ReconnectAfterStreamLostSetsRestored(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := runningCtrl("flap")
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now().Add(-time.Minute))}

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("connected tick: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase = %s, want Connected", cr.Status.Phase)
	}
	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonStreamRestored {
		t.Fatalf("MiteConnected = %+v, want True/StreamRestored", c)
	}
}

// TestHandleRunning_FirstConnectWritesNoMiteConnected asserts that a first
// connection writes no MiteConnected condition. The condition exists only
// after a loss.
func TestHandleRunning_FirstConnectWritesNoMiteConnected(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)

	cr := runningCtrl("first")
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("connected tick: %v", err)
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteConnected); c != nil {
		t.Fatalf("MiteConnected = %+v, want absent", *c)
	}
}

// TestHandleRunning_RemintsExpiredBootstrapToken pins the auditboi wedge: the
// 15-minute bootstrap token routinely expires while the pod is still pulling
// plugins in init, and by the time the mite first registers the phase has
// advanced to Running — where no remint ran. The mite then burns
// Unauthenticated retries until provisioningTimeout fails the controller.
// A Running tick with a disconnected mite must refresh an expired token
// immediately.
func TestHandleRunning_RemintsExpiredBootstrapToken(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("stale-token")
	pre := controllerPrefix(cr)
	signer := mite.NewTokenSigner(testKey)
	expired, err := signer.GenerateToken(cr.Name, cr.Namespace, -time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if client.existingSecrets == nil {
		client.existingSecrets = map[string]map[string][]byte{}
	}
	client.existingSecrets[pre+"-bootstrap"] = map[string][]byte{"token": []byte(expired)}

	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}

	minted, ok := client.createdSecrets[pre+"-bootstrap"]
	if !ok {
		t.Fatal("expected the Running tick to remint the expired bootstrap token")
	}
	if _, err := signer.ValidateToken(string(minted["token"]), cr.Name, cr.Namespace); err != nil {
		t.Fatalf("reminted token does not validate: %v", err)
	}
}

// TestHandleRunning_KeepsValidBootstrapToken: a still-valid token must NOT be
// rotated by the Running tick — rotation invalidates a mite that is about to
// present it (Secret volume propagation lags up to ~60s).
func TestHandleRunning_KeepsValidBootstrapToken(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := runningCtrl("fresh-token")
	pre := controllerPrefix(cr)
	signer := mite.NewTokenSigner(testKey)
	fresh, err := signer.GenerateToken(cr.Name, cr.Namespace, 15*time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if client.existingSecrets == nil {
		client.existingSecrets = map[string]map[string][]byte{}
	}
	client.existingSecrets[pre+"-bootstrap"] = map[string][]byte{"token": []byte(fresh)}

	if err := rec.handleRunning(context.Background(), cr); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}

	if _, rotated := client.createdSecrets[pre+"-bootstrap"]; rotated {
		t.Fatal("expected no rotation of a still-valid bootstrap token")
	}
}

// reconnectingCtrl builds a Running controller whose mite stream was lost a
// minute ago and that connected before, so the next connect is a reconnect.
func reconnectingCtrl(t *testing.T, name string) *v1alpha1.Controller {
	t.Helper()
	cr := newGateCtrl(name, 0, "", nil)
	cr.Status.Phase = v1alpha1.ControllerPhaseRunning
	first := metav1.NewTime(time.Now().Add(-time.Hour))
	cr.Status.FirstConnectedAt = &first
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now().Add(-time.Minute))}
	return cr
}

// TestReconcileRunning_ReconnectPushesInSamePass asserts that the reconcile
// which sees a reconnected mite both moves the controller to Connected and
// sends the desired state, so the push does not wait for the next tick.
func TestReconcileRunning_ReconnectPushesInSamePass(t *testing.T) {
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	tr := &scriptedSendTransport{Transport: rec.miteTransport, epoch: 1}
	rec.miteTransport = tr

	cr := reconnectingCtrl(t, "reconnect-push")
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase = %s, want Connected", cr.Status.Phase)
	}
	if tr.sends != 1 {
		t.Fatalf("sends = %d after the connect pass, want 1", tr.sends)
	}
	if cr.Status.DesiredStateHash == "" || cr.Status.LastDesiredPushAt == nil {
		t.Fatal("the connect pass sent no desired state (hash or push time unset)")
	}

	// The same connection epoch on the next pass sends nothing more.
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("second reconcileController: %v", err)
	}
	if tr.sends != 1 {
		t.Fatalf("sends = %d after a same-epoch pass, want 1", tr.sends)
	}

	// A new connection epoch forces a fresh push.
	tr.epoch = 2
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("third reconcileController: %v", err)
	}
	if tr.sends != 2 {
		t.Fatalf("sends = %d after a new epoch, want 2", tr.sends)
	}
}

// TestReconcileRunning_ForceReprovisionWaitsForConnectedPass asserts that an
// armed force-reprovision annotation keeps the connect pass from chaining the
// push, because only a pass that starts in Connected consumes it.
func TestReconcileRunning_ForceReprovisionWaitsForConnectedPass(t *testing.T) {
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	tr := &scriptedSendTransport{Transport: rec.miteTransport, epoch: 1}
	rec.miteTransport = tr

	cr := reconnectingCtrl(t, "reconnect-force")
	cr.Annotations = map[string]string{annotationForceReprovision: time.Now().Format(time.RFC3339Nano)}
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("phase = %s, want Connected", cr.Status.Phase)
	}
	if tr.sends != 0 {
		t.Fatalf("sends = %d with force-reprovision armed, want 0 on the connect pass", tr.sends)
	}
}

// TestReconcileRunning_ChainedPushFailureKeepsConnected asserts that a failed
// chained push returns its error and still persists the Connected phase, so
// the Running timeout guard cannot fail a connected controller.
func TestReconcileRunning_ChainedPushFailureKeepsConnected(t *testing.T) {
	client := newTestClientWithGateBundle()
	client.composedBundles["gate-bundle"].Status.ResolvedHash = "hash-xyz"
	rec, registry := newTestReconcilerForGate(client)
	tr := &scriptedSendTransport{Transport: rec.miteTransport, sendErr: errors.New("marshal failed"), epoch: 1}
	rec.miteTransport = tr

	cr := reconnectingCtrl(t, "reconnect-fail")
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	if err := rec.reconcileController(context.Background(), cr); err == nil {
		t.Fatal("reconcileController returned nil; want the chained push error")
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		t.Fatalf("in-memory phase = %s after a chained failure, want Connected", cr.Status.Phase)
	}
	if got := client.statuses[cr.Name]; got != string(v1alpha1.ControllerPhaseConnected) {
		t.Fatalf("persisted phase = %q after a chained failure, want Connected", got)
	}
}
