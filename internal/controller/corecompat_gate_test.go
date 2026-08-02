package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// baselineVersions derives an unsafe (one minor below baseline) and safe (well
// above baseline) version from the embedded plugin-lock baseline so the tests
// never hardcode the shipped baseline value.
func baselineVersions(t *testing.T) (unsafe, safe string) {
	t.Helper()
	baseline := pluginlock.Baseline()
	segs, ok := jenkinsver.Core(baseline)
	if !ok || len(segs) < 2 {
		t.Fatalf("unexpected baseline %q", baseline)
	}
	unsafe = fmt.Sprintf("%d.%d", segs[0], segs[1]-1)
	safe = fmt.Sprintf("%d.%d.1", segs[0], segs[1]+9)
	return unsafe, safe
}

// --- Task 4.2: bake-time gate in handleProvisioning ---

func TestProvisioning_BlocksCoreOlderThanBaseline(t *testing.T) {
	unsafe, _ := baselineVersions(t)
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Version = unsafe

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked for core older than baseline")
	}
	if _, ok := client.configMapData[testPrefix+"-plugins"]; ok {
		t.Fatal("plugins ConfigMap must not be written when the version gate denies")
	}
	if len(client.statefulSets) != 0 {
		t.Fatalf("StatefulSet must not be created when the version gate denies, got %v", client.statefulSets)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected VersionUpgradeBlocked=True, got %+v", cond)
	}
	if cond.Reason != v1alpha1.ReasonCoreOlderThanPluginBaseline {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonCoreOlderThanPluginBaseline, cond.Reason)
	}
}

func TestProvisioning_BlocksUnparseableVersion(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Version = "fancy-custom-build"

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked for unparseable version")
	}
	if _, ok := client.configMapData[testPrefix+"-plugins"]; ok {
		t.Fatal("plugins ConfigMap must not be written when the version gate denies")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected VersionUpgradeBlocked=True, got %+v", cond)
	}
	if cond.Reason != v1alpha1.ReasonUnparseableVersion {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonUnparseableVersion, cond.Reason)
	}
}

func TestProvisioning_AllowsCoreAtOrAboveBaseline(t *testing.T) {
	_, safe := baselineVersions(t)
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Version = safe

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("expected provisioning to proceed for safe version, got error: %v", err)
	}
	if _, ok := client.configMapData[testPrefix+"-plugins"]; !ok {
		t.Fatal("plugins ConfigMap should be written on the allow path")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected VersionUpgradeBlocked=False, got %+v", cond)
	}
	if cond.Reason != v1alpha1.ReasonCoreCompatible {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonCoreCompatible, cond.Reason)
	}
}

func TestProvisioning_ClearsStaleBlockWhenReadyProfileAppears(t *testing.T) {
	unsafe, _ := baselineVersions(t)
	client := newTestClientWithBundle()
	// A ready exact profile now vouches for the previously-unsafe version.
	client.profiles["vouch"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "vouch"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: unsafe},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "vouch-content",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["vouch-content"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: git\n    version: latest\n",
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Version = unsafe
	// Pre-seed a stale block from a prior pass.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionVersionUpgradeBlocked,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonCoreOlderThanPluginBaseline,
		Message: "stale",
	})

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("expected provisioning to proceed once a ready profile vouches, got error: %v", err)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected VersionUpgradeBlocked cleared to False, got %+v", cond)
	}
	if cond.Reason != v1alpha1.ReasonCoreCompatible {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonCoreCompatible, cond.Reason)
	}
}

// --- Task 5.3: version-roll gate (A's hook) ---

func TestVersionRollGate_DeniesUnsafeAndStampsCondition(t *testing.T) {
	unsafe, _ := baselineVersions(t)
	client := newTestClient()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Version = unsafe

	allow, reason, msg := rec.evaluateVersionRollGate(context.Background(), cr,
		"jenkins/jenkins:"+unsafe, "jenkins/jenkins:"+unsafe)
	if allow {
		t.Fatal("expected roll gate to deny an unsafe core")
	}
	if reason != v1alpha1.ReasonCoreOlderThanPluginBaseline {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonCoreOlderThanPluginBaseline, reason)
	}
	if msg == "" {
		t.Error("expected a non-empty deny message")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected VersionUpgradeBlocked=True, got %+v", cond)
	}
}

func TestVersionRollGate_AllowsWhenReadyProfileVouches(t *testing.T) {
	unsafe, _ := baselineVersions(t)
	client := newTestClient()
	client.profiles["vouch"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "vouch"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: unsafe},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "vouch-content",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Version = unsafe

	allow, reason, msg := rec.evaluateVersionRollGate(context.Background(), cr, "a", "b")
	if !allow {
		t.Fatalf("expected roll gate to allow when a ready profile vouches; reason=%s msg=%s", reason, msg)
	}
	if reason != "" || msg != "" {
		t.Errorf("allow path must return empty reason/message, got %q/%q", reason, msg)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionUpgradeBlocked)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected VersionUpgradeBlocked=False, got %+v", cond)
	}
}

func TestVersionRollGate_Idempotent(t *testing.T) {
	unsafe, _ := baselineVersions(t)
	client := newTestClient()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Version = unsafe

	a1, r1, m1 := rec.evaluateVersionRollGate(context.Background(), cr, "x", "y")
	a2, r2, m2 := rec.evaluateVersionRollGate(context.Background(), cr, "x", "y")
	if a1 != a2 || r1 != r2 || m1 != m2 {
		t.Errorf("gate not idempotent: (%v,%q,%q) != (%v,%q,%q)", a1, r1, m1, a2, r2, m2)
	}
}
