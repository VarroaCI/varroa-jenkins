package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// withRebindTestMeter swaps in a test MeterProvider backed by a ManualReader and
// rebinds the package-level instruments against it, so a test exercises the REAL
// package gauges (and the real recording call sites) rather than a throwaway
// local gauge. It restores the original provider + instrument bindings on
// cleanup. Package tests run sequentially, so the global swap is safe.
func withRebindTestMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	bindControllerMetrics()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		bindControllerMetrics()
	})
	return reader
}

// TestPluginLockConflictGaugeRecording drives the REAL surfacePluginConflict /
// clearPluginConflict helpers and asserts, via a ManualReader over the rebound
// package instrument, that varroa.controller.plugin_lock_conflict records 1 on
// the conflict path and 0 on the clear path with the controller's attributes.
// This validates production's instrumentation wiring, not just the OTel SDK.
func TestPluginLockConflictGaugeRecording(t *testing.T) {
	reader := withRebindTestMeter(t)
	ctx := context.Background()

	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("ctrl-pc", "ns-pc", v1alpha1.ControllerPhaseProvisioning)

	// Real conflict path.
	rec.surfacePluginConflict(ctx, cr, "plugin foo pinned 1.2 but the version-profile lock requires 1.3")
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after conflict: %v", err)
	}
	if !findGaugeValue(t, rm, "varroa.controller.plugin_lock_conflict", "ns-pc", "ctrl-pc", 1) {
		t.Error("expected plugin_lock_conflict gauge = 1 after surfacePluginConflict, not found")
	}

	// Real clear path (same series overwritten with 0).
	rec.clearPluginConflict(ctx, cr)
	rm = metricdata.ResourceMetrics{}
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after clear: %v", err)
	}
	if !findGaugeValue(t, rm, "varroa.controller.plugin_lock_conflict", "ns-pc", "ctrl-pc", 0) {
		t.Error("expected plugin_lock_conflict gauge = 0 after clearPluginConflict, not found")
	}
}

// TestReconcileBlockedGaugeRecording drives the REAL markReconcileBlocked helper
// and asserts varroa.controller.reconcile.blocked records 1 with the
// controller's attributes. This is the reconcileBlockedGauge's only direct
// coverage.
func TestReconcileBlockedGaugeRecording(t *testing.T) {
	reader := withRebindTestMeter(t)
	ctx := context.Background()

	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("ctrl-rb", "ns-rb", v1alpha1.ControllerPhasePending)

	rec.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, "bundle content configmap missing")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after markReconcileBlocked: %v", err)
	}
	if !findGaugeValue(t, rm, "varroa.controller.reconcile.blocked", "ns-rb", "ctrl-rb", 1) {
		t.Error("expected reconcile.blocked gauge = 1 after markReconcileBlocked, not found")
	}
}

// TestMiteImageStaleGaugeRecording drives the REAL refreshMiteImageStaleness
// helper and asserts varroa.controller.mite_image_stale records 1 when the
// running mite image differs from the operator-desired image and 0 when they
// match, with the controller's attributes.
func TestMiteImageStaleGaugeRecording(t *testing.T) {
	reader := withRebindTestMeter(t)
	ctx := context.Background()

	// Running mite image v1; class-desired mite image v2 → stale.
	client := newTestClient()
	client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
	client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
	client.stsMiteFound = true
	client.stsMitePullPolicy = "IfNotPresent"
	client.controllerClass = &v1alpha1.ControllerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
		Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
	}
	rec := newTestReconciler(client)
	cr := testController("ctrl-mis", "ns-mis", v1alpha1.ControllerPhaseConnected)
	cr.Spec.ClassName = "test-class"

	rec.refreshMiteImageStaleness(ctx, cr)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after stale: %v", err)
	}
	if !findGaugeValue(t, rm, "varroa.controller.mite_image_stale", "ns-mis", "ctrl-mis", 1) {
		t.Error("expected mite_image_stale gauge = 1 when running != desired, not found")
	}

	// Converge the desired image to the running one → current (0).
	client.controllerClass.Spec.Mite.Image = "ghcr.io/varroaci/varroa-jenkins:v1"
	crdstore.MustSeed(client.store, client.controllerClass)
	rec.refreshMiteImageStaleness(ctx, cr)
	rm = metricdata.ResourceMetrics{}
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after current: %v", err)
	}
	if !findGaugeValue(t, rm, "varroa.controller.mite_image_stale", "ns-mis", "ctrl-mis", 0) {
		t.Error("expected mite_image_stale gauge = 0 when running == desired, not found")
	}
}

func findGaugeValue(t *testing.T, rm metricdata.ResourceMetrics, metricName, ns, ctrl string, want int64) bool {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "varroa-operator" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			gd, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("expected Int64Gauge data for %q, got %T", metricName, m.Data)
			}
			for _, dp := range gd.DataPoints {
				dpNS, _ := dp.Attributes.Value("namespace")
				dpCtrl, _ := dp.Attributes.Value("controller")
				if dpNS.AsString() == ns && dpCtrl.AsString() == ctrl && dp.Value == want {
					return true
				}
			}
		}
	}
	return false
}
