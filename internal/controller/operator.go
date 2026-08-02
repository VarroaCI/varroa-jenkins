package controller

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// SetupWithManager registers this reconciler with a controller-runtime Manager.
// It watches Controller CRDs and enqueues reconcile requests on changes.
// The existing phase state machine (Pending→Provisioning→Running→Connected→Failed)
// becomes the Reconcile body.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&v1alpha1.Controller{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
			NeedLeaderElection:      ptr.To(false),
		}).
		WithEventFilter(predicate.Funcs{
			// Skip periodic status-only updates to avoid tight loops.
			UpdateFunc: func(e event.UpdateEvent) bool {
				if e.ObjectNew == nil || e.ObjectOld == nil {
					return true
				}
				// Only reconcile on spec or metadata changes; status-only
				// updates are triggered by our own patches and should not
				// cause a re-reconcile.
				oldCtrl, _ := e.ObjectOld.(*v1alpha1.Controller)
				newCtrl, _ := e.ObjectNew.(*v1alpha1.Controller)
				if oldCtrl == nil || newCtrl == nil {
					return true
				}
				if oldCtrl.Generation != newCtrl.Generation {
					return true
				}
				return annotationBumped(oldCtrl, newCtrl, annotationWakeRequested) ||
					annotationBumped(oldCtrl, newCtrl, annotationForceReprovision)
			},
		}).
		// On-demand reconcile triggers (BFF "reconcile"/"reprovision"/wake
		// relayed over NATS) enqueue events on this channel. The predicate above
		// only fires on generation changes, so without this an explicit reconcile
		// with no spec change would never run.
		WatchesRawSource(source.Channel(r.reconcileEvents, &handler.EnqueueRequestForObject{})).
		// React immediately when RBAC CRDs change: enqueue all Controllers so
		// the updated JenkinsRole/JenkinsRoleBinding is pushed live rather than
		// waiting for the next periodic tick.
		Watches(&v1alpha1.JenkinsRoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAllControllers)).
		Watches(&v1alpha1.JenkinsRole{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAllControllers)).
		// React immediately when a ControllerClass changes: re-enqueue only
		// Controllers referencing that class so class edits converge without
		// waiting for the next periodic tick.
		Watches(&v1alpha1.ControllerClass{}, handler.EnqueueRequestsFromMapFunc(r.enqueueControllersForClass)).
		Complete(r)
}

// enqueueAllControllers returns a reconcile.Request for every Controller CR
// in the cluster. Used as the watch handler for JenkinsRole/JenkinsRoleBinding
// so that any RBAC CRD change immediately triggers reconciliation of all
// controllers rather than waiting for the periodic tick.
func (r *Reconciler) enqueueAllControllers(ctx context.Context, _ client.Object) []reconcile.Request {
	controllers, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		r.Logger.Error("enqueueAllControllers: list failed", "error", err)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(controllers))
	for _, cr := range controllers {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace},
		})
	}
	return reqs
}

// enqueueControllersForClass re-enqueues only Controllers whose
// spec.className matches the changed ControllerClass's name — narrower
// than enqueueAllControllers (used for JenkinsRole, which affects every
// Controller) since most Controllers won't reference any given class.
func (r *Reconciler) enqueueControllersForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	class, ok := obj.(*v1alpha1.ControllerClass)
	if !ok {
		return nil
	}
	controllers, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range controllers {
		if c.Spec.ClassName == class.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name}})
		}
	}
	return reqs
}

// Reconcile implements the controller-runtime reconcile.Reconciler interface.
// It drives the phase state machine: Pending→Provisioning→Running→Connected→Failed.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Shard-ownership gate: if we don't own this controller, drop the request
	// requeue-free — the owning replica maintains the periodic cycle.
	if !r.ownsController(req.Namespace, req.Name) {
		return reconcile.Result{}, nil
	}

	// Fetch the controller CRD.
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, req.Name, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		r.Logger.Error("get controller failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return reconcile.Result{}, err
	}

	// One-shot force-reprovision: consume the annotation up front so no
	// early-return path in the phase machine can leave it armed (an armed
	// annotation re-forces pushes and checksum-gate bypasses every tick).
	// Clear-before-act: if the clear PATCH fails, requeue instead of acting —
	// the annotation survives and the rate-limited retry consumes it exactly
	// once. The in-memory copy on cr still carries the request through this
	// pass for the downstream readers. Only the phases that honor the request
	// (Provisioning's checksum-gate bypass, Connected's forced re-push)
	// consume it; in any other phase it stays armed so the request fires when
	// the controller reaches an honoring phase, as it did pre-#274.
	honorsForceReprovision := cr.Status.Phase == v1alpha1.ControllerPhaseProvisioning ||
		cr.Status.Phase == v1alpha1.ControllerPhaseConnected
	if cr.Annotations[annotationForceReprovision] != "" && honorsForceReprovision {
		if err := crdstore.PatchAnnotations[v1alpha1.Controller](ctx, r.store, cr.Name, cr.Namespace,
			map[string]*string{annotationForceReprovision: nil}); err != nil {
			r.Logger.Warn("failed to consume force-reprovision annotation, requeueing",
				"namespace", req.Namespace, "name", req.Name, "error", err)
			return reconcile.Result{}, err
		}
		// Clear the annotation in-memory too so that any subsequent crdstore.Apply
		// (e.g. from syncRBACBindings) does not re-store it.
		delete(cr.Annotations, annotationForceReprovision)
	}

	// Run the phase state machine.
	beingDeleted := !cr.DeletionTimestamp.IsZero()
	if err := r.reconcileController(ctx, cr); err != nil {
		r.Logger.Error("reconcile failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		// Return the error so controller-runtime applies rate-limited
		// retries. Do not patch status on failed reconciles.
		return reconcile.Result{}, err
	}

	// Patch finalizers if modified. When a controller is being deleted,
	// we must always patch — reconcileController removes the varroa
	// finalizer, and if it was the last one, the slice is empty and
	// would be skipped by the len > 0 check. A missing finalizer patch
	// leaves the CR stuck in Terminating.
	if len(cr.Finalizers) > 0 || (beingDeleted && len(cr.Finalizers) == 0) {
		if err := crdstore.PatchFinalizers[v1alpha1.Controller](ctx, r.store, cr.Name, cr.Namespace, cr.Finalizers); err != nil {
			r.Logger.Error("patch finalizers failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		}
	}

	// Patch status on every successful reconcile. reconcileController mutates
	// many status fields (MiteStatus, Conditions, hashes, timestamps, pending
	// restart) in addition to Phase. The controller-runtime generation filter
	// already prevents re-reconcile loops from status-only updates.
	if err := r.client.PatchControllerStatus(ctx, cr.Name, cr.Namespace, &cr.Status); err != nil {
		r.Logger.Error("patch status failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return reconcile.Result{}, err
	}

	// Requeue based on phase (with backoff + jitter).
	requeueAfter := r.nextRequeueDuration(cr)
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// nextRequeueDuration returns the duration before the next reconciliation,
// based on the controller's phase. Unlike the legacy nextSleepDuration, this
// never returns 0 — active phases get a minimum backoff with jitter.
func (r *Reconciler) nextRequeueDuration(cr *v1alpha1.Controller) time.Duration {
	const minBackoff = 1 * time.Second

	var base time.Duration
	switch cr.Status.Phase {
	case v1alpha1.ControllerPhaseFailed:
		base = 5 * time.Minute
	case v1alpha1.ControllerPhaseConnected:
		if cr.Status.FirstConnectedAt != nil {
			base = r.effectiveInterval(cr)
		} else {
			base = minBackoff
		}
	default:
		// Pending, Provisioning, Running: backoff instead of tight loop.
		base = minBackoff
	}

	// Add random jitter (±20%) to prevent thundering herd.
	jitterRange := time.Duration(int64(base) * 20 / 100)
	if jitterRange == 0 {
		jitterRange = 100 * time.Millisecond
	}
	jitter := time.Duration(rand.Int64N(int64(jitterRange)))
	return base + jitter
}

// Ensure Reconcile satisfies the controller-runtime interface.
var _ reconcile.Reconciler = (*Reconciler)(nil)

// annotationBumped returns true if the annotation value changed from one value
// to another non-empty value (or was set from empty to non-empty).
func annotationBumped(oldC, newC *v1alpha1.Controller, key string) bool {
	nv := newC.GetAnnotations()[key]
	return nv != "" && nv != oldC.GetAnnotations()[key]
}

// ---------------------------------------------------------------------------
// UpdateCenterReconciler registration
// ---------------------------------------------------------------------------

// ucTickerRunnable is a minimal manager.Runnable that periodically calls
// the UpdateCenterReconciler for the singleton UpdateCenter CR.
type ucTickerRunnable struct {
	rec      *UpdateCenterReconciler
	interval time.Duration
	logger   *slog.Logger
}

func (t *ucTickerRunnable) NeedLeaderElection() bool { return true }

func (t *ucTickerRunnable) Start(ctx context.Context) error {
	t.logger.Info("update-center reconciler started", "interval", t.interval)
	// Run immediately, then on tick.
	t.tick(ctx)
	timer := time.NewTimer(t.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			t.logger.Info("update-center reconciler stopped")
			return nil
		case <-timer.C:
			t.tick(ctx)
			timer.Reset(t.interval)
		}
	}
}

func (t *ucTickerRunnable) tick(ctx context.Context) {
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: updateCenterSingletonName},
	}
	if _, err := t.rec.Reconcile(ctx, req); err != nil {
		t.logger.Warn("update-center reconcile failed", "error", err)
	}
}

// SetupWithManager registers the UpdateCenterReconciler as a periodic
// ticker-based reconciler on the given manager. It mirrors the ticker
// pattern used by JenkinsVersionProfileReconciler.
func (r *UpdateCenterReconciler) SetupWithManager(mgr manager.Manager) error {
	return mgr.Add(&ucTickerRunnable{
		rec:      r,
		interval: 30 * time.Second,
		logger:   r.logger,
	})
}
