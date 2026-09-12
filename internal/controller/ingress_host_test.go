package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// A Controller with neither spec.ingressSpec.host nor a rootDomain gets no
// Ingress. That is supported (port-forward); the reconciler must surface a
// NoResolvedHost condition, not stay silent — silence is indistinguishable
// from a broken ingress controller.
func TestReconcileIngressReportsNoResolvedHost(t *testing.T) {
	tc := newTestClient()
	rec := newTestReconciler(tc)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	if err := rec.reconcileIngress(context.Background(), cr); err != nil {
		t.Fatalf("a controller with no host must not be an error: %v", err)
	}

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionNoExternalURL)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatal("expected NoExternalURL=True when no host resolves")
	}
	// The message has to tell the reader what to do; "no host" alone sends
	// them to the ingress controller logs for a non-problem.
	if !strings.Contains(c.Message, "port-forward") || !strings.Contains(c.Message, "rootDomain") {
		t.Errorf("message should name both remedies, got: %s", c.Message)
	}
}

// Setting a host later must clear the condition, not leave a stale True.
func TestReconcileIngressClearsNoExternalURLOnceHostResolves(t *testing.T) {
	ctx := context.Background()
	tc := newTestClient()
	rec := newTestReconciler(tc)

	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	if err := rec.reconcileIngress(ctx, cr); err != nil {
		t.Fatalf("first reconcileIngress: %v", err)
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionNoExternalURL); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Fatal("precondition: expected NoExternalURL=True")
	}

	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "test.example.com"}
	if err := rec.reconcileIngress(ctx, cr); err != nil {
		t.Fatalf("second reconcileIngress: %v", err)
	}

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionNoExternalURL)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatal("expected NoExternalURL to be cleared once a host resolves")
	}
	if !strings.Contains(c.Message, "test.example.com") {
		t.Errorf("cleared message should name the host, got: %s", c.Message)
	}
}

// A cell gets no per-cell Ingress, even when a ProvisioningDefaults
// rootDomain would otherwise resolve a host (design D1).
func TestReconcileIngressSkipsCell(t *testing.T) {
	tc := newTestClient()
	tc.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{RootDomain: "example.com"},
	}
	rec := newTestReconciler(tc)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	}

	if err := rec.reconcileIngress(context.Background(), cr); err != nil {
		t.Fatalf("reconcileIngress on a cell must not error: %v", err)
	}
	if len(tc.ingressCalls) != 0 {
		t.Errorf("expected no Ingress created for a cell, got %+v", tc.ingressCalls)
	}
}

// A one-tenant controller that opts into spec.headless gets no Ingress even
// when a host resolves, an Ingress an earlier reconcile created is deleted,
// and the NoExternalURL condition carries reason Headless (design D4).
func TestReconcileIngressHeadlessDeletesAndSetsCondition(t *testing.T) {
	tc := newTestClient()
	tc.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{RootDomain: "example.com"},
	}
	rec := newTestReconciler(tc)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Headless: true},
	}

	if err := rec.reconcileIngress(context.Background(), cr); err != nil {
		t.Fatalf("reconcileIngress on a headless controller must not error: %v", err)
	}
	if len(tc.ingressCalls) != 0 {
		t.Errorf("expected no Ingress created for a headless controller, got %+v", tc.ingressCalls)
	}

	wantName := controllerPrefix(cr) + "-ingress"
	found := false
	for _, d := range tc.deleted {
		if d.kind == "Ingress" && d.name == wantName {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the Ingress %q to be deleted, got %+v", wantName, tc.deleted)
	}

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionNoExternalURL)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Headless" {
		t.Fatalf("expected NoExternalURL=True with reason Headless, got %+v", c)
	}
}

// A non-NotFound delete failure must reach the caller so the reconcile
// retries. A silently-discarded error would report success while the old
// Ingress still routes in.
func TestReconcileIngressHeadlessReturnsDeleteError(t *testing.T) {
	tc := newTestClient()
	tc.deleteResourceErr = errors.New("delete rejected")
	rec := newTestReconciler(tc)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Headless: true},
	}

	err := rec.reconcileIngress(context.Background(), cr)
	if err == nil {
		t.Fatal("expected reconcileIngress to return the delete error, got nil")
	}
	if !strings.Contains(err.Error(), "delete rejected") {
		t.Errorf("expected the error to wrap the delete failure, got: %v", err)
	}
}
