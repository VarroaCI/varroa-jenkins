package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

// fakeTenancyClient implements tenancy.NamespaceClient backed by a simple
// map for testing the operator gate.
type fakeTenancyClient struct {
	store map[string]bool // name → exists
}

func (f *fakeTenancyClient) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	if f.store[name] {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func (f *fakeTenancyClient) CreateNamespace(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (f *fakeTenancyClient) PatchNamespaceLabels(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func TestHandlePendingNamespaceGate(t *testing.T) {
	ctx := context.Background()

	t.Run("unmanaged namespace holds in Pending with Degraded", func(t *testing.T) {
		fc := &fakeTenancyClient{store: map[string]bool{"ns-a": true}}
		set := tenancy.NewManagedSet("ns-b", "") // scoped: only ns-b is managed
		c := tenancy.NewClassifier(fc, set)
		r := &Reconciler{tenancy: c}

		cr := &v1alpha1.Controller{}
		cr.Namespace = "ns-a"
		cr.Status.Phase = v1alpha1.ControllerPhasePending

		err := r.handlePending(ctx, cr)
		if err != nil {
			t.Fatalf("handlePending: %v", err)
		}
		// Phase unchanged (gate returned nil, did not advance).
		if cr.Status.Phase != v1alpha1.ControllerPhasePending {
			t.Errorf("expected phase Pending (not advanced), got %q", cr.Status.Phase)
		}
		d := findCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded)
		if d == nil {
			t.Fatal("expected ConditionDegraded to be set")
		}
		if d.Status != metav1.ConditionTrue {
			t.Errorf("expected Degraded=True, got %v", d.Status)
		}
		if d.Reason != v1alpha1.ReasonTargetNamespaceUnmanaged {
			t.Errorf("expected reason %q, got %q", v1alpha1.ReasonTargetNamespaceUnmanaged, d.Reason)
		}
	})

	t.Run("now-managed namespace clears Degraded", func(t *testing.T) {
		fc := &fakeTenancyClient{store: map[string]bool{"ns-a": true}}
		set := tenancy.NewManagedSet("ns-a", "") // now ns-a IS managed
		c := tenancy.NewClassifier(fc, set)
		tc := newTestClient()
		r := &Reconciler{tenancy: c, client: tc, store: tc.store}

		cr := &v1alpha1.Controller{}
		cr.Namespace = "ns-a"
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		// Set a prior Degraded condition from the gate.
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{
			Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonTargetNamespaceUnmanaged,
		}}
		// Set a bundle ref so handlePending doesn't fail at the bundle check.
		cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "test-bundle"}

		// handlePending will proceed past the gate and fail on bundle resolution,
		// but the condition should be cleared before that error.
		err := r.handlePending(ctx, cr)
		// We expect an error from bundle resolution, not from the tenancy gate.
		if err == nil {
			t.Fatal("expected error from bundle resolution (gate passed)")
		}
		d := findCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded)
		if d == nil {
			t.Fatal("expected ConditionDegraded to exist")
		}
		if d.Status != metav1.ConditionFalse {
			t.Errorf("expected Degraded=False after namespace becomes managed, got %v", d.Status)
		}
		if d.Reason != v1alpha1.ReasonTargetNamespaceReady {
			t.Errorf("expected reason %q, got %q", v1alpha1.ReasonTargetNamespaceReady, d.Reason)
		}
	})

	t.Run("pre-existing Degraded with different reason is left untouched", func(t *testing.T) {
		fc := &fakeTenancyClient{store: map[string]bool{"ns-a": true}}
		set := tenancy.NewManagedSet("ns-a", "")
		c := tenancy.NewClassifier(fc, set)
		tc := newTestClient()
		r := &Reconciler{tenancy: c, client: tc, store: tc.store}

		cr := &v1alpha1.Controller{}
		cr.Namespace = "ns-a"
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		// A Degraded condition set by another subsystem with a different reason.
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{
			Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
			Reason: "SomeOtherReason",
		}}
		cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "test-bundle"}

		err := r.handlePending(ctx, cr)
		if err == nil {
			t.Fatal("expected error from bundle resolution (gate passed)")
		}
		d := findCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded)
		if d == nil {
			t.Fatal("expected ConditionDegraded to exist")
		}
		// Should still be True with the original reason — not cleared by this gate.
		if d.Status != metav1.ConditionTrue {
			t.Errorf("expected Degraded=True (left untouched), got %v", d.Status)
		}
		if d.Reason != "SomeOtherReason" {
			t.Errorf("expected reason to still be 'SomeOtherReason', got %q", d.Reason)
		}
	})

	t.Run("nil tenancy skips the gate", func(t *testing.T) {
		tc := newTestClient()
		r := &Reconciler{tenancy: nil, client: tc, store: tc.store, operatorNamespace: "varroa-system"}
		cr := &v1alpha1.Controller{}
		cr.Namespace = "ns-a"
		cr.Status.Phase = v1alpha1.ControllerPhasePending

		// Without a tenancy classifier, handlePending proceeds to the bundle
		// lookup, where a nil ComposedBundleRef resolves to the seeded starter
		// bundle and succeeds. The gate being skipped is what we assert; a
		// tenancy rejection would surface as Degraded/TargetNamespaceUnmanaged.
		if err := r.handlePending(ctx, cr); err != nil {
			t.Fatalf("expected the gate to be skipped and the starter bundle to resolve, got: %v", err)
		}
		if d := findCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded); d != nil &&
			d.Reason == v1alpha1.ReasonTargetNamespaceUnmanaged {
			t.Fatal("tenancy gate ran despite a nil classifier")
		}
	})
}
