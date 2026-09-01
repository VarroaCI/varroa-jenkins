package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func upgradeDispatchOp(upgrade *v1alpha1.BroodUpgradeAction) *v1alpha1.BroodOperation {
	return &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbUpgrade, Upgrade: upgrade},
		},
	}
}

// TestDispatchTarget_Upgrade_GranularityB_ReleasesCurrentProfileImage: with no
// targetVersion, the target's OWN version/profile compute the release image,
// only the annotation is written, and spec.version is never touched.
func TestDispatchTarget_Upgrade_GranularityB_ReleasesCurrentProfileImage(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("target failed unexpectedly: %s", target.Reason)
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Annotations[annotationUpgradeRelease] != "jenkins/jenkins:2.570" {
		t.Errorf("release annotation = %q, want jenkins/jenkins:2.570", stored.Annotations[annotationUpgradeRelease])
	}
	if len(fc.ssaApplies) != 0 {
		t.Errorf("ssa applies = %d, want 0 (granularity B never writes spec.version)", len(fc.ssaApplies))
	}
}

// TestDispatchTarget_Upgrade_GranularityB_OverlayGovernsImage: an explicit
// resourceOverlay image always wins over the profile-resolved image.
func TestDispatchTarget_Upgrade_GranularityB_OverlayGovernsImage(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	ctrl.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: jenkins
        image: my-registry/custom:1.0
`}
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Annotations[annotationUpgradeRelease] != "my-registry/custom:1.0" {
		t.Errorf("release annotation = %q, want the overlay image", stored.Annotations[annotationUpgradeRelease])
	}
}

// TestDispatchTarget_Upgrade_GranularityA_ResolvesTargetVersionFresh: the
// image is resolved from targetVersion via a fresh ResolveProfile call, not
// from the target's current cr.Spec.Version/profile, and spec.version is
// written as the literal targetVersion string, not the resolved image.
func TestDispatchTarget_Upgrade_GranularityA_ResolvesTargetVersionFresh(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.479"
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Annotations[annotationUpgradeRelease] != "jenkins/jenkins:2.570" {
		t.Errorf("release annotation = %q, want the resolved targetVersion image", stored.Annotations[annotationUpgradeRelease])
	}
	if len(fc.ssaApplies) != 1 {
		t.Fatalf("ssa applies = %d, want 1", len(fc.ssaApplies))
	}
	call := fc.ssaApplies[0]
	got, _ := json.Marshal(call.spec)
	want, _ := json.Marshal(map[string]any{"version": "2.570"})
	if string(got) != string(want) {
		t.Errorf("spec = %s, want %s (the literal targetVersion string, not the resolved image)", got, want)
	}
	if call.fieldManager != "varroa-ui" || call.force {
		t.Errorf("fieldManager=%q force=%v, want varroa-ui / force=false", call.fieldManager, call.force)
	}
}

// TestDispatchTarget_Upgrade_PluginPinConflictFailsWithoutWriting: a
// conflicting pin fails the target with a distinct, named reason, and neither
// the annotation nor spec.version is ever written.
func TestDispatchTarget_Upgrade_PluginPinConflictFailsWithoutWriting(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	ctrl.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "cb1"}
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns1"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "jcasc-1"}}},
		},
		Status: v1alpha1.ComposedBundleStatus{ContentRef: "cb1-content"},
	})
	fc.configMaps = map[string]map[string]string{
		key("ns1", "cb1-content"): {"plugins.yaml": "plugins:\n  - artifactId: git\n    version: 0.0.1-does-not-match-any-real-pin\n"},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateFailed {
		t.Fatalf("state = %s, want Failed", target.State)
	}
	if got := target.Reason; len(got) < len("plugin pin conflict: ") || got[:len("plugin pin conflict: ")] != "plugin pin conflict: " {
		t.Errorf("reason = %q, want it to start with %q", got, "plugin pin conflict: ")
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := stored.Annotations[annotationUpgradeRelease]; ok {
		t.Error("release annotation must not be written on a plugin-pin conflict")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written on a plugin-pin conflict")
	}
}

// TestDispatchTarget_Upgrade_NotServableFailsDistinctlyFromPinConflict: a
// profile blocked by its own ProfileCandidate's PluginsServable=False fails
// the target with a distinct reason, before CheckPluginPins ever runs.
func TestDispatchTarget_Upgrade_NotServableFailsDistinctlyFromPinConflict(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-candidate"},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.1"},
		Status: v1alpha1.ProfileCandidateStatus{
			Phase: v1alpha1.ProfileCandidatePhaseReady,
			Conditions: []v1alpha1.ProfileCandidateCondition{
				{Type: v1alpha1.ConditionCandidatePluginsServable, Status: metav1.ConditionFalse, Message: "update center unreachable"},
			},
		},
	})

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateFailed {
		t.Fatalf("state = %s, want Failed", target.State)
	}
	const wantPrefix = "target profile not servable: "
	if len(target.Reason) < len(wantPrefix) || target.Reason[:len(wantPrefix)] != wantPrefix {
		t.Errorf("reason = %q, want it to start with %q", target.Reason, wantPrefix)
	}
	if got := target.Reason; got != wantPrefix+"update center unreachable" {
		t.Errorf("reason = %q, want the condition message appended", got)
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := stored.Annotations[annotationUpgradeRelease]; ok {
		t.Error("release annotation must not be written when the profile is not servable")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written when the profile is not servable")
	}
}

// notServableCandidate builds a ProfileCandidate for "p1" carrying
// PluginsServable=False, in the given phase.
func notServableCandidate(name string, phase v1alpha1.ProfileCandidatePhase) *v1alpha1.ProfileCandidate {
	return &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.1"},
		Status: v1alpha1.ProfileCandidateStatus{
			Phase: phase,
			Conditions: []v1alpha1.ProfileCandidateCondition{
				{Type: v1alpha1.ConditionCandidatePluginsServable, Status: metav1.ConditionFalse, Message: "update center unreachable"},
			},
		},
	}
}

// TestDispatchTarget_Upgrade_SupersededCandidateDoesNotBlock: a Superseded
// candidate's PluginsServable=False describes a closed evaluation, not the
// profile's current servability, so it must not gate a later dispatch.
func TestDispatchTarget_Upgrade_SupersededCandidateDoesNotBlock(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, notServableCandidate("p1-old", v1alpha1.ProfileCandidatePhaseSuperseded))

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("a Superseded candidate must not block dispatch, got: %s", target.Reason)
	}
}

// TestDispatchTarget_Upgrade_FailedCandidateDoesNotBlock mirrors the
// Superseded case for a Failed candidate.
func TestDispatchTarget_Upgrade_FailedCandidateDoesNotBlock(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, notServableCandidate("p1-old", v1alpha1.ProfileCandidatePhaseFailed))

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("a Failed candidate must not block dispatch, got: %s", target.Reason)
	}
}

// TestDispatchTarget_Upgrade_OpenCandidateStillBlocks confirms Pending and
// Ready candidates keep gating a dispatch.
func TestDispatchTarget_Upgrade_OpenCandidateStillBlocks(t *testing.T) {
	for _, phase := range []v1alpha1.ProfileCandidatePhase{v1alpha1.ProfileCandidatePhasePending, v1alpha1.ProfileCandidatePhaseReady} {
		t.Run(string(phase), func(t *testing.T) {
			ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
			ctrl.Spec.Version = "2.555"
			op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
			target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
			rec, fc, _ := newBORec(t, op, ctrl)
			crdstore.MustSeed(fc.store, ctrl)
			crdstore.MustSeed(fc.store, lineProfile("2.555"))
			crdstore.MustSeed(fc.store, notServableCandidate("p1-current", phase))

			if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
				t.Fatalf("dispatchTarget: %v", err)
			}
			if target.State != v1alpha1.BroodTargetStateFailed {
				t.Fatalf("a %s candidate must still block dispatch, state = %s", phase, target.State)
			}
		})
	}
}

// TestDispatchTarget_Upgrade_SupersededAlongsideReadyIsServable: a stale
// Superseded False candidate must not shadow a current Ready True one for
// the same profile.
func TestDispatchTarget_Upgrade_SupersededAlongsideReadyIsServable(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, notServableCandidate("p1-old", v1alpha1.ProfileCandidatePhaseSuperseded))
	crdstore.MustSeed(fc.store, &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-current"},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.2"},
		Status: v1alpha1.ProfileCandidateStatus{
			Phase: v1alpha1.ProfileCandidatePhaseReady,
			Conditions: []v1alpha1.ProfileCandidateCondition{
				{Type: v1alpha1.ConditionCandidatePluginsServable, Status: metav1.ConditionTrue},
			},
		},
	})

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("a stale Superseded candidate must not shadow a current Ready one, got: %s", target.Reason)
	}
}

// TestDispatchTarget_Upgrade_NoCandidateSkipsServableCheck: a profile with no
// ProfileCandidate at all is always treated as servable, and dispatch still
// reaches (and passes) CheckPluginPins.
func TestDispatchTarget_Upgrade_NoCandidateSkipsServableCheck(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	// No ProfileCandidate and no bundle content (so CheckPluginPins is skipped
	// for lack of content, not for lack of a candidate) — this must still
	// succeed rather than being blocked by a missing candidate.

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("a profile with no ProfileCandidate must not be treated as not-servable, got: %s", target.Reason)
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := stored.Annotations[annotationUpgradeRelease]; !ok {
		t.Error("expected the release annotation to be written")
	}
}

// TestDispatchTarget_Upgrade_AnnotationWriteFailurePreventsSpecVersionWrite
// pins the write ordering for granularity A: the release annotation is
// written first, and a failure there must prevent spec.version from ever
// being attempted.
func TestDispatchTarget_Upgrade_AnnotationWriteFailurePreventsSpecVersionWrite(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.479"
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	fc.store.FailAlways("patchmeta", ctrlGVR, errors.New("etcdserver: request timed out"))

	if err := rec.dispatchTarget(context.Background(), op, target); err == nil {
		t.Fatal("dispatchTarget succeeded despite the annotation write failing, want error")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must never be attempted when the annotation write fails first")
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetSucceedsImmediately_GranularityB:
// a re-run against a target already running the resolved image succeeds on
// this same dispatch, without writing the release annotation or waiting for
// a phase transition that will never come. Granularity B (release, no
// targetVersion) has no spec.version to write, so nothing is ever written to
// the controller.
func TestDispatchTarget_Upgrade_AlreadyAtTargetSucceedsImmediately_GranularityB(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateSucceeded {
		t.Fatalf("state = %s, want Succeeded (already at target, nothing to release)", target.State)
	}
	if target.FinishedAt == nil {
		t.Error("an immediately-succeeded target must stamp FinishedAt")
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := stored.Annotations[annotationUpgradeRelease]; ok {
		t.Error("release annotation must not be written when there is nothing to release")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written for granularity B, which has no pin")
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_StillWritesPin: a
// granularity-A "move to version" against a target already running the
// resolved image must still advance spec.version to the requested version
// before reporting success — the controller may be line-pinned to an older
// line whose latest promoted patch already happens to match the requested
// image, and skipping the pin write would report success for a move that
// never took the controller off its old line.
func TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_StillWritesPin(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555" // pinned to an older line
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		// Already running the image 2.570 resolves to, despite spec.version
		// still naming the older 2.555 line.
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateSucceeded {
		t.Fatalf("state = %s, want Succeeded (already at target image)", target.State)
	}
	if target.FinishedAt == nil {
		t.Error("an immediately-succeeded target must stamp FinishedAt")
	}
	if len(fc.ssaApplies) != 1 {
		t.Fatalf("ssa applies = %d, want 1 (the pin must still be written even though the image already matches)", len(fc.ssaApplies))
	}
	call := fc.ssaApplies[0]
	got, _ := json.Marshal(call.spec)
	want, _ := json.Marshal(map[string]any{"version": "2.570"})
	if string(got) != string(want) {
		t.Errorf("spec = %s, want %s", got, want)
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_PinWriteFailureDoesNotSucceed:
// when the already-at-target pin write itself fails, the target must not be
// reported Succeeded — a failed pin write means spec.version was never
// advanced, so the same non-idempotent gap this short-circuit exists to close
// would otherwise reopen.
func TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_PinWriteFailureDoesNotSucceed(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}
	fc.ssaApplyErr = errors.New("apply failed")

	if err := rec.dispatchTarget(context.Background(), op, target); err == nil {
		t.Fatal("dispatchTarget: want an error from the failed pin write, got nil")
	}
	if target.State == v1alpha1.BroodTargetStateSucceeded {
		t.Fatal("state = Succeeded, want the target left unreported since the pin write failed")
	}
	if target.FinishedAt != nil {
		t.Error("FinishedAt must not be stamped when the pin write failed")
	}
}

// TestDispatchTarget_Upgrade_RealDeltaStillWaitsForPhaseTransition: a target
// whose applied image differs from the resolved target image is not
// short-circuited — it still goes through the normal release-then-observe
// flow, and evaluateDispatchedTarget still requires the phase to leave and
// return to Connected before reporting success.
func TestDispatchTarget_Upgrade_RealDeltaStillWaitsForPhaseTransition(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.479"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateSucceeded {
		t.Fatal("state = Succeeded, want the target to still be waiting on a real delta")
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Annotations[annotationUpgradeRelease] != "jenkins/jenkins:2.570" {
		t.Errorf("release annotation = %q, want the resolved target image", stored.Annotations[annotationUpgradeRelease])
	}

	target.State = v1alpha1.BroodTargetStateDispatched
	dispatchedAt := metav1.NewTime(frozenNow.Add(-time.Minute))
	target.DispatchedAt = &dispatchedAt
	rec.evaluateDispatchedTarget(context.Background(), op, target, stored, v1alpha1.BroodVerbUpgrade, broodVerbTimeouts[v1alpha1.BroodVerbUpgrade], frozenNow)
	if target.State != v1alpha1.BroodTargetStateDispatched {
		t.Errorf("state = %s, want Dispatched (annotation still held, phase never left Connected)", target.State)
	}
}

// TestDispatchTarget_Upgrade_HeldTargetStillReportsHeld: a target already
// held by upgradePolicy=manual (ConditionUpgradePending set from a prior
// tick) is not mistaken for one that is already at its target image merely
// because a dispatch was re-run against it — the held target's applied image
// is genuinely behind, so it keeps waiting rather than succeeding.
func TestDispatchTarget_Upgrade_HeldTargetStillReportsHeld(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	ctrl.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionUpgradePending, Status: metav1.ConditionTrue, Reason: "UpgradePending", Message: "upgrade held by upgradePolicy=manual"},
	}
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.479"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State == v1alpha1.BroodTargetStateSucceeded {
		t.Fatal("state = Succeeded, want a held target (real delta) to keep waiting, not succeed")
	}

	target.State = v1alpha1.BroodTargetStateDispatched
	dispatchedAt := metav1.NewTime(frozenNow.Add(-time.Minute))
	target.DispatchedAt = &dispatchedAt
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rec.evaluateDispatchedTarget(context.Background(), op, target, stored, v1alpha1.BroodVerbUpgrade, broodVerbTimeouts[v1alpha1.BroodVerbUpgrade], frozenNow)
	if target.State != v1alpha1.BroodTargetStateDispatched {
		t.Errorf("state = %s, want Dispatched (still held, not succeeded)", target.State)
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_NotServableFails: an
// already-at-target granularity-A move still runs the profile-servable
// preflight before deciding the outcome — an unservable profile fails the
// target rather than reporting success (and writing the pin) for a move
// whose plugin set a later reconcile would then reject.
func TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_NotServableFails(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	crdstore.MustSeed(fc.store, notServableCandidate("p1-candidate", v1alpha1.ProfileCandidatePhaseReady))
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		// Already running the image 2.570 resolves to.
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateFailed {
		t.Fatalf("state = %s, want Failed (unservable profile must fail even though the image already matches)", target.State)
	}
	const wantPrefix = "target profile not servable: "
	if len(target.Reason) < len(wantPrefix) || target.Reason[:len(wantPrefix)] != wantPrefix {
		t.Errorf("reason = %q, want it to start with %q", target.Reason, wantPrefix)
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written when the profile is not servable")
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_PinConflictFails: an
// already-at-target granularity-A move still runs the plugin-pin preflight
// before deciding the outcome — a conflicting pin fails the target and still
// emits the pin-conflict event, rather than reporting success (and writing
// the pin) for a plugin set the preflight would have rejected.
func TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityA_PinConflictFails(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	ctrl.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "cb1"}
	targetVersion := "2.570"
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{TargetVersion: &targetVersion})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	crdstore.MustSeed(fc.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns1"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "jcasc-1"}}},
		},
		Status: v1alpha1.ComposedBundleStatus{ContentRef: "cb1-content"},
	})
	fc.configMaps = map[string]map[string]string{
		key("ns1", "cb1-content"): {"plugins.yaml": "plugins:\n  - artifactId: git\n    version: 0.0.1-does-not-match-any-real-pin\n"},
	}
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}
	var captured []activity.Event
	rec.eventSink = func(e activity.Event) { captured = append(captured, e) }

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateFailed {
		t.Fatalf("state = %s, want Failed (a pin conflict must fail even though the image already matches)", target.State)
	}
	if got := target.Reason; len(got) < len("plugin pin conflict: ") || got[:len("plugin pin conflict: ")] != "plugin pin conflict: " {
		t.Errorf("reason = %q, want it to start with %q", got, "plugin pin conflict: ")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written on a plugin-pin conflict")
	}
	found := false
	for _, e := range captured {
		if e.Type == "pluginPinConflict.detected" {
			found = true
		}
	}
	if !found {
		t.Error("expected a pluginPinConflict.detected event even on the already-at-target path")
	}
}

// TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityB_PinConflictFails: the
// same preflight-before-outcome ordering applies to granularity B (release):
// an already-at-target release with a conflicting pin fails rather than
// short-circuiting to success.
func TestDispatchTarget_Upgrade_AlreadyAtTargetGranularityB_PinConflictFails(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.570"
	ctrl.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "cb1"}
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.570"))
	crdstore.MustSeed(fc.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns1"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "jcasc-1"}}},
		},
		Status: v1alpha1.ComposedBundleStatus{ContentRef: "cb1-content"},
	})
	fc.configMaps = map[string]map[string]string{
		key("ns1", "cb1-content"): {"plugins.yaml": "plugins:\n  - artifactId: git\n    version: 0.0.1-does-not-match-any-real-pin\n"},
	}
	fc.statefulSetImages = map[string]statefulSetImagesFixture{
		key("ns1", controllerPrefix(ctrl)): {live: map[string]string{"jenkins": "jenkins/jenkins:2.570"}},
	}

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if target.State != v1alpha1.BroodTargetStateFailed {
		t.Fatalf("state = %s, want Failed (a pin conflict must fail even though granularity B has nothing else to release)", target.State)
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must never be written for granularity B")
	}
}
