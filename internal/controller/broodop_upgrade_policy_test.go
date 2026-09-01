package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// upgradeOp builds a granularity-B (release) upgrade operation, which never
// exercises the admission-time targetVersion resolution step and so isolates
// verbPolicyAllows/checkApplicability from that unrelated path.
func upgradeOp(ns string) *v1alpha1.BroodOperation {
	return &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:    v1alpha1.BroodVerbUpgrade,
				Upgrade: &v1alpha1.BroodUpgradeAction{},
			},
		},
	}
}

// TestCheckApplicability_Upgrade_RequiresConnected mirrors
// TestCheckApplicability_ExecuteGroovy_RequiresConnected: a non-Connected
// target is skipped, not failed, so checkApplicability returns a reason
// string rather than an error.
func TestCheckApplicability_Upgrade_RequiresConnected(t *testing.T) {
	connected := testCtrl2("c", "ns", v1alpha1.ControllerPhaseConnected)
	stopped := testCtrl2("s", "ns", v1alpha1.ControllerPhaseStopped)

	if reason := checkApplicability(v1alpha1.BroodVerbUpgrade, connected); reason != "" {
		t.Errorf("Connected target should be applicable, got reason %q", reason)
	}
	if reason := checkApplicability(v1alpha1.BroodVerbUpgrade, stopped); reason != "not Connected" {
		t.Errorf(`reason = %q, want "not Connected"`, reason)
	}
}

func TestVerbPolicy_Upgrade_Disabled(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		Upgrade: &v1alpha1.BroodUpgradePolicy{Enabled: policyBool(false)},
	})
	ok, why := rec.verbPolicyAllows(context.Background(), upgradeOp("team-a"))
	if ok {
		t.Fatal("enabled=false must deny upgrade")
	}
	if !strings.Contains(why, "broodPolicy") {
		t.Errorf("denial should name the policy that caused it, got %q", why)
	}
}

func TestVerbPolicy_Upgrade_AllowedNamespaces(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		Upgrade: &v1alpha1.BroodUpgradePolicy{AllowedNamespaces: []string{"team-a", "team-b"}},
	})
	if ok, _ := rec.verbPolicyAllows(context.Background(), upgradeOp("team-a")); !ok {
		t.Error("a listed namespace must be allowed")
	}
	ok, why := rec.verbPolicyAllows(context.Background(), upgradeOp("team-c"))
	if ok {
		t.Fatal("an unlisted namespace must be denied")
	}
	if !strings.Contains(why, "team-a") || !strings.Contains(why, "team-c") {
		t.Errorf("denial should name both the namespace and the allow-list, got %q", why)
	}
}

// A restart operation must be unaffected even by the most restrictive upgrade
// policy: the two policies gate their own verb only.
func TestVerbPolicy_Upgrade_OtherVerbsUngoverned(t *testing.T) {
	rec, fc, _ := newBORec(t)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		Upgrade: &v1alpha1.BroodUpgradePolicy{
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
		t.Fatal("upgrade policy must not govern other verbs")
	}
}

// Checking policy only at the Pending transition would make the switch
// decorative for a long-running fleet-wide upgrade, exactly as it would for
// executeGroovy.
func TestReconcileRunningStopsWhenUpgradePolicyRevoked(t *testing.T) {
	op := upgradeOp("team-a")
	op.Status.Phase = v1alpha1.BroodOperationPhaseRunning
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "team-a", Name: "c1", State: v1alpha1.BroodTargetStateSucceeded},
		{Namespace: "team-a", Name: "c2", State: v1alpha1.BroodTargetStatePending},
		{Namespace: "team-a", Name: "c3", State: v1alpha1.BroodTargetStatePending},
	}
	rec, fc, _ := newBORec(t, op,
		testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected),
		testCtrl2("c2", "team-a", v1alpha1.ControllerPhaseConnected),
		testCtrl2("c3", "team-a", v1alpha1.ControllerPhaseConnected),
	)
	seedDefaults(t, fc, &v1alpha1.BroodPolicy{
		Upgrade: &v1alpha1.BroodUpgradePolicy{Enabled: policyBool(false)},
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
	if op.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
		t.Error("a terminal target must not be re-marked")
	}
}
