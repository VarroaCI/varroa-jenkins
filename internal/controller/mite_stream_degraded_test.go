package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// degradedTransport decorates a Transport so a test can force the gateway's
// bus→stream bridge to report as broken while the mite still looks connected.
type degradedTransport struct {
	transport.Transport
	reason   string
	degraded bool
}

func (d *degradedTransport) StreamDegraded(_, _ string) (string, bool) {
	return d.reason, d.degraded
}

// TestConnected_MiteStreamDegradedConditionSet asserts that when the gateway
// cannot bridge desired state to a connected mite, the Controller must stop
// reporting unqualified health — otherwise the CR sits at Connected/Ready
// with no signal at all, and the only evidence is one controller-scoped
// gateway log line.
func TestConnected_MiteStreamDegradedConditionSet(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)
	rec.miteTransport = &degradedTransport{
		Transport: rec.miteTransport,
		reason:    "desired watch setup failing: context deadline exceeded",
		degraded:  true,
	}

	cr := newGateCtrl("ctl", 0, "", nil)
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	_ = rec.handleConnected(context.Background(), cr)

	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteStreamDegraded)
	if cond == nil {
		t.Fatal("MiteStreamDegraded condition was not set while the bus bridge was broken")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("MiteStreamDegraded status = %s, want True", cond.Status)
	}
	if cond.Message == "" {
		t.Fatal("MiteStreamDegraded carries no message; the reason is what makes this debuggable")
	}
}

// TestConnected_MiteStreamDegradedConditionCleared asserts the condition is
// self-healing once the gateway re-establishes its watch.
func TestConnected_MiteStreamDegradedConditionCleared(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)
	rec.miteTransport = &degradedTransport{Transport: rec.miteTransport, degraded: false}

	cr := newGateCtrl("ctl", 0, "", nil)
	cr.Status.Conditions = append(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionMiteStreamDegraded,
		Status: metav1.ConditionTrue,
		Reason: "BusWatchFailed",
	})
	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	_ = rec.handleConnected(context.Background(), cr)

	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteStreamDegraded)
	if cond == nil {
		t.Fatal("MiteStreamDegraded condition disappeared instead of being set False")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("MiteStreamDegraded status = %s, want False once the bridge recovered", cond.Status)
	}
}

// TestDisconnected_MiteStreamDegradedConditionRemoved pins that the condition
// is actually dropped when the mite goes away. It means "connected, but the
// bridge is starving it", so a True left behind survives the whole
// Pending/provisioning cycle claiming a problem that no longer applies.
// The first attempt used removeCondition, which matches on reason as well as
// type and therefore silently removed nothing.
func TestDisconnected_MiteStreamDegradedConditionRemoved(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, _ := newTestReconcilerForGate(client)

	cr := newGateCtrl("ctl", 0, "", nil)
	cr.Status.Conditions = append(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteStreamDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  "BusWatchFailed",
		Message: "desired watch setup failing",
	})

	// No registry entry: the mite is disconnected.
	_ = rec.handleConnected(context.Background(), cr)

	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteStreamDegraded); c != nil {
		t.Fatalf("MiteStreamDegraded survived disconnect: status=%s reason=%s", c.Status, c.Reason)
	}
}

// TestClearMiteConnection_RemovesStreamDegraded pins that powering a controller
// off drops the condition. Only handleConnected reconciles it, and a
// Stopped/Hibernated controller never reaches that path — so a stale True would
// otherwise claim a starved bridge for as long as the controller stays off.
func TestClearMiteConnection_RemovesStreamDegraded(t *testing.T) {
	cr := &v1alpha1.Controller{}
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type:   v1alpha1.ConditionMiteStreamDegraded,
		Status: metav1.ConditionTrue,
		Reason: "BusWatchFailed",
	}}

	clearMiteConnection(cr)

	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteStreamDegraded); c != nil {
		t.Fatalf("MiteStreamDegraded survived power-off: status=%s reason=%s", c.Status, c.Reason)
	}
}
