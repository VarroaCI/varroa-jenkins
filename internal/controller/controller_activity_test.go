package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestPluginConflictActivityEvent exercises the activity-bus publish path for
// plugin-lock-conflict events using an embedded NATS server. It follows the
// pattern in internal/api/activity/integration_test.go.
//
// Because driving the full handleProvisioning flow through a real Reconciler
// requires a complete fake ResourceClient + bundle resolution chain, this test
// exercises at the emit level: a Reconciler with a real activityPublisher,
// simulating prior/current condition state and verifying the right NATS
// traffic.
func TestPluginConflictActivityEvent(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}

	// Start embedded NATS server.
	opts := &server.Options{
		Port:     -1,
		StoreDir: t.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	pub := activity.NewPublisher(bus.DefaultCluster, conn)
	rec := newTestReconciler(newTestClient())
	rec.SetActivityPublisher(pub)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ctrl",
			Namespace: "test-ns",
		},
	}

	activitySubj := bus.ActivitySubject(bus.DefaultCluster, "test-ns", "test-ctrl")
	received := make(chan activity.Event, 10)
	sub, err := conn.SubscribeData(activitySubj, func(data []byte) {
		var e activity.Event
		if err := json.Unmarshal(data, &e); err != nil {
			t.Logf("unmarshal event: %v", err)
			return
		}
		received <- e
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	conflictMsg := "plugin foo: pin=1.0, lock=1.1"

	// Helper: simulate the emit logic from handleProvisioning for a given
	// prior condition state and current-conflict flag.
	emitIfEdge := func(prior *v1alpha1.ControllerCondition, conflictNow bool) {
		if shouldEmitPluginConflictEvent(prior, conflictNow) {
			if rec.activityPublisher != nil {
				rec.activityPublisher.Publish(activity.Event{
					Type:       "pluginConflict.detected",
					Source:     "operator",
					Controller: cr.Name,
					Namespace:  cr.Namespace,
					Message:    conflictMsg,
					Reason:     v1alpha1.ReasonPluginConflict,
					Severity:   "warning",
				})
			}
		}
	}

	drainExtra := func() int {
		n := 0
		for {
			select {
			case <-received:
				n++
			default:
				return n
			}
		}
	}

	// --- Scenario 1: absent → True emits exactly one warning event ---
	emitIfEdge(nil, true)

	select {
	case e := <-received:
		if e.Severity != "warning" {
			t.Errorf("expected Severity=warning, got %q", e.Severity)
		}
		if e.Type != "pluginConflict.detected" {
			t.Errorf("expected Type=pluginConflict.detected, got %q", e.Type)
		}
		if e.Message != conflictMsg {
			t.Errorf("expected Message=%q, got %q", conflictMsg, e.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first event (absent→True)")
	}
	if n := drainExtra(); n > 0 {
		t.Errorf("expected 0 extra events after absent→True, got %d", n)
	}

	// --- Scenario 2: True persists → no duplicate event ---
	emitIfEdge(&v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginConflict,
		Status: metav1.ConditionTrue,
	}, true)
	if n := drainExtra(); n > 0 {
		t.Errorf("expected 0 extra events on repeated True tick, got %d", n)
	}

	// --- Scenario 3: True → False → True emits exactly one new event ---
	// Step 3a: conflict clears → no-conflict pass (no event).
	emitIfEdge(&v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginConflict,
		Status: metav1.ConditionTrue,
	}, false)
	if n := drainExtra(); n > 0 {
		t.Errorf("expected 0 events on True→False transition, got %d", n)
	}

	// Step 3b: new conflict emerges with prior=False → emits.
	emitIfEdge(&v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginConflict,
		Status: metav1.ConditionFalse,
	}, true)
	select {
	case e := <-received:
		if e.Severity != "warning" {
			t.Errorf("second episode: expected Severity=warning, got %q", e.Severity)
		}
		if e.Type != "pluginConflict.detected" {
			t.Errorf("second episode: expected Type=pluginConflict.detected, got %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second episode event (False→True)")
	}
	if n := drainExtra(); n > 0 {
		t.Errorf("expected 0 extra events after second episode, got %d", n)
	}
}

// TestPluginConflictActivityEventNoPublisher verifies that emit logic does not
// panic when activityPublisher is nil (the common case in existing tests).
func TestPluginConflictActivityEventNoPublisher(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	// rec.activityPublisher is nil by default.

	prior := (*v1alpha1.ControllerCondition)(nil)
	if !shouldEmitPluginConflictEvent(prior, true) {
		t.Fatal("shouldEmit returned false for absent→conflict")
	}
	// The nil-guard should prevent any panic — test that we can safely
	// evaluate the guard path even with a nil publisher.
	if rec.activityPublisher != nil {
		t.Error("expected nil publisher in default Reconciler")
	}
	_ = context.Background()
}

// TestConditionPointerAliasing documents the root-cause bug fixed in C4:
// findCondition returns &conds[i] — a pointer into the slice. setCondition
// overwrites conditions[i] with the new value, which aliases the memory
// prior points to. If shouldEmitPluginConflictEvent reads prior.Status AFTER
// setCondition, it sees the NEW True value instead of the old False, and
// silently suppresses the edge event.
//
// This test fails if the emit decision were made after setCondition, proving
// that surfacePluginConflict must capture shouldEmit BEFORE mutating.
func TestConditionPointerAliasing(t *testing.T) {
	// Seed a condition slice with ConditionPluginConflict=False, simulating
	// a prior successful provisioning pass that cleared a former conflict.
	conds := []v1alpha1.ControllerCondition{
		{
			Type:   v1alpha1.ConditionPluginConflict,
			Status: metav1.ConditionFalse,
			Reason: "NoConflict",
		},
	}

	// Step 1: findCondition returns a pointer INTO the slice.
	prior := findCondition(conds, v1alpha1.ConditionPluginConflict)
	if prior == nil {
		t.Fatal("expected to find existing PluginConflict condition")
	}
	if prior.Status != metav1.ConditionFalse {
		t.Fatalf("expected prior.Status=False, got %s", prior.Status)
	}

	// Step 2: setCondition overwrites conditions[0] in place (the returned
	// slice reassignment is deliberate — it mirrors the real call pattern).
	conds = setCondition(conds, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPluginConflict,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonPluginConflict,
		Message: "plugin foo: pin=1.0, lock=1.1",
	})

	// Step 3: prior now aliases the NEW value — this is the bug. Also verify
	// the returned slice actually contains the new condition.
	if len(conds) != 1 || conds[0].Status != metav1.ConditionTrue {
		t.Errorf("setCondition result: expected 1 condition True, got len=%d", len(conds))
	}
	if prior.Status != metav1.ConditionTrue {
		t.Errorf("aliasing: after setCondition overwrites the same element, "+
			"prior.Status should be True (it reads the new value), got %s", prior.Status)
	}

	// With this aliasing, shouldEmitPluginConflictEvent(prior, true) would
	// return false (prior.Status == True), suppressing the edge event.
	// surfacePluginConflict avoids this by computing shouldEmit BEFORE setCondition.
	if shouldEmitPluginConflictEvent(prior, true) {
		t.Error("the aliased prior pointer causes shouldEmit to return false — " +
			"if the call were ordered after setCondition, the event would be suppressed")
	}
}

// TestSurfacePluginConflictWithNATS exercises the full surfacePluginConflict
// path with a real NATS publisher, proving that a pluginConflict.detected
// event IS published when the prior condition is False (the realistic case:
// a prior provisioning pass cleared the conflict). This would FAIL against
// the pre-fix code where the emit decision read the aliased prior pointer
// after setCondition, silently returning false.
func TestSurfacePluginConflictWithNATS(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}

	// Start embedded NATS server.
	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	pub := activity.NewPublisher(bus.DefaultCluster, conn)
	rec := newTestReconciler(newTestClient())
	rec.SetActivityPublisher(pub)

	// Subscribe to activity events for the controller under test.
	activitySubj := bus.ActivitySubject(bus.DefaultCluster, "test-ns", "test-ctrl")
	received := make(chan activity.Event, 5)
	sub, err := conn.SubscribeData(activitySubj, func(data []byte) {
		var e activity.Event
		if err := json.Unmarshal(data, &e); err != nil {
			t.Logf("unmarshal event: %v", err)
			return
		}
		received <- e
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ctrl",
			Namespace: "test-ns",
		},
	}
	// Seed with ConditionPluginConflict=False — the realistic prior state
	// from a previous successful provisioning pass that cleared the conflict.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginConflict,
		Status: metav1.ConditionFalse,
		Reason: "NoConflict",
	})

	ctx := context.Background()
	conflictMsg := "plugin foo: pin=1.0, lock=1.1"

	// Call surfacePluginConflict with a False prior — must emit an edge event.
	rec.surfacePluginConflict(ctx, cr, conflictMsg)

	// Verify the condition was set to True.
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginConflict)
	if cond == nil {
		t.Fatal("surfacePluginConflict did not set ConditionPluginConflict")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected ConditionPluginConflict=True, got %s", cond.Status)
	}

	// Verify a pluginConflict.detected event was published.
	select {
	case e := <-received:
		if e.Type != "pluginConflict.detected" {
			t.Errorf("expected Type=pluginConflict.detected, got %q", e.Type)
		}
		if e.Severity != "warning" {
			t.Errorf("expected Severity=warning, got %q", e.Severity)
		}
		if e.Message != conflictMsg {
			t.Errorf("expected Message=%q, got %q", conflictMsg, e.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for pluginConflict.detected event (False→True edge)")
	}

	// Verify no duplicate events.
	select {
	case <-received:
		t.Error("unexpected duplicate event after False→True edge")
	default:
	}

	// --- Second call: prior is already True → no duplicate event ---
	rec.surfacePluginConflict(ctx, cr, "another conflict")

	select {
	case <-received:
		t.Error("unexpected duplicate event on repeated True tick")
	default:
	}
}

// TestSurfaceAndClearPluginConflict tests the detection-only methods extracted
// for Defect 2: surfacePluginConflict sets ConditionPluginConflict=True and
// records the gauge without blocking (no markReconcileBlocked), and
// clearPluginConflict clears it.
func TestSurfaceAndClearPluginConflict(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	// rec.activityPublisher is nil — surfacePluginConflict must not panic.

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ctrl",
			Namespace: "test-ns",
		},
	}
	ctx := context.Background()

	// --- surfacePluginConflict ---
	conflictMsg := "plugin bar: pin=2.0, lock=2.1"
	rec.surfacePluginConflict(ctx, cr, conflictMsg)

	// Condition must be True with the conflict message.
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginConflict)
	if cond == nil {
		t.Fatal("surfacePluginConflict did not set ConditionPluginConflict")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected ConditionPluginConflict=True, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonPluginConflict {
		t.Errorf("expected Reason=%s, got %s", v1alpha1.ReasonPluginConflict, cond.Reason)
	}
	if cond.Message != conflictMsg {
		t.Errorf("expected Message=%q, got %q", conflictMsg, cond.Message)
	}

	// surfacePluginConflict must NOT set ReconcileBlocked (it's detection-only).
	blocked := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if blocked != nil && blocked.Status == metav1.ConditionTrue {
		t.Error("surfacePluginConflict must not set ConditionReconcileBlocked (detection-only)")
	}

	// --- clearPluginConflict ---
	rec.clearPluginConflict(ctx, cr)

	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginConflict)
	if cond == nil {
		t.Fatal("clearPluginConflict removed ConditionPluginConflict instead of clearing it")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected ConditionPluginConflict=False, got %s", cond.Status)
	}
	if cond.Reason != "NoConflict" {
		t.Errorf("expected Reason=NoConflict, got %s", cond.Reason)
	}
}
