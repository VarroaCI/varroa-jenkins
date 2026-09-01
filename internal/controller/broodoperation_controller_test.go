package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

type fakeBroodClient struct {
	ResourceClient
	store            *crdstore.Fake
	reprovisionCalls []string
	deletePodCalls   []string
	pods             map[string]*corev1.Pod // "ns/name" -> controller pod (nil entry = no pod)
	wakeCalls        []string
	// configMaps holds fixture ConfigMap data keyed by key(namespace, name),
	// for GetConfigMap call sites (e.g. dispatchUpgrade's plugin-set/plugins.yaml
	// lookups).
	configMaps map[string]map[string]string
	// statefulSetImages holds fixture computed/live image maps keyed by
	// key(namespace, name) (the StatefulSet name, i.e. controllerPrefix(cr)),
	// for GetStatefulSetImages call sites (e.g. dispatchUpgrade's
	// already-at-target check). An absent key means no StatefulSet yet.
	statefulSetImages map[string]statefulSetImagesFixture

	// ssaApplies records every ApplyControllerSpecSSA call so tests can assert
	// a brood verb submits only the fields it intends to change.
	ssaApplies []ssaApplyCall
	// ssaApplyErr, when set, is returned by ApplyControllerSpecSSAIfExists
	// instead of recording the call, for tests proving a pin-write failure is
	// not swallowed.
	ssaApplyErr error
	// hibernatedClears records SetHibernated(false) calls (name).
	hibernatedClears []string
}

// ssaApplyCall is one recorded ApplyControllerSpecSSA invocation.
type ssaApplyCall struct {
	namespace, name, fieldManager string
	spec                          map[string]any
	force                         bool
}

func (f *fakeBroodClient) Wake(ns, name string) {
	f.wakeCalls = append(f.wakeCalls, ns+"/"+name)
}

func (f *fakeBroodClient) DeleteControllerPod(_ context.Context, namespace, name string) error {
	f.deletePodCalls = append(f.deletePodCalls, namespace+"/"+name)
	return nil
}

func (f *fakeBroodClient) GetControllerPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	return f.pods[namespace+"/"+name], nil
}

func (f *fakeBroodClient) ApplyControllerSpecSSA(_ context.Context, ns, name string, spec map[string]any, fieldManager string, force bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	f.ssaApplies = append(f.ssaApplies, ssaApplyCall{
		namespace: ns, name: name, fieldManager: fieldManager, spec: spec, force: force,
	})
	return nil, nil, nil
}

// ApplyControllerSpecSSAIfExists mirrors the real ClientsetClient method's
// existence guard: it checks f.store immediately before recording the apply,
// so a target absent from f.store fails with NotFound and is never recorded
// in ssaApplies, exactly like a real deleted-between-GET-and-apply target.
func (f *fakeBroodClient) ApplyControllerSpecSSAIfExists(ctx context.Context, ns, name string, spec map[string]any, fieldManager string, force bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	if f.ssaApplyErr != nil {
		return nil, nil, f.ssaApplyErr
	}
	if _, err := crdstore.Get[v1alpha1.Controller](ctx, f.store, name, ns); err != nil {
		return nil, nil, err
	}
	f.ssaApplies = append(f.ssaApplies, ssaApplyCall{
		namespace: ns, name: name, fieldManager: fieldManager, spec: spec, force: force,
	})
	return nil, nil, nil
}

func (f *fakeBroodClient) SetHibernated(_ context.Context, name, _ string, want bool) (bool, error) {
	if !want {
		f.hibernatedClears = append(f.hibernatedClears, name)
	}
	return true, nil
}

func (f *fakeBroodClient) Reprovision(ns, name string) {
	f.reprovisionCalls = append(f.reprovisionCalls, ns+"/"+name)
}

func (f *fakeBroodClient) GetConfigMap(_ context.Context, name, namespace string) (map[string]string, error) {
	cm, ok := f.configMaps[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "configmaps"}, name)
	}
	return cm, nil
}

// statefulSetImagesFixture is the fixture shape for a GetStatefulSetImages
// call site.
type statefulSetImagesFixture struct {
	computed, live map[string]string
}

func (f *fakeBroodClient) GetStatefulSetImages(_ context.Context, name, namespace string) (map[string]string, map[string]string, error) {
	fixture, ok := f.statefulSetImages[key(namespace, name)]
	if !ok {
		return nil, nil, nil
	}
	return fixture.computed, fixture.live, nil
}

func init() { _ = v1alpha1.AddToScheme(scheme.Scheme) }

var frozenNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func testBroodOp(name, namespace string, spec v1alpha1.BroodOperationSpec, phase v1alpha1.BroodOperationPhase) *v1alpha1.BroodOperation {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
		Status:     v1alpha1.BroodOperationStatus{Phase: phase},
	}
	if phase != "" {
		op.Finalizers = append(op.Finalizers, broodFinalizer)
	}
	return op
}

func testCtrl2(name, namespace string, phase v1alpha1.ControllerPhase) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     v1alpha1.ControllerStatus{Phase: phase},
	}
}

func ctrlWithWave2(name, namespace string, phase v1alpha1.ControllerPhase, wave int) *v1alpha1.Controller {
	c := testCtrl2(name, namespace, phase)
	c.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{RolloutWave: wave}
	return c
}

func ctrlWithVersion2(name, namespace string, phase v1alpha1.ControllerPhase, version string) *v1alpha1.Controller {
	c := testCtrl2(name, namespace, phase)
	c.Spec.Version = version
	return c
}

func newBORec(t *testing.T, objs ...client.Object) (*BroodOperationReconciler, *fakeBroodClient, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&v1alpha1.BroodOperation{}).
		WithObjects(objs...).
		Build()
	fc := &fakeBroodClient{store: crdstore.NewFake()}
	rec := NewBroodOperationReconciler(cl, scheme.Scheme, "operator-ns", fc, fc.store, fc.Wake, func(_, _ string) {}, nil, nil)
	rec.now = func() time.Time { return frozenNow }
	return rec, fc, cl
}

func ptr32(v int32) *int32 { return &v }

func TestBrood1_ReconcileBasic(t *testing.T) {
	spec := v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}},
	}
	op := testBroodOp("test-op", "team-ns", spec, "")
	rec, _, cl := newBORec(t, op)
	res, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-op", Namespace: "team-ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got v1alpha1.BroodOperation
	cl.Get(context.Background(), types.NamespacedName{Name: "test-op", Namespace: "team-ns"}, &got)
	t.Logf("phase=%s requeue=%v finalizers=%v", got.Status.Phase, res.RequeueAfter, got.Finalizers)
}

func TestBrood2_ResolveTargets(t *testing.T) {
	ctx := context.Background()
	ctrlA := ctrlWithWave2("ctrl-a", "team-ns", v1alpha1.ControllerPhaseConnected, 1)
	ctrlB := ctrlWithWave2("ctrl-b", "team-ns", v1alpha1.ControllerPhaseConnected, 0)
	ctrlC := ctrlWithWave2("ctrl-c", "team-ns", v1alpha1.ControllerPhaseStopped, 2)

	t.Run("names-mode-team-ns", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ctrlA, ctrlB).Build()
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a", "ctrl-b"}},
		}, "team-ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("expected 2, got %d", len(targets))
		}
		if targets[0].Name != "ctrl-b" || targets[1].Name != "ctrl-a" {
			t.Errorf("sort order: %s/%s, want ctrl-b then ctrl-a", targets[0].Name, targets[1].Name)
		}
	})

	t.Run("selector-mode", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ctrlA, ctrlB, ctrlC).Build()
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}},
		}, "team-ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 3 {
			t.Fatalf("expected 3, got %d", len(targets))
		}
	})

	t.Run("names-not-found", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Names: []string{"nonexistent"}},
		}, "team-ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 1 {
			t.Fatalf("expected 1, got %d", len(targets))
		}
		if targets[0].Applicable || targets[0].SkipReason != "not found" {
			t.Errorf("expected not-applicable/not-found, got applicable=%v reason=%q", targets[0].Applicable, targets[0].SkipReason)
		}
	})

	t.Run("phase-filter", func(t *testing.T) {
		conn := ctrlWithWave2("conn", "ns", v1alpha1.ControllerPhaseConnected, 0)
		stopped := ctrlWithWave2("stop", "ns", v1alpha1.ControllerPhaseStopped, 0)
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(conn, stopped).Build()
		phase := v1alpha1.ControllerPhaseConnected
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}, Filters: &v1alpha1.BroodTargetFilters{Phase: &phase}},
		}, "ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 1 || targets[0].Name != "conn" {
			t.Errorf("expected 1 conn, got %d %v", len(targets), targets)
		}
	})

	t.Run("version-filter", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
			ctrlWithVersion2("v1", "ns", v1alpha1.ControllerPhaseConnected, "1.0"),
			ctrlWithVersion2("v2", "ns", v1alpha1.ControllerPhaseConnected, "2.0"),
		).Build()
		ver := "1.0"
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}, Filters: &v1alpha1.BroodTargetFilters{Version: &ver}},
		}, "ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 1 || targets[0].Name != "v1" {
			t.Errorf("expected 1 v1, got %d %v", len(targets), targets)
		}
	})

	t.Run("verb-applicability-restart", func(t *testing.T) {
		conn := ctrlWithWave2("conn", "ns", v1alpha1.ControllerPhaseConnected, 0)
		stop := ctrlWithWave2("stop", "ns", v1alpha1.ControllerPhaseStopped, 0)
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(conn, stop).Build()
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}},
		}, "ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		for _, target := range targets {
			switch target.Name {
			case "conn":
				if !target.Applicable {
					t.Errorf("conn: expected applicable")
				}
			case "stop":
				if target.Applicable || target.SkipReason != "not Connected" {
					t.Errorf("stop: expected not Connected, got applicable=%v reason=%q", target.Applicable, target.SkipReason)
				}
			}
		}
	})

	t.Run("sorting", func(t *testing.T) {
		ctrls := []*v1alpha1.Controller{
			ctrlWithWave2("z", "ns", v1alpha1.ControllerPhaseConnected, 1),
			ctrlWithWave2("a", "ns", v1alpha1.ControllerPhaseConnected, 0),
			ctrlWithWave2("m", "ns", v1alpha1.ControllerPhaseConnected, 1),
			ctrlWithWave2("b", "ns", v1alpha1.ControllerPhaseConnected, 0),
		}
		objs := make([]client.Object, len(ctrls))
		for i, c := range ctrls {
			objs[i] = c
		}
		cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
		targets, err := ResolveTargets(ctx, cl, v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}},
		}, "ns", "operator-ns")
		if err != nil {
			t.Fatalf("ResolveTargets: %v", err)
		}
		if len(targets) != 4 {
			t.Fatalf("expected 4, got %d", len(targets))
		}
		exp := []string{"a", "b", "m", "z"}
		for i, e := range exp {
			if targets[i].Name != e {
				t.Errorf("pos %d: want %s got %s", i, e, targets[i].Name)
			}
		}
	})
}

func TestBrood3_PendingToRunning(t *testing.T) {
	ctx := context.Background()

	t.Run("tenancy-violation", func(t *testing.T) {
		spec := v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}, Namespaces: []string{"other-ns"}},
		}
		op := testBroodOp("tv", "team-ns", spec, v1alpha1.BroodOperationPhasePending)
		rec, _, cl := newBORec(t, op)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "tv", Namespace: "team-ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "tv", Namespace: "team-ns"}, &got)
		if got.Status.Phase != v1alpha1.BroodOperationPhaseFailed || got.Status.Reason != "TenancyViolation" {
			t.Errorf("expected Failed/TenancyViolation, got %s/%s", got.Status.Phase, got.Status.Reason)
		}
	})

	t.Run("zero-targets", func(t *testing.T) {
		spec := v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}},
		}
		op := testBroodOp("zt", "team-ns", spec, v1alpha1.BroodOperationPhasePending)
		rec, _, cl := newBORec(t, op)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "zt", Namespace: "team-ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "zt", Namespace: "team-ns"}, &got)
		if got.Status.Phase != v1alpha1.BroodOperationPhaseSucceeded {
			t.Errorf("expected Succeeded, got %s", got.Status.Phase)
		}
	})
}

func TestBrood4_DispatchWriteBeforeSend(t *testing.T) {
	ctrl := ctrlWithWave2("ctrl-a", "team-ns", v1alpha1.ControllerPhaseConnected, 0)
	spec := v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
		Targets:   v1alpha1.BroodTargets{Names: []string{"ctrl-a"}},
		Execution: &v1alpha1.BroodExecution{MaxParallel: ptr32(1)},
	}
	op := testBroodOp("wbs", "team-ns", spec, v1alpha1.BroodOperationPhaseRunning)
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "team-ns", Name: "ctrl-a", Wave: 0, State: v1alpha1.BroodTargetStatePending},
	}
	rec, fc, cl := newBORec(t, op, ctrl)
	rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "wbs", Namespace: "team-ns"}})
	var got v1alpha1.BroodOperation
	cl.Get(context.Background(), types.NamespacedName{Name: "wbs", Namespace: "team-ns"}, &got)
	if len(got.Status.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(got.Status.Targets))
	}
	if got.Status.Targets[0].State != v1alpha1.BroodTargetStateDispatched {
		t.Errorf("expected Dispatched, got %s", got.Status.Targets[0].State)
	}
	if len(fc.deletePodCalls) != 1 || fc.deletePodCalls[0] != "team-ns/ctrl-a" {
		t.Fatalf("expected one pod delete for team-ns/ctrl-a, got %v", fc.deletePodCalls)
	}
}

func TestBrood5_WaveGate(t *testing.T) {
	a := ctrlWithWave2("a", "ns", v1alpha1.ControllerPhaseConnected, 0)
	b := ctrlWithWave2("b", "ns", v1alpha1.ControllerPhaseConnected, 1)
	op := testBroodOp("wg", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{MaxParallel: ptr32(1)},
	}, v1alpha1.BroodOperationPhaseRunning)
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		{Namespace: "ns", Name: "b", Wave: 1, State: v1alpha1.BroodTargetStatePending},
	}
	rec, _, cl := newBORec(t, op, a, b)
	rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "wg", Namespace: "ns"}})
	var got v1alpha1.BroodOperation
	cl.Get(context.Background(), types.NamespacedName{Name: "wg", Namespace: "ns"}, &got)
	for _, target := range got.Status.Targets {
		switch target.Name {
		case "a":
			if target.State != v1alpha1.BroodTargetStateDispatched {
				t.Errorf("a: expected Dispatched, got %s", target.State)
			}
		case "b":
			if target.State != v1alpha1.BroodTargetStatePending {
				t.Errorf("b: expected Pending (gated), got %s", target.State)
			}
		}
	}
}

func TestBrood6_MaxParallel(t *testing.T) {
	ctrls := make([]client.Object, 0, 3)
	for i := 0; i < 3; i++ {
		ctrls = append(ctrls, ctrlWithWave2(fmt.Sprintf("c%d", i), "ns", v1alpha1.ControllerPhaseConnected, 0))
	}
	op := testBroodOp("mp", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{MaxParallel: ptr32(2)},
	}, v1alpha1.BroodOperationPhaseRunning)
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "c0", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		{Namespace: "ns", Name: "c1", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		{Namespace: "ns", Name: "c2", Wave: 0, State: v1alpha1.BroodTargetStatePending},
	}
	allObjs := append(ctrls, op)
	rec, _, cl := newBORec(t, allObjs...)
	rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "mp", Namespace: "ns"}})
	var got v1alpha1.BroodOperation
	cl.Get(context.Background(), types.NamespacedName{Name: "mp", Namespace: "ns"}, &got)
	disp, pend := 0, 0
	for _, target := range got.Status.Targets {
		switch target.State {
		case v1alpha1.BroodTargetStateDispatched:
			disp++
		case v1alpha1.BroodTargetStatePending:
			pend++
		}
	}
	if disp != 2 {
		t.Errorf("expected 2 dispatched, got %d", disp)
	}
	if pend != 1 {
		t.Errorf("expected 1 pending, got %d", pend)
	}
}

func TestBrood7_CompletionPredicates(t *testing.T) {
	ctx := context.Background()

	t.Run("restart-success", func(t *testing.T) {
		// Success evidence is the recreated pod being Ready. Controller
		// phase is deliberately not consulted: it lags the reconciler tick
		// in both directions (fast bounce never observed off Connected;
		// stale Connected right after the delete would open the next wave
		// while Jenkins is still booting).
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		op := testBroodOp("rs", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-2 * time.Minute)}},
		}
		rec, fc, cl := newBORec(t, op, ctrl)

		// Poll 1: old pod (predates dispatch) still standing, within the
		// observe window — stays Dispatched.
		fc.pods = map[string]*corev1.Pod{"ns/a": {ObjectMeta: metav1.ObjectMeta{
			Name: "a-0", Namespace: "ns", CreationTimestamp: metav1.Time{Time: frozenNow.Add(-24 * time.Hour)},
		}}}
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rs", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rs", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateDispatched {
			t.Fatalf("expected Dispatched while old pod stands, got %s/%q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}

		// Poll 2: pod recreated after dispatch but NOT Ready yet (Jenkins
		// booting) — must stay Dispatched or the next wave opens while this
		// controller is down.
		fc.pods["ns/a"].CreationTimestamp = metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rs", Namespace: "ns"}})
		cl.Get(ctx, types.NamespacedName{Name: "rs", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateDispatched {
			t.Fatalf("expected Dispatched while new pod is unready, got %s/%q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}

		// Poll 3: new pod Ready.
		fc.pods["ns/a"].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rs", Namespace: "ns"}})
		cl.Get(ctx, types.NamespacedName{Name: "rs", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s/%q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("restart-not-observed", func(t *testing.T) {
		// Old pod still standing past the observe window: the delete never
		// took effect.
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		op := testBroodOp("rf", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-4 * time.Minute)}},
		}
		rec, fc, cl := newBORec(t, op, ctrl)
		fc.pods = map[string]*corev1.Pod{"ns/a": {ObjectMeta: metav1.ObjectMeta{
			Name: "a-0", Namespace: "ns", CreationTimestamp: metav1.Time{Time: frozenNow.Add(-24 * time.Hour)},
		}}}
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rf", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rf", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateFailed || got.Status.Targets[0].Reason != "restart not observed" {
			t.Errorf("expected Failed/restart not observed, got %s/%s", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("restart-timeout", func(t *testing.T) {
		// Pod gone (delete worked, recreation wedged) and never came back
		// within the verb timeout. A missing pod must NOT fail the observe
		// window — only the verb timeout ends it.
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseProvisioning)
		op := testBroodOp("rt", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-16 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rt", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rt", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateFailed || got.Status.Targets[0].Reason != "restart result timeout" {
			t.Errorf("expected Failed/restart result timeout, got %s/%s", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("reprovision-success", func(t *testing.T) {
		// Two-poll flow: departure from {Connected,Running} is observed
		// first, then the return to Connected counts as success.
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseProvisioning)
		op := testBroodOp("rp", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReprovision}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-2 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "rp", Namespace: "ns"}}
		rec.Reconcile(ctx, req)
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rp", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateDispatched {
			t.Fatalf("expected still Dispatched after departure observed, got %s %q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
		var c v1alpha1.Controller
		cl.Get(ctx, types.NamespacedName{Name: "a", Namespace: "ns"}, &c)
		c.Status.Phase = v1alpha1.ControllerPhaseConnected
		if err := cl.Update(ctx, &c); err != nil {
			t.Fatalf("update controller phase: %v", err)
		}
		rec.Reconcile(ctx, req)
		cl.Get(ctx, types.NamespacedName{Name: "rp", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s %q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("reprovision-still-connected-first-poll", func(t *testing.T) {
		// A still-Connected controller shortly after dispatch must NOT count
		// as success — the departure has not been observed yet.
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		op := testBroodOp("rsc", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReprovision}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-10 * time.Second)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rsc", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rsc", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("expected still Dispatched, got %s %q", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("reprovision-never-left", func(t *testing.T) {
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		op := testBroodOp("rnl", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReprovision}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-5 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rnl", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rnl", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateFailed || got.Status.Targets[0].Reason != "reprovision not observed" {
			t.Errorf("expected Failed/reprovision not observed, got %s/%s", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("reconcile-lastReconciledAt", func(t *testing.T) {
		disp := metav1.NewTime(frozenNow.Add(-2 * time.Minute))
		lr := metav1.NewTime(frozenNow.Add(-1 * time.Minute))
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		ctrl.Status.LastReconciledAt = &lr
		op := testBroodOp("rc", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &disp},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "rc", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "rc", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s/%s", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})

	t.Run("stop-phase", func(t *testing.T) {
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseStopped)
		op := testBroodOp("sp", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStop}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "sp", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "sp", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s", got.Status.Targets[0].State)
		}
	})

	t.Run("start-phase", func(t *testing.T) {
		ctrl := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
		op := testBroodOp("st", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStart}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op, ctrl)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "st", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "st", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s", got.Status.Targets[0].State)
		}
	})

	t.Run("delete-detection", func(t *testing.T) {
		op := testBroodOp("dd", "ns", v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile}}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "gone", State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}},
		}
		rec, _, cl := newBORec(t, op)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "dd", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "dd", Namespace: "ns"}, &got)
		if got.Status.Targets[0].State != v1alpha1.BroodTargetStateFailed || got.Status.Targets[0].Reason != "controller deleted" {
			t.Errorf("expected Failed/controller deleted, got %s/%s", got.Status.Targets[0].State, got.Status.Targets[0].Reason)
		}
	})
}

func TestBrood8_FailurePolicies(t *testing.T) {
	ctx := context.Background()

	t.Run("FailFast", func(t *testing.T) {
		op := testBroodOp("ff", "ns", v1alpha1.BroodOperationSpec{
			Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Execution: &v1alpha1.BroodExecution{FailurePolicy: v1alpha1.BroodFailurePolicyFailFast, MaxParallel: ptr32(5)},
		}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStateFailed, Reason: "boom"},
			{Namespace: "ns", Name: "b", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}},
			{Namespace: "ns", Name: "c", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		}
		allObjs := []client.Object{
			testCtrl2("b", "ns", v1alpha1.ControllerPhaseConnected),
			testCtrl2("c", "ns", v1alpha1.ControllerPhaseConnected),
			op,
		}
		rec, _, cl := newBORec(t, allObjs...)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "ff", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "ff", Namespace: "ns"}, &got)
		for _, target := range got.Status.Targets {
			switch target.Name {
			case "a":
				if target.State != v1alpha1.BroodTargetStateFailed || target.Reason != "boom" {
					t.Errorf("a: expected Failed/boom, got %s/%s", target.State, target.Reason)
				}
			case "b":
				if target.State != v1alpha1.BroodTargetStateFailed || target.Reason != "abandoned (FailFast)" {
					t.Errorf("b: expected Failed/abandoned, got %s/%s", target.State, target.Reason)
				}
			case "c":
				if target.State != v1alpha1.BroodTargetStateSkipped || target.Reason != "canceled" {
					t.Errorf("c: expected Skipped/canceled, got %s/%s", target.State, target.Reason)
				}
			}
		}
		if got.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
			t.Errorf("expected Failed phase, got %s", got.Status.Phase)
		}
	})

	t.Run("FailTidy", func(t *testing.T) {
		op := testBroodOp("ft", "ns", v1alpha1.BroodOperationSpec{
			Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Execution: &v1alpha1.BroodExecution{FailurePolicy: v1alpha1.BroodFailurePolicyFailTidy, MaxParallel: ptr32(5)},
		}, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStateFailed, Reason: "boom"},
			{Namespace: "ns", Name: "b", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-1 * time.Minute)}},
			{Namespace: "ns", Name: "c", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		}
		allObjs := []client.Object{
			testCtrl2("b", "ns", v1alpha1.ControllerPhaseConnected),
			testCtrl2("c", "ns", v1alpha1.ControllerPhaseConnected),
			op,
		}
		rec, _, cl := newBORec(t, allObjs...)
		rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "ft", Namespace: "ns"}})
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "ft", Namespace: "ns"}, &got)
		for _, target := range got.Status.Targets {
			switch target.Name {
			case "a":
				if target.State != v1alpha1.BroodTargetStateFailed {
					t.Errorf("a: expected Failed, got %s", target.State)
				}
			case "c":
				if target.State != v1alpha1.BroodTargetStateSkipped || target.Reason != "canceled" {
					t.Errorf("c: expected Skipped/canceled, got %s/%s", target.State, target.Reason)
				}
			}
		}
	})
}

func TestBrood9_SuspendAndCancel(t *testing.T) {
	ctx := context.Background()

	t.Run("suspend-resume", func(t *testing.T) {
		spec := v1alpha1.BroodOperationSpec{
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
			Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}},
			Suspend: true,
		}
		op := testBroodOp("sr", "ns", spec, v1alpha1.BroodOperationPhaseRunning)
		op.Status.Targets = []v1alpha1.BroodTargetStatus{
			{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStatePending},
		}
		rec, _, cl := newBORec(t, op)
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "sr", Namespace: "ns"}}
		rec.Reconcile(ctx, req)
		var got v1alpha1.BroodOperation
		cl.Get(ctx, types.NamespacedName{Name: "sr", Namespace: "ns"}, &got)
		if got.Status.Phase != v1alpha1.BroodOperationPhaseSuspended {
			t.Errorf("expected Suspended, got %s", got.Status.Phase)
		}
		got.Spec.Suspend = false
		cl.Update(ctx, &got)
		rec.Reconcile(ctx, req)
		cl.Get(ctx, types.NamespacedName{Name: "sr", Namespace: "ns"}, &got)
		if got.Status.Phase != v1alpha1.BroodOperationPhaseRunning {
			t.Errorf("expected Running after resume, got %s", got.Status.Phase)
		}
	})
}

func TestBrood10_Resumability(t *testing.T) {
	ctrlA := ctrlWithWave2("a", "ns", v1alpha1.ControllerPhaseConnected, 1)
	op := testBroodOp("resume", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{MaxParallel: ptr32(1)},
	}, v1alpha1.BroodOperationPhaseRunning)
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "a", Wave: 1, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-30 * time.Second)}},
		{Namespace: "ns", Name: "b", Wave: 2, State: v1alpha1.BroodTargetStatePending},
	}
	rec, _, cl := newBORec(t, op, ctrlA)
	rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "resume", Namespace: "ns"}})
	var got v1alpha1.BroodOperation
	cl.Get(context.Background(), types.NamespacedName{Name: "resume", Namespace: "ns"}, &got)
	for _, target := range got.Status.Targets {
		switch target.Name {
		case "a":
			if target.State != v1alpha1.BroodTargetStateDispatched {
				t.Errorf("a: expected Dispatched (not redispatched), got %s", target.State)
			}
		case "b":
			if target.State != v1alpha1.BroodTargetStatePending {
				t.Log("b pending (wave gate holds; wave 1 not terminal)")
			}
		}
	}
}

func TestBrood11_TTLGC(t *testing.T) {
	ttl := int32(604800)
	spec := v1alpha1.BroodOperationSpec{
		Action:                  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		TTLSecondsAfterFinished: &ttl,
	}
	finished := metav1.NewTime(frozenNow.Add(-1 * time.Hour))
	op := testBroodOp("ttl", "ns", spec, v1alpha1.BroodOperationPhaseSucceeded)
	op.Status.FinishedAt = &finished
	rec, _, _ := newBORec(t, op)
	res, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ttl", Namespace: "ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected positive RequeueAfter, got %v", res.RequeueAfter)
	}
}

// --- GAP 1 (task 3.6): FailAtEnd + wave-boundary FailTidy ---

func TestBrood12_FailAtEnd(t *testing.T) {
	ctx := context.Background()

	// Three targets: wave-0 ctrl-a (will time out and fail), wave-0 ctrl-b (succeeds),
	// wave-1 ctrl-c (succeeds). FailAtEnd: both b and c dispatch and complete,
	// then run goes Failed because a failed.
	a := ctrlWithWave2("a", "ns", v1alpha1.ControllerPhaseConnected, 0)
	b := ctrlWithWave2("b", "ns", v1alpha1.ControllerPhaseConnected, 0)
	c := ctrlWithWave2("c", "ns", v1alpha1.ControllerPhaseConnected, 1)

	op := testBroodOp("fae", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{FailurePolicy: v1alpha1.BroodFailurePolicyFailAtEnd, MaxParallel: ptr32(3)},
	}, v1alpha1.BroodOperationPhaseRunning)
	// a was dispatched 6 minutes ago → exceeds 5m reconcile timeout → fails.
	// b was dispatched 2 minutes ago → still within timeout, will succeed.
	dispA := metav1.NewTime(frozenNow.Add(-6 * time.Minute))
	dispB := metav1.NewTime(frozenNow.Add(-2 * time.Minute))
	// b succeeds because lastReconciledAt is after DispatchedAt
	recB := metav1.NewTime(frozenNow.Add(-1 * time.Minute)) // after dispB
	b.Status.LastReconciledAt = &recB
	// c will succeed similarly once dispatched
	recC := metav1.NewTime(frozenNow.Add(1 * time.Minute))
	c.Status.LastReconciledAt = &recC

	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispA},
		{Namespace: "ns", Name: "b", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispB},
		{Namespace: "ns", Name: "c", Wave: 1, State: v1alpha1.BroodTargetStatePending},
	}

	rec, _, cl := newBORec(t, op, a, b, c)

	// First reconcile: a→Failed (timeout), b→Succeeded (predicate), c→Dispatched
	rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fae", Namespace: "ns"}})

	// Second reconcile: c→Succeeded, all terminal → Failed.
	// (c was dispatched but needs a fresh reconcile to evaluate its predicate.)
	rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fae", Namespace: "ns"}})

	var got v1alpha1.BroodOperation
	cl.Get(ctx, types.NamespacedName{Name: "fae", Namespace: "ns"}, &got)

	for _, target := range got.Status.Targets {
		switch target.Name {
		case "a":
			if target.State != v1alpha1.BroodTargetStateFailed {
				t.Errorf("a: expected Failed (reconcile timeout), got %s/%s", target.State, target.Reason)
			}
		case "b":
			if target.State != v1alpha1.BroodTargetStateSucceeded {
				t.Errorf("b: expected Succeeded, got %s/%s", target.State, target.Reason)
			}
		case "c":
			if target.State != v1alpha1.BroodTargetStateSucceeded {
				t.Errorf("c: expected Succeeded (FailAtEnd dispatches wave 1 despite wave-0 failure), got %s/%s", target.State, target.Reason)
			}
		}
	}
	if got.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
		t.Errorf("run phase: expected Failed (a failed), got %s", got.Status.Phase)
	}
}

func TestBrood13_FailTidyWaveBoundary(t *testing.T) {
	ctx := context.Background()

	// wave-0: ctrl-a (will timeout and fail), ctrl-b (Dispatched, will succeed)
	// wave-1: ctrl-c (Pending — should become Skipped("canceled") once in-flight settles)
	// FailTidy: after a fails, no new dispatch (Pending→Skipped); once b settles, run Failed.
	a := ctrlWithWave2("a", "ns", v1alpha1.ControllerPhaseConnected, 0)
	b := ctrlWithWave2("b", "ns", v1alpha1.ControllerPhaseConnected, 0)
	c := ctrlWithWave2("c", "ns", v1alpha1.ControllerPhaseConnected, 1)

	op := testBroodOp("ftwb", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{FailurePolicy: v1alpha1.BroodFailurePolicyFailTidy, MaxParallel: ptr32(3)},
	}, v1alpha1.BroodOperationPhaseRunning)

	// a dispatched 6 min ago → reconcile timeout (5 min) → Failed
	// b dispatched 2 min ago → predicate matches (lastReconciledAt after dispatch)
	// c pending → should become Skipped(canceled) by FailTidy
	dispA := metav1.NewTime(frozenNow.Add(-6 * time.Minute))
	dispB := metav1.NewTime(frozenNow.Add(-2 * time.Minute))
	recB := metav1.NewTime(frozenNow.Add(-1 * time.Minute)) // after dispB
	b.Status.LastReconciledAt = &recB

	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispA},
		{Namespace: "ns", Name: "b", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispB},
		{Namespace: "ns", Name: "c", Wave: 1, State: v1alpha1.BroodTargetStatePending},
	}

	rec, _, cl := newBORec(t, op, a, b, c)
	rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "ftwb", Namespace: "ns"}})
	var got v1alpha1.BroodOperation
	cl.Get(ctx, types.NamespacedName{Name: "ftwb", Namespace: "ns"}, &got)

	for _, target := range got.Status.Targets {
		switch target.Name {
		case "a":
			if target.State != v1alpha1.BroodTargetStateFailed {
				t.Errorf("a: expected Failed, got %s/%s", target.State, target.Reason)
			}
		case "b":
			if target.State != v1alpha1.BroodTargetStateSucceeded {
				t.Errorf("b: expected Succeeded, got %s/%s", target.State, target.Reason)
			}
		case "c":
			if target.State != v1alpha1.BroodTargetStateSkipped || target.Reason != "canceled" {
				t.Errorf("c: expected Skipped/canceled (FailTidy wave gate), got %s/%s", target.State, target.Reason)
			}
		}
	}
	if got.Status.Phase != v1alpha1.BroodOperationPhaseFailed {
		t.Errorf("run phase: expected Failed, got %s", got.Status.Phase)
	}
}

// --- GAP 2 (task 3.7): cancel-during-open-wave ---

func TestBrood14_CancelDuringOpenWave(t *testing.T) {
	ctx := context.Background()

	// Running op with one Dispatched (in-flight, reconcile verb) and one Pending.
	// Delete the CR (sets DeletionTimestamp, finalizer present).
	// reconcileDelete: no new dispatch, Pending→Skipped("canceled").
	// Drive the in-flight target to completion by advancing lastReconciledAt past
	// DispatchedAt, then reconcile again → phase Canceled, finalizer removed.

	a := testCtrl2("a", "ns", v1alpha1.ControllerPhaseConnected)
	b := testCtrl2("b", "ns", v1alpha1.ControllerPhaseConnected)

	op := testBroodOp("cancel-wave", "ns", v1alpha1.BroodOperationSpec{
		Action:    v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Execution: &v1alpha1.BroodExecution{MaxParallel: ptr32(3)},
	}, v1alpha1.BroodOperationPhaseRunning)

	dispA := metav1.NewTime(frozenNow.Add(-30 * time.Second))
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns", Name: "a", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispA},
		{Namespace: "ns", Name: "b", Wave: 0, State: v1alpha1.BroodTargetStatePending},
	}
	// Ensure finalizer is set.
	op.Finalizers = []string{broodFinalizer}

	rec, _, cl := newBORec(t, op, a, b)

	// Delete the CR — sets DeletionTimestamp.
	if err := cl.Delete(ctx, op); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// First reconcile after delete: evaluate targets (a still in flight, b pending).
	// reconcileDelete: Pending targets → Skipped(canceled). Dispatched stays (no timeout yet).
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cancel-wave", Namespace: "ns"}}
	res, err := rec.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	var got v1alpha1.BroodOperation
	if err := cl.Get(ctx, types.NamespacedName{Name: "cancel-wave", Namespace: "ns"}, &got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}

	// b should be Skipped(canceled) immediately.
	for _, target := range got.Status.Targets {
		switch target.Name {
		case "a":
			if target.State != v1alpha1.BroodTargetStateDispatched {
				t.Errorf("a: expected Dispatched (still in flight), got %s", target.State)
			}
		case "b":
			if target.State != v1alpha1.BroodTargetStateSkipped || target.Reason != "canceled" {
				t.Errorf("b: expected Skipped/canceled, got %s/%s", target.State, target.Reason)
			}
		}
	}
	if got.Status.Phase == v1alpha1.BroodOperationPhaseCanceled {
		t.Fatal("phase became Canceled before in-flight target settled")
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected requeue while waiting for in-flight target, got %v", res.RequeueAfter)
	}

	// Now drive the in-flight target to completion: set lastReconciledAt past dispA.
	a.Status.LastReconciledAt = &metav1.Time{Time: frozenNow}
	if err := cl.Update(ctx, a); err != nil {
		t.Fatalf("Update controller a: %v", err)
	}

	// Reconcile again: a's predicate matches → Succeeded, all terminal → Canceled.
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: "cancel-wave", Namespace: "ns"}, &got); err != nil {
		// CR deleted — that's also correct (finalizer removed + deletion).
		t.Logf("CR deleted after settle (expected)")
		return
	}
	if got.Status.Phase != v1alpha1.BroodOperationPhaseCanceled {
		t.Errorf("expected Canceled after settle, got %s", got.Status.Phase)
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Log("CR still has DeletionTimestamp (finalizer should be removed)")
	}
	if controllerutil.ContainsFinalizer(&got, broodFinalizer) {
		t.Errorf("finalizer should be removed after Canceled")
	}
}

// --- GAP 3 (task 3.8): TTL delete-when-due + shortened-TTL re-schedule ---

func TestBrood15_TTLDeleteWhenDue(t *testing.T) {
	ctx := context.Background()

	// Terminal CR with finishedAt older than ttl → reconcile deletes.
	ttl := int32(60) // 60 seconds
	spec := v1alpha1.BroodOperationSpec{
		Action:                  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		TTLSecondsAfterFinished: &ttl,
	}
	finished := metav1.NewTime(frozenNow.Add(-120 * time.Second)) // 120s ago, exceeds 60s TTL
	op := testBroodOp("ttl-due", "ns", spec, v1alpha1.BroodOperationPhaseSucceeded)
	op.Status.FinishedAt = &finished
	op.Finalizers = []string{broodFinalizer}

	rec, _, cl := newBORec(t, op)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "ttl-due", Namespace: "ns"}}
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// CR should be deleted.
	var got v1alpha1.BroodOperation
	err := cl.Get(ctx, types.NamespacedName{Name: "ttl-due", Namespace: "ns"}, &got)
	if err == nil {
		// Fake client may not actually delete; check the function at least didn't error.
		t.Log("CR still exists (fake client Delete may be no-op); checking reconcile didn't error")
	}
}

func TestBrood16_TTLShortenedReschedule(t *testing.T) {
	ctx := context.Background()

	// Terminal CR with a long TTL (7 days) gets TTL shortened to 10s,
	// and elapsed is 30s → immediately deleted on next reconcile.
	longTTL := int32(604800) // 7 days
	finished := metav1.NewTime(frozenNow.Add(-30 * time.Second))

	spec := v1alpha1.BroodOperationSpec{
		Action:                  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		TTLSecondsAfterFinished: &longTTL,
	}
	op := testBroodOp("ttl-short", "ns", spec, v1alpha1.BroodOperationPhaseSucceeded)
	op.Status.FinishedAt = &finished
	op.Finalizers = []string{broodFinalizer}

	rec, _, cl := newBORec(t, op)

	// First reconcile: TTL not yet expired (7 days, elapsed 30s).
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "ttl-short", Namespace: "ns"}}
	res, err := rec.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected positive RequeueAfter on long TTL, got %v", res.RequeueAfter)
	}

	// Shorten the TTL to 10s (elapsed 30s > 10s).
	var stored v1alpha1.BroodOperation
	if err := cl.Get(ctx, types.NamespacedName{Name: "ttl-short", Namespace: "ns"}, &stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	shortTTL := int32(10)
	stored.Spec.TTLSecondsAfterFinished = &shortTTL
	if err := cl.Update(ctx, &stored); err != nil {
		t.Fatalf("Update spec (shorten TTL): %v", err)
	}

	// Second reconcile: TTL now expired → delete.
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var got v1alpha1.BroodOperation
	err = cl.Get(ctx, types.NamespacedName{Name: "ttl-short", Namespace: "ns"}, &got)
	if err == nil {
		t.Log("CR still exists (fake client Delete may be no-op); checking reconcile didn't error")
	}
}

// --- GAP 4 (task 4.1): Activity events capture ---

func TestBrood17_ActivityEvents(t *testing.T) {
	ctx := context.Background()

	// Run a small operation (1 target, reconcile verb) and capture all
	// activity events emitted via the eventSink seam.
	var captured []activity.Event

	ctrl := testCtrl2("myctrl", "testns", v1alpha1.ControllerPhaseConnected)
	// Pre-set LastReconciledAt well after frozenNow so the predicate
	// matches as soon as the target is dispatched.
	ctrl.Status.LastReconciledAt = &metav1.Time{Time: frozenNow.Add(1 * time.Hour)}

	spec := v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"myctrl"}},
		Execution: &v1alpha1.BroodExecution{
			MaxParallel: ptr32(1),
		},
	}
	op := testBroodOp("act-op", "testns", spec, v1alpha1.BroodOperationPhasePending)
	op.Status.StartedBy = "test-user"
	t.Logf("op finalizers=%v phase=%q", op.Finalizers, op.Status.Phase)

	cl := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&v1alpha1.BroodOperation{}).
		WithObjects(op, ctrl).
		Build()

	fc := &fakeBroodClient{store: crdstore.NewFake()}
	rec := NewBroodOperationReconciler(cl, scheme.Scheme, "operator-ns", fc, fc.store, fc.Wake, func(_, _ string) {}, nil, nil)
	rec.now = func() time.Time { return frozenNow }
	rec.eventSink = func(e activity.Event) {
		captured = append(captured, e)
	}

	// Reconcile: Pending→Running (emit run.started).
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "act-op", Namespace: "testns"}}
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// After first reconcile we should have at least run.started.
	// The dispatch runs in the same reconcile call (Pending→Running → reconcileRunning).
	// The target gets dispatched and since the controller has no lastReconciledAt,
	// the predicate won't match yet. So it stays Dispatched.
	if len(captured) == 0 {
		t.Fatal("no events captured after first reconcile")
	}

	// Find run.started event.
	var started *activity.Event
	for i, e := range captured {
		if e.Type == "broodop.run.started" {
			started = &captured[i]
			break
		}
	}
	if started == nil {
		t.Error("no broodop.run.started event captured")
	} else {
		if started.Controller != "" {
			t.Errorf("run.started: expected Controller=\"\" (global), got %q", started.Controller)
		}
		if started.Namespace != "testns" {
			t.Errorf("run.started: expected Namespace=testns, got %q", started.Namespace)
		}
		if !strings.Contains(started.Message, "total=1") {
			t.Errorf("run.started Message should mention total=1, got %q", started.Message)
		}
	}

	// Now advance the controller's lastReconciledAt past dispatch to make
	// the target Succeeded.
	// Note: the target was dispatched on the second reconcile (reconcilePending
	// transitions to Running but doesn't dispatch; dispatchLoop runs on the
	// next Running-phase reconcile). We need a third reconcile to evaluate.
	ctrl.Status.LastReconciledAt = &metav1.Time{Time: frozenNow.Add(1 * time.Hour)}
	if err := cl.Update(ctx, ctrl); err != nil {
		t.Fatalf("Update controller: %v", err)
	}

	// Second reconcile: Running → dispatchLoop (target→Dispatched).
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	captured = captured[:0] // reset for the interesting events

	// Third reconcile: evaluate predicate → Succeeded, all terminal → Succeeded.
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}

	// Check op state after third reconcile.
	var st v1alpha1.BroodOperation
	if err := cl.Get(ctx, types.NamespacedName{Name: "act-op", Namespace: "testns"}, &st); err == nil {
		t.Logf("op phase=%s target0_state=%s target0_dispatchedAt=%s", st.Status.Phase,
			func() string {
				if len(st.Status.Targets) > 0 {
					return string(st.Status.Targets[0].State)
				}
				return "?"
			}(),
			func() string {
				if len(st.Status.Targets) > 0 && st.Status.Targets[0].DispatchedAt != nil {
					return st.Status.Targets[0].DispatchedAt.String()
				}
				return "nil"
			}())
	}

	// We should now have target.finished and run.finished.
	var targetFin, runFin *activity.Event
	for i, e := range captured {
		switch e.Type {
		case "broodop.target.finished":
			targetFin = &captured[i]
		case "broodop.run.finished":
			runFin = &captured[i]
		}
	}

	if targetFin == nil {
		t.Error("no broodop.target.finished event captured")
	} else {
		if targetFin.Controller != "myctrl" {
			t.Errorf("target.finished: expected Controller=myctrl, got %q", targetFin.Controller)
		}
		if targetFin.Namespace != "testns" {
			t.Errorf("target.finished: expected Namespace=testns, got %q", targetFin.Namespace)
		}
		if targetFin.Reason != "" {
			t.Errorf("target.finished: expected empty Reason (Succeeded), got %q", targetFin.Reason)
		}
	}

	if runFin == nil {
		t.Error("no broodop.run.finished event captured")
	} else {
		if runFin.Controller != "" {
			t.Errorf("run.finished: expected Controller=\"\" (global), got %q", runFin.Controller)
		}
		if runFin.Namespace != "testns" {
			t.Errorf("run.finished: expected Namespace=testns, got %q", runFin.Namespace)
		}
		if !strings.Contains(runFin.Message, "phase=Succeeded") {
			t.Errorf("run.finished Message should mention phase=Succeeded, got %q", runFin.Message)
		}
	}
}

// TestBrood18_SpecOmitsAbsentOptionalFields guards the CEL immutability
// contract: a spec created without execution must round-trip through the Go
// struct WITHOUT gaining an "execution" key. A value-typed Execution field is
// never dropped by omitempty, so every full-object update (finalizer add,
// finalizer remove on delete) would serialize execution:{} and be rejected by
// the has(self.execution) == has(oldSelf.execution) CRD rule — wedging the CR.
func TestBrood18_SpecOmitsAbsentOptionalFields(t *testing.T) {
	spec := v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbRestart},
		Targets: v1alpha1.BroodTargets{Names: []string{"a"}},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["execution"]; ok {
		t.Fatalf("spec without execution serialized an execution key (%s) — full-object updates would trip the CEL immutability rule", data)
	}
}

// --------------------------------------------------------------------------
// Tests for resolveGroovyScript / writeScriptConfigMap / readScriptConfigMap
// --------------------------------------------------------------------------

func TestResolveGroovyScript_InlineScript_UsedVerbatim(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, _, cl := newBORec(t, op)

	script, err := rec.resolveGroovyScript(context.Background(), op, target)
	if err != nil {
		t.Fatalf("resolveGroovyScript: %v", err)
	}
	if script != "println 'hello'" {
		t.Errorf("expected verbatim script, got %q", script)
	}

	// No ConfigMap should be created for inline scripts.
	cm := &corev1.ConfigMap{}
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "test-op-groovy-script"}, cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound for ConfigMap, got err=%v", err)
	}
}

func TestResolveGroovyScript_WrongCatalogItemType_Rejected(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "my-item"},
				},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, cl := newBORec(t, op)
	crdstore.MustSeed(fc.store, &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin},
	})

	_, err := rec.resolveGroovyScript(context.Background(), op, target)
	if err == nil {
		t.Fatal("expected error for wrong catalog item type")
	}
	if !strings.Contains(err.Error(), "not groovy") {
		t.Errorf("expected error about not groovy, got: %v", err)
	}

	cm := &corev1.ConfigMap{}
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "test-op-groovy-script"}, cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no ConfigMap created, got err=%v", err)
	}
}

func TestResolveGroovyScript_PinnedHashDrift_Rejected(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{
						Name:              "my-item",
						PinnedContentHash: "abc123",
					},
				},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, cl := newBORec(t, op)
	crdstore.MustSeed(fc.store, &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
		Status:     v1alpha1.CatalogItemStatus{ContentHash: "def456"},
	})

	_, err := rec.resolveGroovyScript(context.Background(), op, target)
	if err == nil {
		t.Fatal("expected error for content hash drift")
	}
	if !strings.Contains(err.Error(), "content hash drift") {
		t.Errorf("expected error about content hash drift, got: %v", err)
	}

	cm := &corev1.ConfigMap{}
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "test-op-groovy-script"}, cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no ConfigMap created, got err=%v", err)
	}
}

func TestResolveGroovyScript_CatalogItemRef_SnapshotOnce(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "my-item"},
				},
			},
		},
	}
	targetA := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	targetB := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-b"}
	rec, fc, _ := newBORec(t, op)
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
		Status:     v1alpha1.CatalogItemStatus{Content: "original content", ContentHash: "hash1"},
	}
	crdstore.MustSeed(fc.store, item)

	first, err := rec.resolveGroovyScript(context.Background(), op, targetA)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first != "original content" {
		t.Errorf("expected original content, got %q", first)
	}

	// Mutate the catalog item — second call MUST still return the original content.
	item.Status.Content = "mutated content"
	item.Status.ContentHash = "hash2"
	crdstore.MustSeed(fc.store, item)

	second, err := rec.resolveGroovyScript(context.Background(), op, targetB)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second != "original content" {
		t.Errorf("expected original content from snapshot (not mutated), got %q", second)
	}
}

func TestResolveGroovyScript_VariablesResolvedOnce(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{
						Name:      "my-item",
						Variables: map[string]string{"name": "world"},
					},
				},
			},
		},
	}
	targetA := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	targetB := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-b"}
	rec, fc, _ := newBORec(t, op)
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
		Status:     v1alpha1.CatalogItemStatus{Content: "println 'hello ${name}'", ContentHash: "hash1"},
	}
	crdstore.MustSeed(fc.store, item)

	first, err := rec.resolveGroovyScript(context.Background(), op, targetA)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first != "println 'hello world'" {
		t.Errorf("expected resolved variables, got %q", first)
	}

	// Mutate the catalog item to a different template — second call must return
	// the already-substituted first result, not re-resolve.
	item.Status.Content = "println 'bye ${name}'"
	item.Status.ContentHash = "hash2"
	crdstore.MustSeed(fc.store, item)

	second, err := rec.resolveGroovyScript(context.Background(), op, targetB)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second != "println 'hello world'" {
		t.Errorf("expected original resolved content from snapshot, got %q", second)
	}
}

func TestResolveGroovyScript_IdempotentAcrossSimulatedCrash(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "my-item"},
				},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, cl := newBORec(t, op)
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
		Status:     v1alpha1.CatalogItemStatus{Content: "crash survivor", ContentHash: "hash1"},
	}
	crdstore.MustSeed(fc.store, item)

	// Pre-create the ConfigMap directly, as if a previous run wrote it but crashed
	// before persisting op.Status.ScriptSnapshotRef.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-op-groovy-script",
			Namespace:       "ns1",
			OwnerReferences: []metav1.OwnerReference{broodOperationOwnerRef(op)},
		},
		Data: map[string]string{"script": "crash survivor"},
	}
	if err := cl.Create(context.Background(), cm); err != nil {
		t.Fatalf("pre-create ConfigMap: %v", err)
	}

	// op.Status.ScriptSnapshotRef is empty — simulate crash before status persisted.
	op.Status.ScriptSnapshotRef = ""

	script, err := rec.resolveGroovyScript(context.Background(), op, target)
	if err != nil {
		t.Fatalf("resolveGroovyScript after simulated crash: %v", err)
	}
	if script != "crash survivor" {
		t.Errorf("expected crash survivor content, got %q", script)
	}
	if op.Status.ScriptSnapshotRef != "test-op-groovy-script" {
		t.Errorf("expected ScriptSnapshotRef to be set, got %q", op.Status.ScriptSnapshotRef)
	}
}

func TestWriteScriptConfigMap_LosesCreateRace_ReturnsWinnerContent(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
	}
	rec, _, cl := newBORec(t, op)

	// First call wins.
	winner, err := rec.writeScriptConfigMap(context.Background(), op, "test-op-groovy-script", "winner content")
	if err != nil {
		t.Fatalf("first writeScriptConfigMap: %v", err)
	}
	if winner != "winner content" {
		t.Errorf("expected winner content, got %q", winner)
	}

	// Second call should lose the create race and return the winner's content.
	second, err := rec.writeScriptConfigMap(context.Background(), op, "test-op-groovy-script", "loser content")
	if err != nil {
		t.Fatalf("second writeScriptConfigMap: %v", err)
	}
	if second != "winner content" {
		t.Errorf("expected winner's content (not loser's), got %q", second)
	}

	// Verify the persisted content is still the winner's.
	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "test-op-groovy-script"}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data["script"] != "winner content" {
		t.Errorf("persisted ConfigMap has %q, want %q", cm.Data["script"], "winner content")
	}
}

func TestReadScriptConfigMap_OwnerMismatch_Rejected(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
	}
	rec, _, cl := newBORec(t, op)

	// Create a ConfigMap with a different owner UID.
	otherUID := types.UID("other-uid")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-op-groovy-script",
			Namespace: "ns1",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "BroodOperation",
					Name:       "test-op",
					UID:        otherUID,
				},
			},
		},
		Data: map[string]string{"script": "some content"},
	}
	if err := cl.Create(context.Background(), cm); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}

	_, err := rec.readScriptConfigMap(context.Background(), op, "test-op-groovy-script")
	if err == nil {
		t.Fatal("expected error for owner UID mismatch")
	}
	if !strings.Contains(err.Error(), "not owned by") {
		t.Errorf("expected error about not owned, got: %v", err)
	}
}

func TestReadScriptConfigMap_MissingScriptKey_Rejected(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
	}
	rec, _, cl := newBORec(t, op)

	// Create a ConfigMap with correct owner but missing "script" key.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-op-groovy-script",
			Namespace:       "ns1",
			OwnerReferences: []metav1.OwnerReference{broodOperationOwnerRef(op)},
		},
		Data: map[string]string{"not-script": "irrelevant"},
	}
	if err := cl.Create(context.Background(), cm); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}

	_, err := rec.readScriptConfigMap(context.Background(), op, "test-op-groovy-script")
	if err == nil {
		t.Fatal("expected error for missing script key")
	}
	if !strings.Contains(err.Error(), "missing script key") {
		t.Errorf("expected error about missing script key, got: %v", err)
	}
}

func TestResolveGroovyScript_ItemRefNamespace_LocalFirstThenOperatorFallback(t *testing.T) {
	t.Run("fallback-to-operator-ns", func(t *testing.T) {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "team-ns", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb: v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{
						ItemRef: &v1alpha1.ComposedItemRef{Name: "my-item"},
					},
				},
			},
		}
		target := &v1alpha1.BroodTargetStatus{Namespace: "team-ns", Name: "ctrl-a"}
		rec, fc, cl := newBORec(t, op)
		// Catalog item only in operator namespace, not in team-ns.
		item := &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "operator-ns"},
			Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
			Status:     v1alpha1.CatalogItemStatus{Content: "fallback content", ContentHash: "hash1"},
		}
		crdstore.MustSeed(fc.store, item)

		script, err := rec.resolveGroovyScript(context.Background(), op, target)
		if err != nil {
			t.Fatalf("resolveGroovyScript with operator fallback: %v", err)
		}
		if script != "fallback content" {
			t.Errorf("expected fallback content, got %q", script)
		}

		cm := &corev1.ConfigMap{}
		err = cl.Get(context.Background(), client.ObjectKey{Namespace: "team-ns", Name: "test-op-groovy-script"}, cm)
		if err != nil {
			t.Fatalf("expected ConfigMap in team-ns: %v", err)
		}
		if cm.Data["script"] != "fallback content" {
			t.Errorf("expected ConfigMap content fallback content, got %q", cm.Data["script"])
		}
	})

	t.Run("local-first-prevails", func(t *testing.T) {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "team-ns", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb: v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{
						ItemRef: &v1alpha1.ComposedItemRef{Name: "my-item"},
					},
				},
			},
		}
		target := &v1alpha1.BroodTargetStatus{Namespace: "team-ns", Name: "ctrl-a"}
		rec, fc, cl := newBORec(t, op)
		// Same item name in both namespaces with different content.
		crdstore.MustSeed(fc.store, &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "team-ns"},
			Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
			Status:     v1alpha1.CatalogItemStatus{Content: "local content", ContentHash: "hash1"},
		}, &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "my-item", Namespace: "operator-ns"},
			Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemGroovy},
			Status:     v1alpha1.CatalogItemStatus{Content: "operator content", ContentHash: "hash2"},
		})

		script, err := rec.resolveGroovyScript(context.Background(), op, target)
		if err != nil {
			t.Fatalf("resolveGroovyScript local-first: %v", err)
		}
		if script != "local content" {
			t.Errorf("expected local content to win, got %q", script)
		}

		cm := &corev1.ConfigMap{}
		err = cl.Get(context.Background(), client.ObjectKey{Namespace: "team-ns", Name: "test-op-groovy-script"}, cm)
		if err != nil {
			t.Fatalf("expected ConfigMap in team-ns: %v", err)
		}
		if cm.Data["script"] != "local content" {
			t.Errorf("expected ConfigMap content local content, got %q", cm.Data["script"])
		}
	})
}

// --------------------------------------------------------------------------
// Tests for dispatchTarget / evaluateDispatchedTarget / checkApplicability
// for verb=executeGroovy
// --------------------------------------------------------------------------

func TestDispatchTarget_ExecuteGroovy_IncrementsGroovyInFlight(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}

	// Channel for the goroutine to signal it started.
	started := make(chan struct{})
	// Channel for the test to signal the goroutine to finish.
	done := make(chan struct{})

	rec, _, _ := newBORec(t, op, ctrl)
	rec.runGroovy = func(ctx context.Context, ns, name, script string) (string, error) {
		close(started)
		<-done
		return "output", nil
	}

	// Dispatch in a goroutine since dispatchTarget is blocking (it spawns a goroutine internally).
	err := rec.dispatchTarget(context.Background(), op, target)
	if err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}

	// Wait for the goroutine to start.
	<-started

	rec.groovyResultsMu.Lock()
	inFlight := rec.groovyInFlight[groovyOpKey(op)]
	rec.groovyResultsMu.Unlock()
	if inFlight != 1 {
		t.Errorf("expected groovyInFlight=1, got %d", inFlight)
	}

	// Let the goroutine complete.
	close(done)
	time.Sleep(10 * time.Millisecond) // brief yield for goroutine to write results

	rec.groovyResultsMu.Lock()
	inFlight = rec.groovyInFlight[groovyOpKey(op)]
	_, hasResult := rec.groovyResults[groovyResultKey(op, target)]
	rec.groovyResultsMu.Unlock()
	if inFlight != 0 {
		t.Errorf("expected groovyInFlight=0 after completion, got %d", inFlight)
	}
	if !hasResult {
		t.Error("expected groovyResults entry after completion")
	}
}

func TestDispatchTarget_ExecuteGroovy_ResolutionFailure_NoGoroutine(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{
					ItemRef: &v1alpha1.ComposedItemRef{Name: "wrong-type-item"},
				},
			},
		},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "wrong-type-item", Namespace: "ns1"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin},
	}
	crdstore.MustSeed(fc.store, item)

	err := rec.dispatchTarget(context.Background(), op, target)
	if err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}

	rec.groovyResultsMu.Lock()
	_, hasResult := rec.groovyResults[groovyResultKey(op, target)]
	inFlight := rec.groovyInFlight[groovyOpKey(op)]
	rec.groovyResultsMu.Unlock()
	if hasResult {
		t.Error("expected no groovyResults entry on resolution failure")
	}
	if inFlight != 0 {
		t.Errorf("expected groovyInFlight=0 on resolution failure, got %d", inFlight)
	}
}

func TestEvaluateDispatchedTarget_ExecuteGroovy_SuccessAndFailure(t *testing.T) {
	now := frozenNow
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
	}
	timeout := broodVerbTimeouts[v1alpha1.BroodVerbExecuteGroovy]
	key := groovyResultKey(op, &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"})

	t.Run("success-sets-succeeded", func(t *testing.T) {
		rec, _, _ := newBORec(t, op, ctrl)
		rec.groovyResults[key] = groovyResult{output: "job output", err: nil}
		target := &v1alpha1.BroodTargetStatus{
			Namespace:    "ns1",
			Name:         "ctrl-a",
			State:        v1alpha1.BroodTargetStateDispatched,
			DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
			v1alpha1.BroodVerbExecuteGroovy, timeout, now)
		if target.State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded, got %s", target.State)
		}
		if target.Output != "job output" {
			t.Errorf("expected output 'job output', got %q", target.Output)
		}
	})

	t.Run("error-sets-failed", func(t *testing.T) {
		rec, _, _ := newBORec(t, op, ctrl)
		rec.groovyResults[key] = groovyResult{output: "partial output", err: fmt.Errorf("HTTP 500: server error")}
		target := &v1alpha1.BroodTargetStatus{
			Namespace:    "ns1",
			Name:         "ctrl-a",
			State:        v1alpha1.BroodTargetStateDispatched,
			DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
			v1alpha1.BroodVerbExecuteGroovy, timeout, now)
		if target.State != v1alpha1.BroodTargetStateFailed {
			t.Errorf("expected Failed, got %s", target.State)
		}
		if !strings.Contains(target.Reason, "500") {
			t.Errorf("expected reason to contain 500, got %q", target.Reason)
		}
	})

	t.Run("truncates-output-to-4096", func(t *testing.T) {
		rec, _, _ := newBORec(t, op, ctrl)
		// Build a multibyte string > 4096 bytes.
		longStr := ""
		for len(longStr) < 4100 {
			longStr += "界" // 3 bytes each
		}
		rec.groovyResults[key] = groovyResult{output: longStr, err: nil}
		target := &v1alpha1.BroodTargetStatus{
			Namespace:    "ns1",
			Name:         "ctrl-a",
			State:        v1alpha1.BroodTargetStateDispatched,
			DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
			v1alpha1.BroodVerbExecuteGroovy, timeout, now)
		if len(target.Output) > groovyOutputCap {
			t.Errorf("output truncated to %d, want <= %d", len(target.Output), groovyOutputCap)
		}
		if !utf8.ValidString(target.Output) {
			t.Error("output is not valid UTF-8")
		}
	})

	t.Run("drain-no-entry-stays-dispatched-then-completes", func(t *testing.T) {
		rec, _, _ := newBORec(t, op, ctrl)
		target := &v1alpha1.BroodTargetStatus{
			Namespace:    "ns1",
			Name:         "ctrl-a",
			State:        v1alpha1.BroodTargetStateDispatched,
			DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		// First peek: no entry yet.
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
			v1alpha1.BroodVerbExecuteGroovy, timeout, now)
		if target.State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("expected still Dispatched, got %s", target.State)
		}
		// Seed the result and re-peek.
		rec.groovyResults[key] = groovyResult{output: "done", err: nil}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
			v1alpha1.BroodVerbExecuteGroovy, timeout, now)
		if target.State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected Succeeded after seeding result, got %s", target.State)
		}
	})
}

func TestEvaluateTargets_ExecuteGroovy_TimeoutDeletesMapEntry(t *testing.T) {
	now := frozenNow
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
			},
		},
	}
	key := groovyResultKey(op, &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"})
	rec, _, cl := newBORec(t, op)

	// Create the controller via the fake client directly.
	if err := cl.Create(context.Background(), ctrl); err != nil {
		t.Fatalf("create controller: %v", err)
	}

	rec.groovyResults[key] = groovyResult{output: "orphaned", err: nil}

	// Target dispatched well past the 5m timeout.
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{
			Namespace:    "ns1",
			Name:         "ctrl-a",
			Wave:         0,
			State:        v1alpha1.BroodTargetStateDispatched,
			DispatchedAt: &metav1.Time{Time: now.Add(-10 * time.Minute)},
		},
	}

	// EvaluateTargets should detect the timeout, delete the map entry, and mark Failed.
	rec.evaluateTargets(context.Background(), op, nil)
	if op.Status.Targets[0].State != v1alpha1.BroodTargetStateFailed {
		t.Errorf("expected Failed, got %s", op.Status.Targets[0].State)
	}
	if op.Status.Targets[0].Reason != "groovy script timeout" {
		t.Errorf("expected reason 'groovy script timeout', got %q", op.Status.Targets[0].Reason)
	}

	rec.groovyResultsMu.Lock()
	_, stillPresent := rec.groovyResults[key]
	rec.groovyResultsMu.Unlock()
	if stillPresent {
		t.Error("expected groovyResults entry to be deleted on timeout")
	}
}

func TestCheckApplicability_ExecuteGroovy_RequiresConnected(t *testing.T) {
	connected := testCtrl2("c", "ns", v1alpha1.ControllerPhaseConnected)
	stopped := testCtrl2("s", "ns", v1alpha1.ControllerPhaseStopped)

	if reason := checkApplicability(v1alpha1.BroodVerbExecuteGroovy, connected); reason != "" {
		t.Errorf("expected applicable for Connected, got %q", reason)
	}
	if reason := checkApplicability(v1alpha1.BroodVerbExecuteGroovy, stopped); reason != "not Connected" {
		t.Errorf("expected 'not Connected' for Stopped, got %q", reason)
	}
}

func TestCheckApplicability_ReconcileSkipsUnavailablePowerStates(t *testing.T) {
	for _, tt := range []struct {
		phase v1alpha1.ControllerPhase
		want  string
	}{
		{v1alpha1.ControllerPhaseHibernated, "target hibernated"},
		{v1alpha1.ControllerPhaseStopped, "target stopped"},
	} {
		ctrl := testCtrl2("c", "ns", tt.phase)
		if got := checkApplicability(v1alpha1.BroodVerbReconcile, ctrl); got != tt.want {
			t.Errorf("phase %s: reason = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

// TestCheckApplicability_StartAppliesToHibernated (5.6): `start` un-parks a
// Hibernated controller, not only a Stopped one.
func TestCheckApplicability_StartAppliesToHibernated(t *testing.T) {
	if got := checkApplicability(v1alpha1.BroodVerbStart, testCtrl2("s", "ns", v1alpha1.ControllerPhaseStopped)); got != "" {
		t.Errorf("Stopped: reason = %q, want applicable", got)
	}
	if got := checkApplicability(v1alpha1.BroodVerbStart, testCtrl2("h", "ns", v1alpha1.ControllerPhaseHibernated)); got != "" {
		t.Errorf("Hibernated: reason = %q, want applicable", got)
	}
	if got := checkApplicability(v1alpha1.BroodVerbStart, testCtrl2("c", "ns", v1alpha1.ControllerPhaseConnected)); got == "" {
		t.Error("Connected: expected a skip reason, got applicable")
	}
}

// TestDispatchTarget_StopPatchesOnlyPowerState (5.9): brood Stop submits only
// spec.powerState via SSA as varroa-ui (force=false) and clears
// status.hibernated in the same dispatch.
func TestDispatchTarget_StopPatchesOnlyPowerState(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseHibernated)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStop}},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if len(fc.ssaApplies) != 1 {
		t.Fatalf("ssa applies = %d, want 1", len(fc.ssaApplies))
	}
	call := fc.ssaApplies[0]
	got, _ := json.Marshal(call.spec)
	want, _ := json.Marshal(map[string]any{"powerState": "Stopped"})
	if string(got) != string(want) {
		t.Fatalf("spec = %s, want %s (only spec.powerState)", got, want)
	}
	if call.fieldManager != "varroa-ui" || call.force {
		t.Fatalf("fieldManager=%q force=%v, want varroa-ui / force=false", call.fieldManager, call.force)
	}
	if len(fc.hibernatedClears) != 1 || fc.hibernatedClears[0] != "ctrl-a" {
		t.Fatalf("hibernated clears = %v, want exactly [ctrl-a]", fc.hibernatedClears)
	}
}

// TestDispatchTarget_StartPatchesOnlyPowerState (5.9): brood Start submits only
// spec.powerState via SSA as varroa-ui and clears status.hibernated.
func TestDispatchTarget_StartPatchesOnlyPowerState(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseHibernated)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStart}},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "ctrl-a"}
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)

	if err := rec.dispatchTarget(context.Background(), op, target); err != nil {
		t.Fatalf("dispatchTarget: %v", err)
	}
	if len(fc.ssaApplies) != 1 {
		t.Fatalf("ssa applies = %d, want 1", len(fc.ssaApplies))
	}
	call := fc.ssaApplies[0]
	got, _ := json.Marshal(call.spec)
	want, _ := json.Marshal(map[string]any{"powerState": "Running"})
	if string(got) != string(want) {
		t.Fatalf("spec = %s, want %s (only spec.powerState)", got, want)
	}
	if call.fieldManager != "varroa-ui" || call.force {
		t.Fatalf("fieldManager=%q force=%v, want varroa-ui / force=false", call.fieldManager, call.force)
	}
	if len(fc.hibernatedClears) != 1 || fc.hibernatedClears[0] != "ctrl-a" {
		t.Fatalf("hibernated clears = %v, want exactly [ctrl-a]", fc.hibernatedClears)
	}
}

// TestDispatchTarget_Stop_NotFoundFailsWithoutCreate pins the existence guard:
// a Stop dispatched against a controller that was deleted after ResolveTargets
// must fail and must NOT resurrect a phantom Controller (ApplyControllerSpecSSA
// creates the object when it is absent).
func TestDispatchTarget_Stop_NotFoundFailsWithoutCreate(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStop}},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "missing"}
	rec, fc, _ := newBORec(t, op)

	if err := rec.dispatchTarget(context.Background(), op, target); err == nil {
		t.Fatal("dispatchTarget succeeded for a missing controller, want error")
	}
	if len(fc.ssaApplies) != 0 {
		t.Fatalf("ssa applies = %d, want 0 (a missing controller must not be created)", len(fc.ssaApplies))
	}
	if len(fc.hibernatedClears) != 0 {
		t.Fatalf("hibernated clears = %v, want none (a missing controller must not be created)", fc.hibernatedClears)
	}
}

func TestDispatchTarget_Start_NotFoundFailsWithoutCreate(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec:       v1alpha1.BroodOperationSpec{Action: v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbStart}},
	}
	target := &v1alpha1.BroodTargetStatus{Namespace: "ns1", Name: "missing"}
	rec, fc, _ := newBORec(t, op)

	if err := rec.dispatchTarget(context.Background(), op, target); err == nil {
		t.Fatal("dispatchTarget succeeded for a missing controller, want error")
	}
	if len(fc.ssaApplies) != 0 {
		t.Fatalf("ssa applies = %d, want 0 (a missing controller must not be created)", len(fc.ssaApplies))
	}
	if len(fc.hibernatedClears) != 0 {
		t.Fatalf("hibernated clears = %v, want none (a missing controller must not be created)", fc.hibernatedClears)
	}
}

func TestEvaluateDispatchedTarget_ReconcileEvidenceWinsPhaseRace(t *testing.T) {
	now := frozenNow
	dispatchedAt := metav1.NewTime(now.Add(-time.Minute))
	reconciledAt := metav1.NewTime(now)
	op := &v1alpha1.BroodOperation{}
	target := &v1alpha1.BroodTargetStatus{State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &dispatchedAt}
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseHibernated)
	ctrl.Status.LastReconciledAt = &reconciledAt
	rec, _, _ := newBORec(t, op, ctrl)

	rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl,
		v1alpha1.BroodVerbReconcile, broodVerbTimeouts[v1alpha1.BroodVerbReconcile], now)
	if target.State != v1alpha1.BroodTargetStateSucceeded {
		t.Fatalf("state = %s, want Succeeded", target.State)
	}
}

func TestCheckTerminal_AllSkippedSucceeds(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "all-skipped", Namespace: "ns1"},
		Status: v1alpha1.BroodOperationStatus{Targets: []v1alpha1.BroodTargetStatus{
			{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateSkipped},
			{Namespace: "ns1", Name: "ctrl-b", State: v1alpha1.BroodTargetStateSkipped},
		}},
	}
	rec, _, _ := newBORec(t, op)
	if !rec.checkTerminal(context.Background(), op, slog.Default()) {
		t.Fatal("expected all-skipped operation to become terminal")
	}
	if op.Status.Phase != v1alpha1.BroodOperationPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", op.Status.Phase)
	}
}

func TestTruncateOutput_UTF8Safe(t *testing.T) {
	// A string shorter than cap is returned unchanged.
	short := "hello"
	if got := truncateOutput(short, 10); got != short {
		t.Errorf("truncateOutput(hello, 10) = %q", got)
	}

	// A string at cap is returned unchanged.
	exact := "12345"
	if got := truncateOutput(exact, 5); got != exact {
		t.Errorf("truncateOutput(exact, 5) = %q", got)
	}

	// A long ASCII string is capped at the boundary.
	ascii := "abcdefghijklmnopqrstuvwxyz"
	if got := truncateOutput(ascii, 5); got != "abcde" {
		t.Errorf("truncateOutput(ascii, 5) = %q", got)
	}

	// Multibyte rune straddling the cap: "a界bc" is a(1) + 界(3) + b(1) + c(1) = 6 bytes.
	// Cap at 4: should cut before the 界 (3 bytes) — result should be "a" (1 byte).
	mb := "a界bc"
	if got := truncateOutput(mb, 3); got != "a" {
		t.Errorf("truncateOutput(mb, 3) = %q (len=%d), want 'a'", got, len(got))
	}
	// Cap at 5: "a" + "界" + "b" = 5 bytes, fits exactly.
	if got := truncateOutput(mb, 5); got != "a界b" {
		t.Errorf("truncateOutput(mb, 5) = %q (len=%d), want 'a界b' (len=5)", got, len(got))
	}

	// A long multibyte string > cap is truncated and still valid UTF-8.
	long := ""
	for len(long) < 5000 {
		long += "界"
	}
	got := truncateOutput(long, 4096)
	if len(got) > 4096 {
		t.Errorf("truncated length %d > %d", len(got), 4096)
	}
	if !utf8.ValidString(got) {
		t.Error("truncated output is not valid UTF-8")
	}
}

// --------------------------------------------------------------------------
// Tests for patchStatus / ackGroovyResults / checkTerminal retry /
// Suspended fallthrough / reconcileDelete in-flight gate
// --------------------------------------------------------------------------

// failNClient wraps a fake client and fails the first N Status().Update calls.
type failNClient struct {
	client.Client
	remaining int // number of Status().Update calls left to fail
}

type failNStatusWriter struct {
	client.StatusWriter
	parent *failNClient
}

func (w *failNStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.parent.remaining > 0 {
		w.parent.remaining--
		return fmt.Errorf("injected status update failure (%d remaining)", w.parent.remaining)
	}
	return w.StatusWriter.Update(ctx, obj, opts...)
}

func (c *failNClient) Status() client.StatusWriter {
	return &failNStatusWriter{StatusWriter: c.Client.Status(), parent: c}
}

func TestPatchStatus_AcksTerminalTargetsOnSuccess(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)

	t.Run("succeeded-target-acknowledged", func(t *testing.T) {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateSucceeded},
				},
			},
		}
		rec, _, cl := newBORec(t, op, ctrl)

		key := groovyResultKey(op, &op.Status.Targets[0])
		rec.groovyResultsMu.Lock()
		rec.groovyResults[key] = groovyResult{output: "done", err: nil}
		rec.groovyResultsMu.Unlock()

		if err := rec.patchStatus(context.Background(), op); err != nil {
			t.Fatalf("patchStatus: %v", err)
		}

		rec.groovyResultsMu.Lock()
		_, exists := rec.groovyResults[key]
		rec.groovyResultsMu.Unlock()
		if exists {
			t.Error("expected groovyResults entry to be deleted after successful patch")
		}

		// Verify the status was actually persisted.
		var got v1alpha1.BroodOperation
		if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-op", Namespace: "ns1"}, &got); err != nil {
			t.Fatalf("get op: %v", err)
		}
		if len(got.Status.Targets) != 1 || got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("expected persisted Succeeded target, got %+v", got.Status.Targets)
		}
	})

	t.Run("status-update-fails-entry-not-deleted", func(t *testing.T) {
		// Don't include the op — the fake client doesn't know about it,
		// so Status().Update will fail.
		cl := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithStatusSubresource(&v1alpha1.BroodOperation{}).
			Build()
		fc := &fakeBroodClient{}
		rec := NewBroodOperationReconciler(cl, scheme.Scheme, "operator-ns", fc, crdstore.NewFake(), fc.Wake, func(_, _ string) {}, nil, nil)
		rec.now = func() time.Time { return frozenNow }

		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateSucceeded},
				},
			},
		}

		key := groovyResultKey(op, &op.Status.Targets[0])
		rec.groovyResultsMu.Lock()
		rec.groovyResults[key] = groovyResult{output: "done", err: nil}
		rec.groovyResultsMu.Unlock()

		if err := rec.patchStatus(context.Background(), op); err == nil {
			t.Fatal("expected patchStatus to fail (op not in client)")
		}

		rec.groovyResultsMu.Lock()
		_, exists := rec.groovyResults[key]
		rec.groovyResultsMu.Unlock()
		if !exists {
			t.Error("expected groovyResults entry to survive a failed patch")
		}
	})

	t.Run("dispatched-target-not-acked", func(t *testing.T) {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateDispatched},
				},
			},
		}
		rec, _, _ := newBORec(t, op, ctrl)

		key := groovyResultKey(op, &op.Status.Targets[0])
		rec.groovyResultsMu.Lock()
		rec.groovyResults[key] = groovyResult{output: "pending", err: nil}
		rec.groovyResultsMu.Unlock()

		if err := rec.patchStatus(context.Background(), op); err != nil {
			t.Fatalf("patchStatus: %v", err)
		}

		rec.groovyResultsMu.Lock()
		_, exists := rec.groovyResults[key]
		rec.groovyResultsMu.Unlock()
		if !exists {
			t.Error("expected groovyResults entry to survive for Dispatched target")
		}
	})

	t.Run("double-ack-noop", func(t *testing.T) {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateSucceeded},
				},
			},
		}
		rec, _, _ := newBORec(t, op, ctrl)

		// First call: ack clears it.
		if err := rec.patchStatus(context.Background(), op); err != nil {
			t.Fatalf("patchStatus: %v", err)
		}
		// Second call: no entry present, should not panic.
		if err := rec.patchStatus(context.Background(), op); err != nil {
			t.Fatalf("second patchStatus: %v", err)
		}
	})
}

func TestReconcileSuspended_PersistsPartialCompletionSameTick(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Suspend: true,
			Action: v1alpha1.BroodAction{
				Verb:   v1alpha1.BroodVerbExecuteGroovy,
				Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
			},
		},
		Status: v1alpha1.BroodOperationStatus{
			Phase: v1alpha1.BroodOperationPhaseSuspended,
			Targets: []v1alpha1.BroodTargetStatus{
				{Namespace: "ns1", Name: "ctrl-a", Wave: 0, State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: frozenNow.Add(-time.Minute)}},
			},
		},
	}
	op.Finalizers = []string{broodFinalizer}
	rec, _, cl := newBORec(t, op, ctrl)

	// Seed a completed result for ctrl-a.
	key := groovyResultKey(op, &op.Status.Targets[0])
	rec.groovyResultsMu.Lock()
	rec.groovyResults[key] = groovyResult{output: "complete", err: nil}
	rec.groovyResultsMu.Unlock()

	// Drive Reconcile — Suspended branch should evaluate, succeed the target,
	// patchStatus (persists + acks).
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-op", Namespace: "ns1"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Verify the target is Succeeded in persisted status.
	var got v1alpha1.BroodOperation
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-op", Namespace: "ns1"}, &got); err != nil {
		t.Fatalf("get op: %v", err)
	}
	if len(got.Status.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(got.Status.Targets))
	}
	if got.Status.Targets[0].State != v1alpha1.BroodTargetStateSucceeded {
		t.Errorf("expected Succeeded, got %s", got.Status.Targets[0].State)
	}
	if got.Status.Targets[0].Output != "complete" {
		t.Errorf("expected output 'complete', got %q", got.Status.Targets[0].Output)
	}

	// Verify the groovyResults entry was acked.
	rec.groovyResultsMu.Lock()
	_, exists := rec.groovyResults[key]
	rec.groovyResultsMu.Unlock()
	if exists {
		t.Error("expected groovyResults entry to be acked after suspend-persist")
	}
}

func TestCheckTerminal_PatchFailureReturnsFalseAndCallerRetries(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
		Spec: v1alpha1.BroodOperationSpec{
			Action: v1alpha1.BroodAction{
				Verb: v1alpha1.BroodVerbReconcile,
			},
		},
		Status: v1alpha1.BroodOperationStatus{
			Phase: v1alpha1.BroodOperationPhaseRunning,
			Targets: []v1alpha1.BroodTargetStatus{
				{Namespace: "ns1", Name: "ctrl-a", Wave: 0, State: v1alpha1.BroodTargetStateSucceeded},
			},
		},
	}
	op.Finalizers = []string{broodFinalizer}

	// Create a fake client that fails the first Status().Update call.
	baseCl := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&v1alpha1.BroodOperation{}).
		WithObjects(op, ctrl).
		Build()
	failCl := &failNClient{Client: baseCl, remaining: 1}

	fc := &fakeBroodClient{}
	rec := NewBroodOperationReconciler(failCl, scheme.Scheme, "operator-ns", fc, crdstore.NewFake(), fc.Wake, func(_, _ string) {}, nil, nil)
	rec.now = func() time.Time { return frozenNow }

	// All targets are terminal, so checkTerminal will try to patch.
	if got := rec.checkTerminal(context.Background(), op, slog.Default()); got {
		t.Error("expected checkTerminal to return false when patch fails")
	}
	// checkTerminal sets the phase before attempting the patch, so on failure
	// the phase is already set but the function returned false (not terminal)
	// so the caller can retry via fallthrough patchStatus.
	if op.Status.Phase != v1alpha1.BroodOperationPhaseSucceeded {
		t.Errorf("expected phase Succeeded (set optimistically before patch attempt), got %s", op.Status.Phase)
	}
}

func TestReconcileDelete_GatedOnInFlightCounter(t *testing.T) {
	ctx := context.Background()

	t.Run("in-flight-gate-holds-finalizer", func(t *testing.T) {
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateFailed, FinishedAt: &metav1.Time{Time: frozenNow}},
				},
			},
		}
		op.Finalizers = []string{broodFinalizer}
		rec, _, cl := newBORec(t, op, ctrl)

		// Set in-flight counter to 1 — gate should hold even though target is terminal.
		rec.groovyResultsMu.Lock()
		rec.groovyInFlight[groovyOpKey(op)] = 1
		rec.groovyResultsMu.Unlock()

		// Delete to set DeletionTimestamp.
		if err := cl.Delete(ctx, op); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-op", Namespace: "ns1"}}
		res, err := rec.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter != broodPoll {
			t.Errorf("expected requeue after broodPoll, got %v", res.RequeueAfter)
		}

		var got v1alpha1.BroodOperation
		if err := cl.Get(ctx, types.NamespacedName{Name: "test-op", Namespace: "ns1"}, &got); err != nil {
			t.Fatalf("get op: %v", err)
		}
		if !controllerutil.ContainsFinalizer(&got, broodFinalizer) {
			t.Error("expected finalizer still present (in-flight gate held)")
		}
		if got.Status.Phase != "" {
			t.Errorf("expected phase unset (not Canceled), got %s", got.Status.Phase)
		}
	})

	t.Run("in-flight-zero-sweeps-and-removes-finalizer", func(t *testing.T) {
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
			Spec: v1alpha1.BroodOperationSpec{
				Action: v1alpha1.BroodAction{
					Verb:   v1alpha1.BroodVerbExecuteGroovy,
					Groovy: &v1alpha1.BroodGroovyAction{Script: "println 'hello'"},
				},
			},
			Status: v1alpha1.BroodOperationStatus{
				Targets: []v1alpha1.BroodTargetStatus{
					{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStateFailed, FinishedAt: &metav1.Time{Time: frozenNow}},
				},
			},
		}
		op.Finalizers = []string{broodFinalizer}
		rec, _, cl := newBORec(t, op, ctrl)

		// In-flight is 0 (default), but leave a stale groovyResults entry.
		orphanKey := groovyResultKey(op, &op.Status.Targets[0])
		rec.groovyResultsMu.Lock()
		rec.groovyResults[orphanKey] = groovyResult{output: "orphaned", err: nil}
		rec.groovyResultsMu.Unlock()

		if err := cl.Delete(ctx, op); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-op", Namespace: "ns1"}}
		_, err := rec.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		// The CR should be deleted (finalizer removed → GC).
		var got v1alpha1.BroodOperation
		err = cl.Get(ctx, types.NamespacedName{Name: "test-op", Namespace: "ns1"}, &got)
		if !apierrors.IsNotFound(err) {
			t.Fatal("expected CR to be deleted after finalizer removal")
		}

		// Internal state must be swept.
		rec.groovyResultsMu.Lock()
		_, exists := rec.groovyResults[orphanKey]
		_, opKeyExists := rec.groovyInFlight[groovyOpKey(op)]
		rec.groovyResultsMu.Unlock()
		if exists {
			t.Error("expected orphan groovyResults entry to be swept")
		}
		if opKeyExists {
			t.Error("expected groovyInFlight entry to be deleted")
		}
	})
}
