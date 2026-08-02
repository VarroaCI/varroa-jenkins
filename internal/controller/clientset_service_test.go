package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestCreateServicePorts verifies the controller Service exposes both the HTTP
// port and the inbound agent port (#315). The jenkins/jenkins image fixes the
// TCP agent listener at 50000 (JENKINS_SLAVE_AGENT_PORT) and the StatefulSet
// declares the matching containerPort; without the Service port, kubernetes
// plugin agents in TCP inbound mode can never connect back. Multi-port
// Services also require every port to be named.
func TestCreateServicePorts(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	if err := c.CreateService(context.Background(), "ci-svc", "team-a", 8080, ""); err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	svc, err := c.clientset.CoreV1().Services("team-a").Get(context.Background(), "ci-svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %v, want http + agent", svc.Spec.Ports)
	}
	byName := map[string]corev1.ServicePort{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p
	}
	http, ok := byName["http"]
	if !ok || http.Port != 8080 || http.TargetPort.IntValue() != 8080 {
		t.Errorf("http port = %+v, want 8080->8080", byName["http"])
	}
	agent, ok := byName["agent"]
	if !ok || agent.Port != 50000 || agent.TargetPort.IntValue() != 50000 {
		t.Errorf("agent port = %+v, want 50000->50000", byName["agent"])
	}
}

// TestCreateServiceUpdatesExisting verifies CreateService reconciles an already
// existing Service instead of treating it as an idempotent no-op (#315, same
// trap as #88 for Ingress): a pre-fix Service with a single unnamed port must
// converge to the named http+agent ports while preserving the allocated
// ClusterIP (immutable) and metadata owned out of band.
func TestCreateServiceUpdatesExisting(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	// Seed a pre-fix Service: single unnamed port, allocated ClusterIP,
	// ownerReference set out of band.
	pre := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ci-svc",
			Namespace:       "team-a",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "ci"}},
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.42",
			Ports: []corev1.ServicePort{{
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
			}},
			Selector: map[string]string{"app": "ci"},
		},
	}
	if _, err := c.clientset.CoreV1().Services("team-a").Create(ctx, pre, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	if err := c.CreateService(ctx, "ci-svc", "team-a", 8080, ""); err != nil {
		t.Fatalf("reconcile CreateService: %v", err)
	}

	got, err := c.clientset.CoreV1().Services("team-a").Get(ctx, "ci-svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service after reconcile: %v", err)
	}
	names := map[string]bool{}
	for _, p := range got.Spec.Ports {
		names[p.Name] = true
	}
	if len(got.Spec.Ports) != 2 || !names["http"] || !names["agent"] {
		t.Errorf("ports = %v, want named http + agent", got.Spec.Ports)
	}
	if got.Spec.ClusterIP != "10.96.0.42" {
		t.Errorf("ClusterIP = %q, want preserved 10.96.0.42", got.Spec.ClusterIP)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "ci" {
		t.Errorf("ownerReferences clobbered: %v", got.OwnerReferences)
	}
}

// TestCreateServiceNoOpWhenConverged verifies the reconcile path skips the
// Update call when the live Service already matches the owned fields (ports,
// selector, type, managed annotations). reconcileService runs on every
// Running/Connected tick fleet-wide, so a converged Service must not generate
// steady-state write traffic.
func TestCreateServiceNoOpWhenConverged(t *testing.T) {
	fc := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: fc}
	ctx := context.Background()

	if err := c.CreateService(ctx, "ci-svc", "team-a", 8080, ""); err != nil {
		t.Fatalf("initial CreateService: %v", err)
	}

	fc.ClearActions()
	if err := c.CreateService(ctx, "ci-svc", "team-a", 8080, ""); err != nil {
		t.Fatalf("reconcile CreateService: %v", err)
	}
	for _, a := range fc.Actions() {
		if a.GetVerb() == "update" {
			t.Errorf("converged Service must not be updated, got action %v", a)
		}
	}
}

// TestServiceReconciledPostProvisioning verifies the Service is re-derived on
// the Running/Connected tick, not only during provisioning (#315). Without
// this, Services created by pre-fix operators never converge to expose the
// agent port.
func TestServiceReconciledPostProvisioning(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseRunning)
	// Service convergence is non-fatal and runs before the phase handler, so
	// the reconcile outcome itself is irrelevant here.
	_ = rec.reconcileController(context.Background(), cr)

	if len(client.services) == 0 {
		t.Fatal("Service should be reconciled on the Running tick")
	}
}
