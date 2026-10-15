package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestProvisioningItemsRefusedSetsItemsRefusedReason asserts that a refused
// items bundle blocks Provisioning under its own reason. The condition must
// not read as a bundle that still materializes.
func TestProvisioningItemsRefusedSetsItemsRefusedReason(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["items.yaml"] = plainLibraryItem("git", "missing-cred")
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	if err := rec.reconcileController(context.Background(), cr); err == nil {
		t.Fatal("expected provisioning to be blocked by the refused items, got nil")
	}
	if len(client.statefulSets) != 0 {
		t.Errorf("StatefulSet must not be created for refused items, got %v", client.statefulSets)
	}
	resolved := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleResolved)
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Fatalf("BundleResolved should be False, got %+v", resolved)
	}
	if resolved.Reason != v1alpha1.ReasonItemsRefused {
		t.Errorf("BundleResolved reason = %q, want %q", resolved.Reason, v1alpha1.ReasonItemsRefused)
	}
	if !strings.Contains(resolved.Message, "missing-cred") || strings.Contains(resolved.Message, "waiting for composed bundle") {
		t.Errorf("BundleResolved message = %q, want the refusal and not the wait text", resolved.Message)
	}
}

// TestProvisioningBundleNotMaterializedKeepsReason asserts that a bundle that
// is still Pending keeps the BundleNotMaterialized reason.
func TestProvisioningBundleNotMaterializedKeepsReason(t *testing.T) {
	client := newTestClient()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundlePending},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	_ = rec.reconcileController(context.Background(), cr)
	resolved := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleResolved)
	if resolved == nil || resolved.Reason != "BundleNotMaterialized" {
		t.Fatalf("BundleResolved = %+v, want reason BundleNotMaterialized", resolved)
	}
}

// TestProvisioningRestoresBundleResolvedTrue asserts that a stale False left by
// a refusal becomes True once the bundle resolves again, and that a second
// successful resolve keeps the transition time of the first.
func TestProvisioningRestoresBundleResolvedTrue(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionBundleResolved, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonItemsRefused, Message: "stale refusal"},
	}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Provisioning reconcile: %v", err)
	}
	resolved := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("BundleResolved = %+v, want True after a successful resolve", resolved)
	}
	firstTransition := resolved.LastTransitionTime

	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("second Provisioning reconcile: %v", err)
	}
	resolved = findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Fatalf("BundleResolved = %+v, want True after the second resolve", resolved)
	}
	if !resolved.LastTransitionTime.Equal(&firstTransition) {
		t.Errorf("LastTransitionTime moved from %v to %v on an unchanged True status", firstTransition, resolved.LastTransitionTime)
	}
}

// TestConnectedItemsRefusedSetsBundleFailedItemsRefused asserts that a refusal
// that appears after provisioning sets BundleFailed under the ItemsRefused
// reason, and keeps the last good desired state.
func TestConnectedItemsRefusedSetsBundleFailedItemsRefused(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Provisioning reconcile: %v", err)
	}
	client.configMapData["test-bundle-content"]["items.yaml"] = plainLibraryItem("git", "missing-cred")
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	cr.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
	}
	_ = rec.reconcileController(context.Background(), cr)

	bf := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleFailed)
	if bf == nil || bf.Status != metav1.ConditionTrue {
		t.Fatalf("BundleFailed = %+v, want True", bf)
	}
	if bf.Reason != v1alpha1.ReasonItemsRefused {
		t.Errorf("BundleFailed reason = %q, want %q", bf.Reason, v1alpha1.ReasonItemsRefused)
	}
	if !strings.Contains(bf.Message, "missing-cred") {
		t.Errorf("BundleFailed message = %q, want the refused credential ID", bf.Message)
	}
}

// TestConnectedOtherResolveFaultKeepsBundleResolutionFailed asserts that a
// resolve fault that is not an items refusal keeps its own reason.
func TestConnectedOtherResolveFaultKeepsBundleResolutionFailed(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Provisioning reconcile: %v", err)
	}
	delete(client.configMapData, "test-bundle-content")
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	cr.Status.Conditions = []v1alpha1.ControllerCondition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
	}
	_ = rec.reconcileController(context.Background(), cr)

	bf := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleFailed)
	if bf == nil || bf.Reason != "BundleResolutionFailed" {
		t.Fatalf("BundleFailed = %+v, want reason BundleResolutionFailed", bf)
	}
}

// pinnedPluginSetClient returns a client whose version profile "pinned" pins
// the given plugins.yaml body for Jenkins version 2.570. A controller on that
// version resolves its plugin set from the profile, not the embedded baseline.
func pinnedPluginSetClient(pluginsYAML string) *testClient {
	client := newTestClientWithBundle()
	client.profiles["pinned"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "pinned-content",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["pinned-content"] = map[string]string{
		"plugins.yaml": pluginsYAML,
	}
	return client
}

// TestResolveBundleItemsRefusalForEveryPluginCheck drives each plugin refusal
// through resolveBundleForController. The pinned plugin set omits the plugin
// that the items content needs, so each case must return errItemsRefused.
func TestResolveBundleItemsRefusalForEveryPluginCheck(t *testing.T) {
	cases := []struct {
		name        string
		pluginsYAML string
		items       string
		wantInMsg   string
	}{
		{
			name:        "branch build strategies",
			pluginsYAML: "plugins:\n  - artifactId: git\n    version: latest\n",
			items:       multibranchItemsYAML,
			wantInMsg:   "basic-branch-build-strategies",
		},
		{
			name:        "folder library plugins",
			pluginsYAML: "plugins:\n  - artifactId: pipeline-groovy-lib\n    version: latest\n",
			items:       folderLibraryItemsYAML,
			wantInMsg:   `"git"`,
		},
		{
			name: "gitea plugin",
			// The gitea fixture is a multibranch item, so the branch check
			// must pass first. The pinned set carries its plugin.
			pluginsYAML: "plugins:\n  - artifactId: git\n    version: latest\n  - artifactId: basic-branch-build-strategies\n    version: latest\n",
			items:       giteaSourceItemsYAML,
			wantInMsg:   "gitea",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := pinnedPluginSetClient(tc.pluginsYAML)
			client.configMapData["test-bundle-content"]["items.yaml"] = tc.items
			rec := newTestReconciler(client)

			cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
			cr.Spec.Version = "2.570"
			_, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
			if !errors.Is(err, errItemsRefused) {
				t.Fatalf("error = %v, want errItemsRefused", err)
			}
			if !strings.HasPrefix(err.Error(), "assemble items: ") {
				t.Errorf("error = %q, want the assemble items prefix", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantInMsg) {
				t.Errorf("error = %q, want it to name %q", err.Error(), tc.wantInMsg)
			}
		})
	}
}

// TestResolveBundleItemsRefusalWrapsSentinel asserts that a refused items
// bundle carries errItemsRefused, and that the message keeps its prefix.
func TestResolveBundleItemsRefusalWrapsSentinel(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["items.yaml"] = plainLibraryItem("git", "missing-cred")
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	_, _, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if !errors.Is(err, errItemsRefused) {
		t.Fatalf("error = %v, want errItemsRefused", err)
	}
	if !strings.HasPrefix(err.Error(), "assemble items: ") {
		t.Errorf("error = %q, want the assemble items prefix", err.Error())
	}
}
