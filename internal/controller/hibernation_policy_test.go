package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ---------------------------------------------------------------------------
// hibernationIgnoreRegex tests
// ---------------------------------------------------------------------------

func TestHibernationIgnoreRegex_NilSpec(t *testing.T) {
	if got := hibernationIgnoreRegex(nil); got != "" {
		t.Errorf("expected empty string for nil spec, got %q", got)
	}
}

func TestHibernationIgnoreRegex_EmptySpec(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{}
	if got := hibernationIgnoreRegex(spec); got != "" {
		t.Errorf("expected empty string for empty spec, got %q", got)
	}
}

func TestHibernationIgnoreRegex_Set(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{ActivityIgnoreRegex: "^/health"}
	if got := hibernationIgnoreRegex(spec); got != "^/health" {
		t.Errorf("expected ^/health, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// mintWakeToken tests
// ---------------------------------------------------------------------------

func TestMintWakeToken_EnabledAndEmpty(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{Enabled: true}
	token, err := mintWakeToken(spec, false, "")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if len(token) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("expected 32-char hex token, got %q (len=%d)", token, len(token))
	}
}

func TestMintWakeToken_HibernatedFlagAndEmpty(t *testing.T) {
	token, err := mintWakeToken(nil, true, "")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if len(token) != 32 {
		t.Errorf("expected 32-char hex token, got %q (len=%d)", token, len(token))
	}
}

func TestMintWakeToken_Idempotent(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{Enabled: true}
	token, err := mintWakeToken(spec, false, "existing-token-123")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty (no mint needed), got %q", token)
	}
}

func TestMintWakeToken_NotNeeded(t *testing.T) {
	// Not enabled, not hibernated, no token.
	token, err := mintWakeToken(nil, false, "")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty (no mint needed), got %q", token)
	}
}

func TestMintWakeToken_RotationOnClear(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{Enabled: true}
	// First call: token created.
	token1, err := mintWakeToken(spec, false, "")
	if err != nil {
		t.Fatalf("first mintWakeToken failed: %v", err)
	}
	if len(token1) != 32 {
		t.Fatalf("expected 32-char token, got %q", token1)
	}
	// Token now exists: second call returns empty (no mint).
	token2, err := mintWakeToken(spec, false, token1)
	if err != nil {
		t.Fatalf("second mintWakeToken failed: %v", err)
	}
	if token2 != "" {
		t.Errorf("expected empty when token already exists, got %q", token2)
	}
	// Token cleared: third call creates a new token.
	token3, err := mintWakeToken(spec, false, "")
	if err != nil {
		t.Fatalf("third mintWakeToken failed: %v", err)
	}
	if len(token3) != 32 {
		t.Fatalf("expected 32-char token after clear, got %q", token3)
	}
	if token3 == token1 {
		t.Errorf("expected different token after clear, got same: %q", token3)
	}
}

// ---------------------------------------------------------------------------
// WakeHibernatedController tests (5.3)
// ---------------------------------------------------------------------------

func TestWakeHibernatedController_ClearsFlag(t *testing.T) {
	tc := &testClient{statuses: map[string]string{}, hibernatedChanged: true}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	r.WakeHibernatedController(context.Background(), "team-a", "foo")

	if len(tc.hibernatedWrites) != 1 {
		t.Fatalf("expected 1 SetHibernated call, got %d", len(tc.hibernatedWrites))
	}
	got := tc.hibernatedWrites[0]
	if got.name != "foo" || got.namespace != "team-a" || got.want != false {
		t.Errorf("SetHibernated = %+v, want {foo team-a false}", got)
	}
}

func TestWakeHibernatedController_NoOpWhenNotHibernated(t *testing.T) {
	// SetHibernated reports no change (controller was not hibernated). Wake
	// must not panic and must still have attempted exactly one CAS.
	tc := &testClient{statuses: map[string]string{}, hibernatedChanged: false}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	r.WakeHibernatedController(context.Background(), "team-a", "foo")

	if len(tc.hibernatedWrites) != 1 {
		t.Fatalf("expected 1 SetHibernated call, got %d", len(tc.hibernatedWrites))
	}
}

func TestWakeHibernatedController_ErrorIsSwallowed(t *testing.T) {
	tc := &testClient{statuses: map[string]string{}, hibernatedErr: errors.New("conflict")}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	// Must not panic; error is logged, not propagated (best-effort signal).
	r.WakeHibernatedController(context.Background(), "team-a", "foo")
}

// ---------------------------------------------------------------------------
// Authenticated hibernate / wake action tests (bus subjects group)
// ---------------------------------------------------------------------------

// seededActionController stores a Running controller in the fake store so the
// hibernate/wake action entry points can resolve it.
func seededActionController(client *testClient, powerState string) {
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{PowerState: powerState},
		Status:     v1alpha1.ControllerStatus{Hibernated: true},
	})
}

func TestWakeController_NudgeDoesNotClearHibernation(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	seededActionController(client, "Running")

	// The nudge only wakes the reconcile goroutine; it must never write
	// status.hibernated. A wake routed here would leave a controller asleep.
	rec.WakeController("core", "team-a", "foo")

	if len(client.hibernatedWrites) != 0 {
		t.Fatalf("nudge must not touch hibernation, got %+v", client.hibernatedWrites)
	}
}

func TestWakeControllerAction_ClearsHibernation(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	seededActionController(client, "Running")

	if err := rec.WakeControllerAction(context.Background(), "team-a", "foo"); err != nil {
		t.Fatalf("WakeControllerAction: %v", err)
	}

	if len(client.hibernatedWrites) != 1 {
		t.Fatalf("expected 1 SetHibernated call, got %d", len(client.hibernatedWrites))
	}
	got := client.hibernatedWrites[0]
	if got.name != "foo" || got.namespace != "team-a" || got.want != false {
		t.Errorf("SetHibernated = %+v, want {foo team-a false}", got)
	}
}

func TestHibernateController_SetsHibernation(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	seededActionController(client, "Running")

	if err := rec.HibernateController(context.Background(), "team-a", "foo"); err != nil {
		t.Fatalf("HibernateController: %v", err)
	}

	if len(client.hibernatedWrites) != 1 {
		t.Fatalf("expected 1 SetHibernated call, got %d", len(client.hibernatedWrites))
	}
	got := client.hibernatedWrites[0]
	if got.name != "foo" || got.namespace != "team-a" || got.want != true {
		t.Errorf("SetHibernated = %+v, want {foo team-a true}", got)
	}
}

func TestWakeControllerAction_RefusesStopped(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	seededActionController(client, "Stopped")

	err := rec.WakeControllerAction(context.Background(), "team-a", "foo")
	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("expected ErrControllerStopped, got %v", err)
	}
	if len(client.hibernatedWrites) != 0 {
		t.Fatalf("refused wake must not write, got %+v", client.hibernatedWrites)
	}
}

func TestHibernateController_RefusesStopped(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	seededActionController(client, "Stopped")

	err := rec.HibernateController(context.Background(), "team-a", "foo")
	if !errors.Is(err, ErrControllerStopped) {
		t.Fatalf("expected ErrControllerStopped, got %v", err)
	}
	if len(client.hibernatedWrites) != 0 {
		t.Fatalf("refused hibernate must not write, got %+v", client.hibernatedWrites)
	}
}

// ---------------------------------------------------------------------------
// Reconcile-level hibernation tests
// ---------------------------------------------------------------------------

// idleTransport reports an idle mite so handleConnected reaches the
// auto-hibernate branch; every other transport method comes from
// captureTransport (connected, healthy, Send succeeds without a gRPC stream).
type idleTransport struct {
	*captureTransport
}

func (t *idleTransport) IdleGauges(_, _ string) (*mitev1.IdleGauges, time.Time, bool) {
	return freshGauges(), time.Now(), true
}

func TestAutoHibernateWritesStatusNotSpec(t *testing.T) {
	client := newTestClient()
	client.hibernatedChanged = true
	rec := newTestReconciler(client)
	rec.miteTransport = &idleTransport{captureTransport: &captureTransport{}}

	now := time.Now()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(now)},
		Spec: v1alpha1.ControllerSpec{
			PowerState:  "Running",
			Hibernation: &v1alpha1.HibernationSpec{Enabled: true, GracePeriodMinutes: 60},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:            v1alpha1.ControllerPhaseConnected,
			FirstConnectedAt: &metav1.Time{Time: now.Add(-2 * time.Hour)},
			Conditions: []v1alpha1.ControllerCondition{{
				Type:               v1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "JenkinsHealthy",
				LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour)),
			}},
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The auto-hibernate write must go to status via SetHibernated(true)...
	if len(client.hibernatedWrites) != 1 {
		t.Fatalf("expected exactly 1 SetHibernated write, got %d", len(client.hibernatedWrites))
	}
	w := client.hibernatedWrites[0]
	if w.name != "test" || w.namespace != "ns" || !w.want {
		t.Errorf("SetHibernated = %+v, want {test ns true}", w)
	}
	// ...and must not touch spec: powerState is unchanged.
	if cr.Spec.PowerState != "Running" {
		t.Errorf("spec.powerState = %q, want Running (auto-hibernate must not write spec)", cr.Spec.PowerState)
	}
}

func TestStopThenStartCoalescedStartsAndDoesNotRehibernate(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{PowerState: "Running"},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseHibernated,
			// The coalesced Stop→Start power writes clear status.hibernated
			// before any reconcile observes Stopped; this reconcile sees the
			// final state: Running + not hibernated + old phase Hibernated.
			Hibernated: false,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhasePending {
		t.Fatalf("phase = %q, want Pending (waking from hibernation)", cr.Status.Phase)
	}
	// Must not re-park: no SetHibernated(true) and no scale-to-0.
	if len(client.hibernatedWrites) != 0 {
		t.Errorf("expected no SetHibernated writes, got %+v", client.hibernatedWrites)
	}
	if len(client.scaleCalls) != 0 {
		t.Errorf("expected no ScaleStatefulSet calls (must not re-hibernate), got %+v", client.scaleCalls)
	}
}

// ---------------------------------------------------------------------------
// shouldHibernate tests
// ---------------------------------------------------------------------------

// baseController returns a controller with hibernation enabled and a 60m grace period.
func baseController() *v1alpha1.Controller {
	now := metav1.Now()
	return &v1alpha1.Controller{
		Spec: v1alpha1.ControllerSpec{
			PowerState: "Running",
			Hibernation: &v1alpha1.HibernationSpec{
				Enabled:            true,
				GracePeriodMinutes: 60,
			},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:            v1alpha1.ControllerPhaseConnected,
			FirstConnectedAt: &metav1.Time{Time: now.Add(-2 * time.Hour)}, // connected 2h ago
			Conditions: []v1alpha1.ControllerCondition{
				{
					Type:               v1alpha1.ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "JenkinsHealthy",
					LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour)), // 2h ago
				},
			},
		},
	}
}

// freshGauges returns gauges with no activity in the last 90 minutes.
func freshGauges() *mitev1.IdleGauges {
	return &mitev1.IdleGauges{
		LastHttpActivityUnix: time.Now().Add(-90 * time.Minute).Unix(),
		LastEventUnix:        time.Now().Add(-90 * time.Minute).Unix(),
		RunningBuilds:        0,
		QueueLength:          0,
		TimerTriggerJobs:     0,
	}
}

func TestShouldHibernate_AllGatesPass(t *testing.T) {
	cr := baseController()
	gauges := freshGauges()
	receivedAt := time.Now()
	now := time.Now()

	ok, lastActivity := shouldHibernate(cr, gauges, receivedAt, now)
	if !ok {
		t.Error("expected all gates to pass")
	}
	if lastActivity.IsZero() {
		t.Error("expected non-zero lastActivity")
	}
}

// Gate 1: Hibernation not enabled.
func TestShouldHibernate_Gate1_NotEnabled(t *testing.T) {
	cr := baseController()
	cr.Spec.Hibernation.Enabled = false
	ok, _ := shouldHibernate(cr, freshGauges(), time.Now(), time.Now())
	if ok {
		t.Error("gate 1: expected false when hibernation is not enabled")
	}
}

// Gate 1b: Hibernation spec is nil.
func TestShouldHibernate_Gate1_NilSpec(t *testing.T) {
	cr := baseController()
	cr.Spec.Hibernation = nil
	ok, _ := shouldHibernate(cr, freshGauges(), time.Now(), time.Now())
	if ok {
		t.Error("gate 1: expected false when hibernation spec is nil")
	}
}

// Gate 2: Grace period has not elapsed.
func TestShouldHibernate_Gate2_GracePeriodNotElapsed(t *testing.T) {
	cr := baseController()
	cr.Spec.Hibernation.GracePeriodMinutes = 120 // 2h grace
	gauges := freshGauges()                      // activity 90min ago
	ok, _ := shouldHibernate(cr, gauges, time.Now(), time.Now())
	if ok {
		t.Error("gate 2: expected false when grace period (120m) > idle time (90m)")
	}
}

// Gate 2 variant: very recent activity.
func TestShouldHibernate_Gate2_RecentActivity(t *testing.T) {
	cr := baseController()
	gauges := &mitev1.IdleGauges{
		LastHttpActivityUnix: time.Now().Add(-5 * time.Minute).Unix(),
		LastEventUnix:        time.Now().Add(-90 * time.Minute).Unix(),
		RunningBuilds:        0,
		QueueLength:          0,
	}
	ok, _ := shouldHibernate(cr, gauges, time.Now(), time.Now())
	if ok {
		t.Error("gate 2: expected false when recent HTTP activity exists")
	}
}

// Gate 3: Running builds present.
func TestShouldHibernate_Gate3_RunningBuilds(t *testing.T) {
	cr := baseController()
	gauges := freshGauges()
	gauges.RunningBuilds = 1
	ok, _ := shouldHibernate(cr, gauges, time.Now(), time.Now())
	if ok {
		t.Error("gate 3: expected false when running builds > 0")
	}
}

// Gate 4: Queue not empty.
func TestShouldHibernate_Gate4_QueueLength(t *testing.T) {
	cr := baseController()
	gauges := freshGauges()
	gauges.QueueLength = 3
	ok, _ := shouldHibernate(cr, gauges, time.Now(), time.Now())
	if ok {
		t.Error("gate 4: expected false when queue length > 0")
	}
}

// Gate 5: Gauges are stale.
func TestShouldHibernate_Gate5_StaleGauges(t *testing.T) {
	cr := baseController()
	gauges := freshGauges()
	receivedAt := time.Now().Add(-10 * time.Minute) // 10 min ago — stale
	ok, _ := shouldHibernate(cr, gauges, receivedAt, time.Now())
	if ok {
		t.Error("gate 5: expected false when gauges are stale (>5 min)")
	}
}

// Gate 5 variant: zero receivedAt (never received).
func TestShouldHibernate_Gate5_NeverReceived(t *testing.T) {
	cr := baseController()
	ok, _ := shouldHibernate(cr, freshGauges(), time.Time{}, time.Now())
	if ok {
		t.Error("gate 5: expected false when gauges never received")
	}
}

// lastActivityAt derivation: max of three sources.
func TestShouldHibernate_LastActivity_MaxOfThree(t *testing.T) {
	now := time.Now()
	readyTransition := now.Add(-120 * time.Minute) // 2h ago
	httpActivity := now.Add(-90 * time.Minute)     // 90min ago
	eventActivity := now.Add(-60 * time.Minute)    // 60min ago = most recent

	cr := baseController()
	cr.Status.Conditions = []v1alpha1.ControllerCondition{
		{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "JenkinsHealthy",
			LastTransitionTime: metav1.NewTime(readyTransition),
		},
	}
	gauges := &mitev1.IdleGauges{
		LastHttpActivityUnix: httpActivity.Unix(),
		LastEventUnix:        eventActivity.Unix(),
		RunningBuilds:        0,
		QueueLength:          0,
	}
	cr.Spec.Hibernation.GracePeriodMinutes = 50 // grace 50min, elapsed since 60min ago

	ok, lastActivity := shouldHibernate(cr, gauges, now, now)
	if !ok {
		t.Error("expected grace period to pass with 60min activity and 50min grace")
	}
	if lastActivity.Unix() != eventActivity.Unix() {
		t.Errorf("expected lastActivity=%d (event), got %d", eventActivity.Unix(), lastActivity.Unix())
	}

	// Now make HTTP activity the most recent.
	gauges.LastHttpActivityUnix = now.Add(-40 * time.Minute).Unix()
	ok, _ = shouldHibernate(cr, gauges, now, now)
	if ok {
		t.Error("expected gate to fail: HTTP activity 40min ago < grace 50min")
	}
}

// When TimerTriggerJobs > 0, hibernation should still be allowed (the condition
// flag is set by the caller, not inside shouldHibernate).
func TestShouldHibernate_TimerTriggerJobsDoNotBlock(t *testing.T) {
	cr := baseController()
	gauges := freshGauges()
	gauges.TimerTriggerJobs = 5 // has timer triggers
	ok, _ := shouldHibernate(cr, gauges, time.Now(), time.Now())
	if !ok {
		t.Error("timer trigger jobs should NOT block hibernation")
	}
}

// Verify that lastActivity falls back to FirstConnectedAt when the activity
// gauges have zero timestamps — the connect-time floor that prevents
// instant-parking a freshly-provisioned, never-touched controller.
func TestShouldHibernate_ConnectFloor_UsesReadyCondition(t *testing.T) {
	now := time.Now()
	connectedAt := now.Add(-90 * time.Minute)
	cr := baseController()
	cr.Status.FirstConnectedAt = &metav1.Time{Time: now.Add(-10 * time.Hour)} // old first-connect, must be ignored
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "JenkinsHealthy",
		LastTransitionTime: metav1.NewTime(connectedAt), // last entered Connected 90min ago
	}}
	gauges := &mitev1.IdleGauges{} // no activity gauges
	ok, lastActivity := shouldHibernate(cr, gauges, now, now)
	if !ok {
		t.Error("expected all gates to pass (90min > default grace)")
	}
	if lastActivity.Unix() != connectedAt.Unix() {
		t.Errorf("expected lastActivity=%d (Ready transition), got %d", connectedAt.Unix(), lastActivity.Unix())
	}
}

// When the Ready condition is absent, the connect-time floor falls back to the
// stable FirstConnectedAt timestamp.
func TestShouldHibernate_ConnectFloor_FallsBackToFirstConnected(t *testing.T) {
	now := time.Now()
	connectedAt := now.Add(-90 * time.Minute)
	cr := baseController()
	cr.Status.Conditions = nil
	cr.Status.FirstConnectedAt = &metav1.Time{Time: connectedAt}
	ok, lastActivity := shouldHibernate(cr, &mitev1.IdleGauges{}, now, now)
	if !ok {
		t.Error("expected all gates to pass (90min > default grace)")
	}
	if lastActivity.Unix() != connectedAt.Unix() {
		t.Errorf("expected lastActivity=%d (FirstConnectedAt fallback), got %d", connectedAt.Unix(), lastActivity.Unix())
	}
}

// A just-(re)connected controller with zero activity must NOT instant-park: the
// Ready-transition floor keeps it awake until the grace period elapses. This is
// the post-wake case — the floor is the most recent Connected transition, not
// the first-ever connect.
func TestShouldHibernate_FreshlyConnected_NotInstantParked(t *testing.T) {
	now := time.Now()
	cr := baseController()
	cr.Status.FirstConnectedAt = &metav1.Time{Time: now.Add(-10 * time.Hour)} // old first-connect
	cr.Status.Conditions = []v1alpha1.ControllerCondition{{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "JenkinsHealthy",
		LastTransitionTime: metav1.NewTime(now.Add(-1 * time.Minute)), // reconnected 1min ago
	}}
	cr.Spec.Hibernation.GracePeriodMinutes = 60
	ok, _ := shouldHibernate(cr, &mitev1.IdleGauges{}, now, now)
	if ok {
		t.Error("a controller reconnected 1min ago must not hibernate before the 60min grace")
	}
}

// ---------------------------------------------------------------------------
// Controller fixture + env test: VARROA_HIBERNATION_IGNORE_REGEX in STS
// ---------------------------------------------------------------------------

// TestSTSEnv_HibernationIgnoreRegexAbsent verifies the env var is absent when
// the regex is not configured.
func TestSTSEnv_HibernationIgnoreRegexAbsent(t *testing.T) {
	spec := StatefulSetSpec{
		Name:            "test-ctrl",
		Namespace:       "ns",
		ControllerName:  "test-ctrl",
		VarroaEndpoint:  "example.com:443",
		OIDCIssuer:      "https://issuer.example.com",
		VarroaLoginURL:  "https://login.example.com",
		OIDCUserClaim:   "sub",
		OIDCGroupClaim:  "groups",
		MitePubKeyPEM:   "pubkey",
		MitePubKeyKID:   "kid",
		ApikeyVerifyURL: "https://verify.example.com",
		CAPEM:           "ca",
		// HibernationIgnoreRegex is zero-value "" — should NOT produce env var.
	}
	sts := buildStatefulSet(spec)
	env := extractContainerEnv(sts, "jenkins")
	if _, ok := env["VARROA_HIBERNATION_IGNORE_REGEX"]; ok {
		t.Error("VARROA_HIBERNATION_IGNORE_REGEX should be absent when regex is empty")
	}
}

// TestSTSEnv_HibernationIgnoreRegexPresent verifies the env var is set when
// the regex is configured.
func TestSTSEnv_HibernationIgnoreRegexPresent(t *testing.T) {
	spec := StatefulSetSpec{
		Name:                   "test-ctrl",
		Namespace:              "ns",
		ControllerName:         "test-ctrl",
		VarroaEndpoint:         "example.com:443",
		OIDCIssuer:             "https://issuer.example.com",
		VarroaLoginURL:         "https://login.example.com",
		OIDCUserClaim:          "sub",
		OIDCGroupClaim:         "groups",
		MitePubKeyPEM:          "pubkey",
		MitePubKeyKID:          "kid",
		ApikeyVerifyURL:        "https://verify.example.com",
		CAPEM:                  "ca",
		HibernationIgnoreRegex: "^/health",
	}
	sts := buildStatefulSet(spec)
	env := extractContainerEnv(sts, "jenkins")
	val, ok := env["VARROA_HIBERNATION_IGNORE_REGEX"]
	if !ok {
		t.Fatal("VARROA_HIBERNATION_IGNORE_REGEX should be present when regex is set")
	}
	if val != "^/health" {
		t.Errorf("expected ^/health, got %q", val)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// extractContainerEnv reads environment variables from a rendered StatefulSet
// for the named container. Returns a map of env name → value.
func extractContainerEnv(sts *unstructured.Unstructured, containerName string) map[string]string {
	result := make(map[string]string)

	// NestedFieldNoCopy instead of NestedSlice: the STS builder produces
	// []map[string]interface{} (typed), which NestedSlice rejects.
	raw, found, err := unstructured.NestedFieldNoCopy(sts.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		panic(fmt.Sprintf("extract env: no containers: %v", err))
	}
	for _, ctr := range asMapSlice(raw) {
		if ctr["name"] != containerName {
			continue
		}
		for _, entry := range asMapSlice(ctr["env"]) {
			name, _ := entry["name"].(string)
			val, _ := entry["value"].(string)
			result[name] = val
		}
	}
	return result
}

// asMapSlice normalizes a slice that may be []interface{} or
// []map[string]interface{} into the latter.
func asMapSlice(v interface{}) []map[string]interface{} {
	switch s := v.(type) {
	case []map[string]interface{}:
		return s
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(s))
		for _, e := range s {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}
