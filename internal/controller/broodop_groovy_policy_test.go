package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func policyBool(b bool) *bool { return &b }

func groovyOp(ns string) *v1alpha1.BroodOperation {
	return &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hi'"},
			},
		},
	}
}

func seedDefaults(t *testing.T, fc *fakeBroodClient, policy *v1alpha1.BroodPolicy) {
	t.Helper()
	crdstore.MustSeed(fc.store, &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: provisioningDefaultsName},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{BroodPolicy: policy},
	})
}

func TestVerbPolicy_NoDefaultsAllows(t *testing.T) {
	rec, _, _ := newBORec(t)
	// The singleton is optional. Its absence must never disable a verb that is
	// on by default.
	if ok, why := rec.verbPolicyAllows(context.Background(), groovyOp("team-a")); !ok {
		t.Fatalf("absent ProvisioningDefaults must allow executeGroovy, got %q", why)
	}
}

func TestVerbPolicy_NilPolicyFieldsAllow(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, nil)
	if ok, _ := rec.verbPolicyAllows(context.Background(), groovyOp("team-a")); !ok {
		t.Fatal("a ProvisioningDefaults with no broodPolicy must allow executeGroovy")
	}

	rec2, fc2, _ := newBORec(t)
	seedDefaults(t, fc2, &v1alpha1.BroodPolicy{})
	if ok, _ := rec2.verbPolicyAllows(context.Background(), groovyOp("team-a")); !ok {
		t.Fatal("a broodPolicy with no executeGroovy section must allow executeGroovy")
	}

	rec3, fc3, _ := newBORec(t)
	seedDefaults(t, fc3, &v1alpha1.BroodPolicy{ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{}})
	if ok, _ := rec3.verbPolicyAllows(context.Background(), groovyOp("team-a")); !ok {
		t.Fatal("an executeGroovy section with nil enabled must allow executeGroovy")
	}
}

func TestVerbPolicy_Disabled(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{Enabled: policyBool(false)},
	})
	ok, why := rec.verbPolicyAllows(context.Background(), groovyOp("team-a"))
	if ok {
		t.Fatal("enabled=false must deny executeGroovy")
	}
	if !strings.Contains(why, "broodPolicy") {
		t.Errorf("denial should name the policy that caused it, got %q", why)
	}
}

// Enabled is the outer gate: a namespace on the allow-list is still denied when
// the verb is switched off entirely.
func TestVerbPolicy_DisabledBeatsAllowedNamespaces(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{
			Enabled:           policyBool(false),
			AllowedNamespaces: []string{"team-a"},
		},
	})
	if ok, _ := rec.verbPolicyAllows(context.Background(), groovyOp("team-a")); ok {
		t.Fatal("enabled=false must deny even a listed namespace")
	}
}

func TestVerbPolicy_AllowedNamespaces(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{AllowedNamespaces: []string{"team-a", "team-b"}},
	})
	if ok, _ := rec.verbPolicyAllows(context.Background(), groovyOp("team-a")); !ok {
		t.Error("a listed namespace must be allowed")
	}
	ok, why := rec.verbPolicyAllows(context.Background(), groovyOp("team-c"))
	if ok {
		t.Fatal("an unlisted namespace must be denied")
	}
	// The message has to name the allowed set, or the operator cannot tell
	// whether to change the policy or move the operation.
	if !strings.Contains(why, "team-a") || !strings.Contains(why, "team-c") {
		t.Errorf("denial should name both the namespace and the allow-list, got %q", why)
	}
}

// The policy governs executeGroovy only. A restart operation must be unaffected
// even by the most restrictive policy.
func TestVerbPolicy_OtherVerbsUngoverned(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{
			Enabled:           policyBool(false),
			AllowedNamespaces: []string{"nowhere"},
		},
	})
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-2", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
		},
	}
	if ok, _ := rec.verbPolicyAllows(context.Background(), op); !ok {
		t.Fatal("executeGroovy policy must not govern other verbs")
	}
}

// A denied operation must end terminally with a reason, not hang in Pending:
// BroodOperationStatus has no conditions, so phase+reason is the only channel.
func TestReconcilePendingDeniedByPolicy(t *testing.T) {
	op := groovyOp("team-a")
	rec, fc, _ := newBORec(t, op)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{Enabled: policyBool(false)},
	})

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
		t.Fatalf("phase = %s, want Failed", op.Status.Phase)
	}
	if !strings.Contains(op.Status.Reason, "broodPolicy") {
		t.Errorf("reason should explain the denial, got %q", op.Status.Reason)
	}
	if op.Status.FinishedAt == nil {
		t.Error("a denied operation must be marked finished")
	}
}

// Provenance is a pointer, never a body: activity is retained for up to 90 days
// and Groovy scripts routinely carry credentials.
func TestGroovyProvenanceNeverCarriesTheScript(t *testing.T) {
	secret := "println 'hunter2-token'"
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: secret},
			},
		},
	}
	got := groovyProvenance(op)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("provenance leaked the script body: %s", got)
	}
	if !strings.Contains(got, "scriptSource=inline") || !strings.Contains(got, "scriptSha256=") {
		t.Errorf("inline script should be identified by digest, got %q", got)
	}
	if !strings.Contains(got, "scriptBytes=23") {
		t.Errorf("inline script should report its length, got %q", got)
	}
}

func TestGroovyProvenanceForCatalogScript(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "cleanup"},
				},
			},
		},
		Status: v1alpha1.BroodOperationStatus{ScriptSnapshotRef: "run-1-groovy-script"},
	}
	got := groovyProvenance(op)
	for _, want := range []string{
		"scriptSource=catalog",
		"scriptItemRef=team-a/cleanup",                 // namespace defaulted to the op's
		"scriptSnapshotRef=team-a/run-1-groovy-script", // where the bytes actually are
	} {
		if !strings.Contains(got, want) {
			t.Errorf("provenance %q missing %q", got, want)
		}
	}
}

func TestGroovyProvenanceEmptyForOtherVerbs(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
		},
	}
	if got := groovyProvenance(op); got != "" {
		t.Errorf("non-groovy verb should carry no script provenance, got %q", got)
	}
}

// The finished event is where an auditor looks. It must say which verb ran, who
// started it, and which script — the message alone previously said none of that.
func TestTargetFinishedEventCarriesVerbActorAndProvenance(t *testing.T) {
	rec, _, _ := newBORec(t)
	op := groovyOp("team-a")
	op.Status.StartedBy = "alice@example.com"
	target := &v1alpha1.BroodTargetStatus{
		Namespace: "team-a", Name: "ctl-1", State: v1alpha1.BroodTargetStateSucceeded,
	}

	e := rec.buildTargetFinishedEvent(op, target)
	if e.Actor != "alice@example.com" {
		t.Errorf("actor = %q, want the operation's startedBy", e.Actor)
	}
	if !strings.Contains(e.Message, "verb=executeGroovy") {
		t.Errorf("message should name the verb, got %q", e.Message)
	}
	if !strings.Contains(e.Message, "scriptSha256=") {
		t.Errorf("message should carry script provenance, got %q", e.Message)
	}
	if strings.Contains(e.Message, "println") {
		t.Errorf("message leaked the script body: %q", e.Message)
	}
}

// Checking policy only at the Pending transition made the switch decorative: a
// 100-target operation that started while allowed would run all 100 after an
// admin disabled the verb.
func TestReconcileRunningStopsWhenPolicyRevoked(t *testing.T) {
	op := groovyOp("team-a")
	op.Status.Phase = v1alpha1.BroodOperationPhaseRunning
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "team-a", Name: "c1", State: v1alpha1.BroodTargetStateSucceeded},
		{Namespace: "team-a", Name: "c2", State: v1alpha1.BroodTargetStatePending},
		{Namespace: "team-a", Name: "c3", State: v1alpha1.BroodTargetStatePending},
	}
	// The targets must exist and be Connected, or evaluateTargets — which now
	// correctly runs before the policy gate so in-flight targets still time out —
	// would fail them for being absent and the assertions below would pass for
	// the wrong reason.
	rec, fc, _ := newBORec(t, op,
		testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected),
		testCtrl2("c2", "team-a", v1alpha1.ControllerPhaseConnected),
		testCtrl2("c3", "team-a", v1alpha1.ControllerPhaseConnected),
	)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{Enabled: policyBool(false)},
	})

	if err := rec.reconcileRunning(context.Background(), op, slog.Default()); err != nil {
		t.Fatalf("reconcileRunning: %v", err)
	}

	for _, tg := range op.Status.Targets[1:] {
		if tg.State != v1alpha1.BroodTargetStateSkipped {
			t.Errorf("target %s state = %s, want Skipped after the policy was revoked", tg.Name, tg.State)
		}
		if !strings.Contains(tg.Reason, "broodPolicy") {
			t.Errorf("target %s reason should explain the cancellation, got %q", tg.Name, tg.Reason)
		}
	}
	// An already-succeeded target must not be rewritten.
	if op.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
		t.Error("a terminal target must not be re-marked")
	}
}

// The Jenkins client embeds up to 4 KiB of response body in its error, and a
// script-console compilation failure echoes the submitted source back in it.
// That body must not reach a 90-day retention stream.
func TestEventReasonStripsJenkinsResponseBody(t *testing.T) {
	op := groovyOp("team-a")
	leaky := `script console once: HTTP 500: groovy.lang.MissingPropertyException: ` +
		`No such property: hunter2Token for class: Script1`

	got := eventReason(op, leaky)
	if strings.Contains(got, "hunter2Token") {
		t.Fatalf("activity reason leaked the script source: %q", got)
	}
	if !strings.Contains(got, "500") {
		t.Errorf("the HTTP status is the actionable part and should survive, got %q", got)
	}

	// Non-groovy verbs are unaffected — their reasons are Varroa's own text.
	restart := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
		},
	}
	if got := eventReason(restart, "restart result timeout"); got != "restart result timeout" {
		t.Errorf("non-groovy reason should pass through, got %q", got)
	}

	// A transport error carries a URL, not a body, but is still capped.
	long := "script console once: dial tcp " + strings.Repeat("x", 400)
	if got := eventReason(op, long); len(got) > 210 {
		t.Errorf("reason should be capped, got %d chars", len(got))
	}
}

// A status patch can fail between resolving the script and the finished event.
// An audit record that silently drops its pointer is worse than one naming a
// ConfigMap the reader can go check for.
func TestGroovyProvenanceFallsBackToDeterministicSnapshotName(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-7", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "cleanup"},
				},
			},
		},
		// ScriptSnapshotRef deliberately unset.
	}
	got := groovyProvenance(op)
	if !strings.Contains(got, "scriptSnapshotRef=team-a/run-7-groovy-script") {
		t.Errorf("provenance should fall back to the deterministic snapshot name, got %q", got)
	}
}

// Cancelling on policy revocation must not strand in-flight targets. If the gate
// short-circuited before evaluateTargets, a Dispatched target would never have
// its timeout predicate evaluated and the operation would sit in Running
// forever — a worse failure than the one the gate prevents.
func TestReconcileRunningStillTimesOutDispatchedTargetsWhenPolicyRevoked(t *testing.T) {
	op := groovyOp("team-a")
	op.Status.Phase = v1alpha1.BroodOperationPhaseRunning
	longAgo := metav1.NewTime(frozenNow.Add(-2 * time.Hour))
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{
			Namespace: "team-a", Name: "c1",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &longAgo,
		},
		{Namespace: "team-a", Name: "c2", State: v1alpha1.BroodTargetStatePending},
	}
	rec, fc, _ := newBORec(t, op,
		testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected),
		testCtrl2("c2", "team-a", v1alpha1.ControllerPhaseConnected),
	)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		ExecuteGroovy: &v1alpha1.ExecuteGroovyPolicy{Enabled: policyBool(false)},
	})

	if err := rec.reconcileRunning(context.Background(), op, slog.Default()); err != nil {
		t.Fatalf("reconcileRunning: %v", err)
	}

	if op.Status.Targets[0].State == v1alpha1.BroodTargetStateDispatched {
		t.Error("a long-overdue dispatched target must still be resolved, not left in flight")
	}
	if op.Status.Targets[1].State != v1alpha1.BroodTargetStateSkipped {
		t.Errorf("pending target state = %s, want Skipped", op.Status.Targets[1].State)
	}
}
