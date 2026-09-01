package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestUpgradePolicyVersionRollGate covers the upgradePolicy dial composed at
// the versionRollGate seam.
func TestUpgradePolicyVersionRollGate(t *testing.T) {
	ctx := context.Background()

	t.Run("manual holds a profile-driven roll", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"

		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition: upgradePolicy=manual must hold the roll")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionTrue || vc.Reason != "UpgradePending" {
			t.Errorf("VersionRollPending = %+v, want True/UpgradePending", vc)
		}
		uc := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpgradePending)
		if uc == nil || uc.Status != metav1.ConditionTrue {
			t.Errorf("UpgradePending = %+v, want True", uc)
		}
	})

	t.Run("clears on convergence regardless of cause", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		// Pre-seed a held state as if a previous reconcile had held it.
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionUpgradePending,
			Status: metav1.ConditionTrue,
			Reason: "UpgradePending",
		})

		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition (already converged)")
		}
		uc := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpgradePending)
		if uc == nil || uc.Status != metav1.ConditionFalse {
			t.Errorf("UpgradePending = %+v, want False after convergence, regardless of what held it before", uc)
		}
	})

	t.Run("resourceOverlay-governed image always proceeds", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: jenkins\n        image: my-reg/custom:1.0\n",
		}

		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected transition: an explicit resourceOverlay image must proceed regardless of upgradePolicy=manual")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})

	t.Run("ProvisioningDefaults read error holds fail-safe", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		rec := newTestReconciler(client)
		pdGVR, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
		if err != nil {
			t.Fatalf("GVRFor: %v", err)
		}
		client.store.FailAlways("get", pdGVR, errors.New("etcdserver: request timed out"))

		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"

		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition: an unreadable ProvisioningDefaults must fail safe and hold")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionTrue || vc.Reason != "UpgradePending" {
			t.Errorf("VersionRollPending = %+v, want True/UpgradePending", vc)
		}
		const wantMsg = "upgrade held: ProvisioningDefaults unreadable, failing safe"
		if vc.Message != wantMsg {
			t.Errorf("VersionRollPending.Message = %q, want %q", vc.Message, wantMsg)
		}
		uc := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpgradePending)
		if uc == nil || uc.Status != metav1.ConditionTrue {
			t.Errorf("UpgradePending = %+v, want True", uc)
		}
	})

	t.Run("matching release annotation allows a manual-held roll and clears it", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		cr.Annotations = map[string]string{"varroa.dev/upgrade-release": "jenkins/jenkins:2.570.2"}
		crdstore.MustSeed(client.store, cr)

		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected transition: a matching release annotation must allow the roll despite upgradePolicy=manual")
		}
		stored, err := crdstore.Get[v1alpha1.Controller](ctx, client.store, "t", "ns")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if _, ok := stored.Annotations["varroa.dev/upgrade-release"]; ok {
			t.Error("release annotation should have been cleared in the store")
		}
		uc := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpgradePending)
		if uc == nil || uc.Status != metav1.ConditionFalse {
			t.Errorf("UpgradePending = %+v, want False", uc)
		}
	})

	t.Run("matching release annotation short-circuits an unreadable ProvisioningDefaults", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		rec := newTestReconciler(client)
		pdGVR, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
		if err != nil {
			t.Fatalf("GVRFor: %v", err)
		}
		client.store.FailAlways("get", pdGVR, errors.New("etcdserver: request timed out"))

		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		cr.Annotations = map[string]string{"varroa.dev/upgrade-release": "jenkins/jenkins:2.570.2"}
		crdstore.MustSeed(client.store, cr)

		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected transition: the release annotation match must never read ProvisioningDefaults")
		}
	})

	t.Run("release annotation clear failure holds the roll without allowing it", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
		if err != nil {
			t.Fatalf("GVRFor: %v", err)
		}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		cr.Annotations = map[string]string{"varroa.dev/upgrade-release": "jenkins/jenkins:2.570.2"}
		crdstore.MustSeed(client.store, cr)
		client.store.FailAlways("patchmeta", ctrlGVR, errors.New("etcdserver: request timed out"))

		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition: a failed annotation clear must not allow the roll")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		const wantMsg = "upgrade held: release annotation clear failed, retrying"
		if vc == nil || vc.Status != metav1.ConditionTrue || vc.Reason != "UpgradePending" || vc.Message != wantMsg {
			t.Errorf("VersionRollPending = %+v, want True/UpgradePending/%q", vc, wantMsg)
		}
	})

	t.Run("non-matching release annotation falls through to upgradePolicy: manual", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{Name: "p1"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version:        "2.570",
					ResolveVersion: "2.570.2",
				},
			},
		}
		client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
			Spec: v1alpha1.ProvisioningDefaultsSpec{UpgradePolicy: "manual"},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570"
		cr.Annotations = map[string]string{"varroa.dev/upgrade-release": "jenkins/jenkins:9.9.9"}
		crdstore.MustSeed(client.store, cr)

		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition: a non-matching release annotation must still be held by upgradePolicy=manual")
		}
		uc := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpgradePending)
		if uc == nil || uc.Status != metav1.ConditionTrue {
			t.Errorf("UpgradePending = %+v, want True", uc)
		}
	})
}

// funcSourceSHA256 extracts the exact source bytes of the named top-level
// function from file (from its doc-comment-free Pos to its End) and returns
// their sha256 hex digest.
func funcSourceSHA256(t *testing.T, file, fn string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var digest string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			return true
		}
		start := fset.Position(fd.Pos()).Offset
		end := fset.Position(fd.End()).Offset
		sum := sha256.Sum256(src[start:end])
		digest = hex.EncodeToString(sum[:])
		found = true
		return false
	})
	if !found {
		t.Fatalf("function %s not found in %s", fn, file)
	}
	return digest
}

// TestEvaluateVersionRollGateAndComputePluginRollGate_Unmodified is a golden
// hash guard: the upgradePolicy dial is composed entirely at the
// versionRollGate seam and must not touch evaluateVersionRollGate or
// computePluginRollGate. If either function's body changes, this test fails
// and the hash below must be re-derived deliberately, not silently updated.
func TestEvaluateVersionRollGateAndComputePluginRollGate_Unmodified(t *testing.T) {
	const wantEvaluateVersionRollGate = "d74522dd6531a0d3c7aa8309927023ef4ca55755acb5ef2da01dc35041337e02"
	const wantComputePluginRollGate = "6badd461f1c6e8569eb269562a8b469836c355db85f72f4f863f4c1c2c38410a"

	if got := funcSourceSHA256(t, "corecompat.go", "evaluateVersionRollGate"); got != wantEvaluateVersionRollGate {
		t.Errorf("evaluateVersionRollGate source changed (sha256 = %s, want %s) — upgradePolicy composition wraps this function and must not edit it", got, wantEvaluateVersionRollGate)
	}
	if got := funcSourceSHA256(t, "desiredstate.go", "computePluginRollGate"); got != wantComputePluginRollGate {
		t.Errorf("computePluginRollGate source changed (sha256 = %s, want %s) — upgradePolicy composition wraps this function and must not edit it", got, wantComputePluginRollGate)
	}
}
