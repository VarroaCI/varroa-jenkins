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
	token, err := mintWakeToken(spec, "Running", "")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if len(token) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("expected 32-char hex token, got %q (len=%d)", token, len(token))
	}
}

func TestMintWakeToken_HibernatedPhaseAndEmpty(t *testing.T) {
	token, err := mintWakeToken(nil, "Hibernated", "")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if len(token) != 32 {
		t.Errorf("expected 32-char hex token, got %q (len=%d)", token, len(token))
	}
}

func TestMintWakeToken_Idempotent(t *testing.T) {
	spec := &v1alpha1.HibernationSpec{Enabled: true}
	token, err := mintWakeToken(spec, "Running", "existing-token-123")
	if err != nil {
		t.Fatalf("mintWakeToken failed: %v", err)
	}
	if token != "" {
		t.Errorf("expected empty (no mint needed), got %q", token)
	}
}

func TestMintWakeToken_NotNeeded(t *testing.T) {
	// Not enabled, not Hibernated, no token.
	token, err := mintWakeToken(nil, "Running", "")
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
	token1, err := mintWakeToken(spec, "Running", "")
	if err != nil {
		t.Fatalf("first mintWakeToken failed: %v", err)
	}
	if len(token1) != 32 {
		t.Fatalf("expected 32-char token, got %q", token1)
	}
	// Token now exists: second call returns empty (no mint).
	token2, err := mintWakeToken(spec, "Running", token1)
	if err != nil {
		t.Fatalf("second mintWakeToken failed: %v", err)
	}
	if token2 != "" {
		t.Errorf("expected empty when token already exists, got %q", token2)
	}
	// Token cleared: third call creates a new token.
	token3, err := mintWakeToken(spec, "Running", "")
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

func TestWakeHibernatedController_TransitionArgs(t *testing.T) {
	tc := &testClient{statuses: map[string]string{}, powerCASChanged: true}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	r.WakeHibernatedController(context.Background(), "team-a", "foo")

	if len(tc.powerTransitions) != 1 {
		t.Fatalf("expected 1 powerState transition, got %d", len(tc.powerTransitions))
	}
	got := tc.powerTransitions[0]
	if got.name != "foo" || got.from != "Hibernated" || got.to != "Running" {
		t.Errorf("transition = %+v, want {foo Hibernated Running}", got)
	}
}

func TestWakeHibernatedController_NoOpWhenNotHibernated(t *testing.T) {
	// CAS reports no change (controller was not Hibernated). Wake must not panic
	// and must still have attempted exactly one CAS.
	tc := &testClient{statuses: map[string]string{}, powerCASChanged: false}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	r.WakeHibernatedController(context.Background(), "team-a", "foo")

	if len(tc.powerTransitions) != 1 {
		t.Fatalf("expected 1 CAS attempt, got %d", len(tc.powerTransitions))
	}
}

func TestWakeHibernatedController_ErrorIsSwallowed(t *testing.T) {
	tc := &testClient{statuses: map[string]string{}, powerCASErr: errors.New("conflict")}
	r := &Reconciler{client: tc, Logger: slog.Default()}

	// Must not panic; error is logged, not propagated (best-effort signal).
	r.WakeHibernatedController(context.Background(), "team-a", "foo")
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
