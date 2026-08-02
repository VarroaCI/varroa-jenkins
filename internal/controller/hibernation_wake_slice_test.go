package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestOperatorPodIPsErrorBackoff(t *testing.T) {
	t.Setenv("POD_IP", "")
	ctx := context.Background()

	// No prior cache + list error surfaces the error to the caller.
	client := newTestClient()
	client.wakePodErr = errors.New("boom")
	rec := newTestReconciler(client)
	if _, err := rec.operatorPodIPs(ctx); err == nil {
		t.Fatal("expected error with no cached IPs")
	}

	// Seed a good cache, then fail the next list after forcing TTL expiry:
	// the last-known-good IPs are served through the transient error.
	client.wakePodErr = nil
	client.wakePodIPs = []string{"10.0.0.5"}
	rec.wakePodMu.Lock()
	rec.wakePodFetchedAt = time.Time{}
	rec.wakePodMu.Unlock()
	if ips, err := rec.operatorPodIPs(ctx); err != nil || len(ips) != 1 || ips[0] != "10.0.0.5" {
		t.Fatalf("seed fetch = %v %v", ips, err)
	}
	client.wakePodErr = errors.New("boom")
	rec.wakePodMu.Lock()
	rec.wakePodFetchedAt = time.Time{}
	rec.wakePodMu.Unlock()
	if ips, err := rec.operatorPodIPs(ctx); err != nil || len(ips) != 1 || ips[0] != "10.0.0.5" {
		t.Fatalf("expected last-known-good on error, got %v %v", ips, err)
	}
}

func TestHibernatedReconcileEnsuresWakeSlice(t *testing.T) {
	t.Setenv("POD_IP", "")
	client := newTestClient()
	client.wakePodIPs = []string{"10.0.0.2", "10.0.0.1"}
	rec := newTestReconciler(client)
	rec.SetWakeServerPort(8082, true)
	cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
	cr.Spec.PowerState = "Hibernated"

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile hibernated controller: %v", err)
	}
	if len(client.wakeEnsureCalls) != 1 {
		t.Fatalf("EnsureWakeEndpointSlice calls = %d, want 1", len(client.wakeEnsureCalls))
	}
	call := client.wakeEnsureCalls[0]
	if call.service != testPrefix+"-svc" || call.port != 8082 {
		t.Fatalf("unexpected ensure call: %+v", call)
	}
	if got, want := len(call.ips), 2; got != want {
		t.Fatalf("ensure IP count = %d, want %d", got, want)
	}
}

func TestHibernatedWakeSliceSkipsWhenDisabledOrNoIPs(t *testing.T) {
	t.Setenv("POD_IP", "")
	for _, enabled := range []bool{false, true} {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.SetWakeServerPort(8082, enabled)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseHibernated)
		cr.Spec.PowerState = "Hibernated"
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("reconcile enabled=%t: %v", enabled, err)
		}
		if len(client.wakeEnsureCalls) != 0 {
			t.Fatalf("EnsureWakeEndpointSlice calls with enabled=%t and no IPs = %d, want 0", enabled, len(client.wakeEnsureCalls))
		}
	}
}

func TestDisabledWakeDeletesSliceFromHibernatedController(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetWakeServerPort(8082, false)
	cr := testController("test", "ns", v1alpha1.ControllerPhaseHibernated)
	cr.Spec.PowerState = "Hibernated"

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile hibernated controller: %v", err)
	}
	if len(client.wakeEnsureCalls) != 0 || len(client.wakeDeleteCalls) != 1 {
		t.Fatalf("ensure calls=%d delete calls=%d, want 0/1", len(client.wakeEnsureCalls), len(client.wakeDeleteCalls))
	}
}

func TestDisabledWakeStillRunsDeletePaths(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetWakeServerPort(8082, false)
	cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)

	rec.deleteWakeSlice(context.Background(), cr, rec.Logger)
	rec.deleteWakeSlice(context.Background(), cr, rec.Logger)
	if len(client.wakeDeleteCalls) != 1 {
		t.Fatalf("delete calls=%d, want one confirmed delete with memo", len(client.wakeDeleteCalls))
	}
}

func TestProvisioningTimeoutAfterWakeDeletesSlice(t *testing.T) {
	t.Setenv("POD_IP", "")
	client := newTestClientWithBundle()
	client.wakePodIPs = []string{"10.0.0.1"}
	rec := newTestReconciler(client)
	rec.SetWakeServerPort(8082, true)
	cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
	cr.Spec.PowerState = "Hibernated"

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("hibernate reconcile: %v", err)
	}
	if len(client.wakeEnsureCalls) != 1 {
		t.Fatalf("ensure calls=%d, want 1", len(client.wakeEnsureCalls))
	}

	cr.Spec.PowerState = "Running"
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("leave hibernated reconcile: %v", err)
	}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("pending reconcile: %v", err)
	}
	expired := metav1.NewTime(time.Now().Add(-provisioningTimeout - time.Minute))
	cr.Status.ProvisioningStartedAt = &expired
	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning timeout")
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseFailed || len(client.wakeDeleteCalls) != 1 {
		t.Fatalf("phase=%s delete calls=%d, want Failed/1", cr.Status.Phase, len(client.wakeDeleteCalls))
	}
}

func TestWakeSliceDeleteMemoAndRetry(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetWakeServerPort(8082, true)
	cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
	ctx := context.Background()

	client.wakeDeleteErr = errors.New("transient")
	rec.deleteWakeSlice(ctx, cr, rec.Logger)
	rec.deleteWakeSlice(ctx, cr, rec.Logger)
	if got := len(client.wakeDeleteCalls); got != 2 {
		t.Fatalf("failed delete calls = %d, want 2 retries", got)
	}

	client.wakeDeleteErr = nil
	rec.deleteWakeSlice(ctx, cr, rec.Logger)
	rec.deleteWakeSlice(ctx, cr, rec.Logger)
	if got := len(client.wakeDeleteCalls); got != 3 {
		t.Fatalf("successful delete calls = %d, want memoized 3", got)
	}
}

func TestWakeSliceLifecycleDeletes(t *testing.T) {
	ctx := context.Background()

	t.Run("provisioning ready", func(t *testing.T) {
		client := newTestClientWithBundle()
		ready := true
		client.statefulSetReady = &ready
		rec := newTestReconciler(client)
		rec.SetWakeServerPort(8082, true)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.handleProvisioning(ctx, cr); err != nil {
			t.Fatalf("handle provisioning: %v", err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseRunning || len(client.wakeDeleteCalls) != 1 {
			t.Fatalf("phase=%s delete calls=%d, want Running/1", cr.Status.Phase, len(client.wakeDeleteCalls))
		}
	})

	t.Run("stopped", func(t *testing.T) {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.SetWakeServerPort(8082, true)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseHibernated)
		cr.Spec.PowerState = "Stopped"
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("reconcile stopped: %v", err)
		}
		if len(client.wakeDeleteCalls) != 1 {
			t.Fatalf("delete calls=%d, want 1", len(client.wakeDeleteCalls))
		}
	})

	t.Run("running healing sweep", func(t *testing.T) {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.SetWakeServerPort(8082, true)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		rec.deleteWakeSlice(ctx, cr, rec.Logger)
		rec.deleteWakeSlice(ctx, cr, rec.Logger)
		if len(client.wakeDeleteCalls) != 1 {
			t.Fatalf("healing delete calls=%d, want 1", len(client.wakeDeleteCalls))
		}
	})

	t.Run("finalize", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		rec.SetWakeServerPort(8082, true)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		if err := rec.Finalize(ctx, cr); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if len(client.wakeDeleteCalls) != 1 {
			t.Fatalf("delete calls=%d, want 1", len(client.wakeDeleteCalls))
		}
	})
}
