package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// A Controller with neither spec.ingressSpec.host nor a rootDomain gets no
// Ingress. That is supported (port-forward), but it used to be completely
// silent, which is indistinguishable from a broken ingress controller.
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
