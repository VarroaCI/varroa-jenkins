package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// upgradeOpWithVersion builds a granularity-A (bulk-write) upgrade operation
// targeting a single controller, for exercising the admission-time
// targetVersion validation step in reconcilePending.
func upgradeOpWithVersion(targetVersion string) *v1alpha1.BroodOperation {
	return &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:    v1alpha1.BroodVerbUpgrade,
				Upgrade: &v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion},
			},
			Targets: v1alpha1.BroodTargets{Names: []string{"c1"}},
		},
	}
}

func lineProfile(version string) *v1alpha1.JenkinsVersionProfile {
	return &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p1"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: version},
	}
}

func TestReconcilePendingUpgrade_ExactMatchPassesAdmission(t *testing.T) {
	op := upgradeOpWithVersion("2.570")
	rec, fc, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Phase == v1alpha1.BroodOperationPhaseFailed {
		t.Fatalf("exact-match targetVersion must pass admission, got Failed: %s", op.Status.Reason)
	}
	if len(op.Status.Targets) == 0 {
		t.Error("a passing admission must proceed to target resolution")
	}
}

func TestReconcilePendingUpgrade_LineMatchPassesAdmission(t *testing.T) {
	op := upgradeOpWithVersion("2.570.5")
	rec, fc, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Phase == v1alpha1.BroodOperationPhaseFailed {
		t.Fatalf("line-match targetVersion must pass admission, got Failed: %s", op.Status.Reason)
	}
	if len(op.Status.Targets) == 0 {
		t.Error("a passing admission must proceed to target resolution")
	}
}

func TestReconcilePendingUpgrade_SentinelsPassAdmissionWithNoProfiles(t *testing.T) {
	for _, tv := range []string{"", "lts"} {
		t.Run(tv, func(t *testing.T) {
			op := upgradeOpWithVersion(tv)
			rec, _, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
			// No JenkinsVersionProfile seeded at all: the sentinel must pass
			// without ever needing a match.
			if err := rec.reconcilePending(context.Background(), op); err != nil {
				t.Fatalf("reconcilePending: %v", err)
			}
			if op.Status.Phase == v1alpha1.BroodOperationPhaseFailed {
				t.Fatalf("targetVersion %q must pass admission regardless of profiles, got Failed: %s", tv, op.Status.Reason)
			}
		})
	}
}

func TestReconcilePendingUpgrade_NonResolvingVersionFailsAdmission(t *testing.T) {
	op := upgradeOpWithVersion("9.9.9")
	rec, fc, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
		t.Fatalf("phase = %s, want Failed", op.Status.Phase)
	}
	if op.Status.Reason != "TargetVersionUnresolved" {
		t.Errorf("reason = %q, want TargetVersionUnresolved", op.Status.Reason)
	}
	if op.Status.FinishedAt == nil {
		t.Error("a failed admission must be marked finished")
	}
	if len(op.Status.Targets) != 0 {
		t.Error("no target should be written when admission fails")
	}
}

func TestReconcilePendingUpgrade_UnreadableProfileListFailsAdmission(t *testing.T) {
	op := upgradeOpWithVersion("2.570")
	rec, fc, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
	gvr, err := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	fc.store.FailAlways("list", gvr, errors.New("etcdserver: request timed out"))

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
		t.Fatalf("phase = %s, want Failed", op.Status.Phase)
	}
	if op.Status.Reason != "TargetVersionUnresolved" {
		t.Errorf("reason = %q, want TargetVersionUnresolved", op.Status.Reason)
	}
	if len(op.Status.Targets) != 0 {
		t.Error("no target should be written when the profile list is unreadable")
	}
}

// Granularity B never resolves a targetVersion, so an unreadable profile list
// must not affect it at all: the admission step must not even run.
func TestReconcilePendingUpgrade_GranularityBSkipsVersionValidation(t *testing.T) {
	op := upgradeOp("team-a")
	op.Spec.Targets = v1alpha1.BroodTargets{Names: []string{"c1"}}
	rec, fc, _ := newBORec(t, op, testCtrl2("c1", "team-a", v1alpha1.ControllerPhaseConnected))
	gvr, err := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	fc.store.FailAlways("list", gvr, errors.New("etcdserver: request timed out"))

	if err := rec.reconcilePending(context.Background(), op); err != nil {
		t.Fatalf("reconcilePending: %v", err)
	}
	if op.Status.Reason == "TargetVersionUnresolved" {
		t.Fatal("granularity B must not run the targetVersion admission step at all")
	}
}
