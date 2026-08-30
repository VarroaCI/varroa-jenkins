package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	"github.com/varroaci/varroa-jenkins/internal/mite"
)

const (
	broodFinalizer = "varroa.dev/broodop-finalizer"
	broodPoll      = 5 * time.Second
)

// provisioningDefaultsName is the cluster-scoped ProvisioningDefaults singleton.
// The object is optional: every reader must treat absence and read failure as
// "no defaults" rather than as an error.
const provisioningDefaultsName = "varroa-defaults"

// broodVerbTimeouts is the per-verb timeout map per D4.
var broodVerbTimeouts = map[v1alpha1.BroodVerb]time.Duration{
	v1alpha1.BroodVerbRestart:       15 * time.Minute,
	v1alpha1.BroodVerbReprovision:   30 * time.Minute,
	v1alpha1.BroodVerbReconcile:     5 * time.Minute,
	v1alpha1.BroodVerbStop:          10 * time.Minute,
	v1alpha1.BroodVerbStart:         20 * time.Minute,
	v1alpha1.BroodVerbExecuteGroovy: 5 * time.Minute,
}

// reprovisionObserveWindow is the time within which a reprovision target's
// phase must leave {Connected,Running} (3 minutes per D4).
const reprovisionObserveWindow = 3 * time.Minute

// reprovisionLeftMarker is stored in a Dispatched reprovision target's Reason
// once the phase is observed leaving {Connected,Running}; success requires it.
const reprovisionLeftMarker = "reprovision in progress"

// restartObserveWindow is the time within which a restart target's pod must be
// observed recreated (creationTimestamp after dispatch). A pod still older
// than the dispatch after this window means the delete never took effect.
const restartObserveWindow = 3 * time.Minute

const (
	groovyCallTimeout = 60 * time.Second // per-target Jenkins call deadline (goroutine ctx)
	groovyTokenTTL    = 2 * time.Minute  // operator JWT TTL: > call deadline, << 5m verb timeout
	groovyOutputCap   = 4096             // BroodTargetStatus.Output byte cap (UTF-8-safe)
	groovyReasonCap   = 256              // BroodTargetStatus.Reason byte cap on failure (UTF-8-safe)
)

// ResolvedTarget is the result of target resolution, used internally and
// returned by ResolveTargets for BFF preview reuse.
type ResolvedTarget struct {
	Namespace  string
	Name       string
	Wave       int32
	Applicable bool   // true = will be dispatched, false = skipped
	SkipReason string // set when !Applicable
	Controller *v1alpha1.Controller
}

type groovyResult struct {
	output string
	err    error
}

// groovyResultKey identifies one target's result: opNS/opName/targetNS/targetName.
func groovyResultKey(op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) string {
	return op.Namespace + "/" + op.Name + "/" + t.Namespace + "/" + t.Name
}

// groovyOpKey identifies one operation's in-flight goroutine counter: opNS/opName.
func groovyOpKey(op *v1alpha1.BroodOperation) string {
	return op.Namespace + "/" + op.Name
}

// truncateOutput caps s at maxBytes bytes on a UTF-8 rune boundary (never splits
// a multibyte rune). Returns s unchanged when it already fits.
func truncateOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// back off to a rune boundary at or before maxBytes
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// BroodOperationReconciler reconciles BroodOperation CRs.
type BroodOperationReconciler struct {
	client            client.Client
	scheme            *runtime.Scheme
	operatorNamespace string
	now               func() time.Time
	resourceClient    ResourceClient
	store             crdstore.Backend
	wakeFn            func(namespace, name string)
	reprovisionFn     func(namespace, name string)
	activityPublisher *activity.Publisher
	Logger            *slog.Logger

	// eventSink, when non-nil, is called instead of activityPublisher.Publish.
	// Used in tests to capture emitted events.
	eventSink func(activity.Event)

	groovyResults       map[string]groovyResult                                            // key: groovyResultKey
	groovyInFlight      map[string]int                                                     // key: groovyOpKey — in-flight goroutines per operation
	groovyResultsMu     sync.Mutex                                                         // guards groovyResults AND groovyInFlight
	operatorTokenSigner *mite.MiteTokenSigner                                              // mints the executeGroovy operator JWT; nil in tests that stub runGroovy
	runGroovy           func(ctx context.Context, ns, name, script string) (string, error) // seam over runGroovyOnTarget
}

// NewBroodOperationReconciler creates a new BroodOperationReconciler.
func NewBroodOperationReconciler(
	cl client.Client,
	scheme *runtime.Scheme,
	operatorNamespace string,
	resourceClient ResourceClient,
	store crdstore.Backend,
	wakeFn func(namespace, name string),
	reprovisionFn func(namespace, name string),
	activityPublisher *activity.Publisher,
	logger *slog.Logger,
) *BroodOperationReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	r := &BroodOperationReconciler{
		client:            cl,
		scheme:            scheme,
		operatorNamespace: operatorNamespace,
		now:               time.Now,
		resourceClient:    resourceClient,
		store:             store,
		wakeFn:            wakeFn,
		reprovisionFn:     reprovisionFn,
		activityPublisher: activityPublisher,
		Logger:            logger,
	}
	r.groovyResults = make(map[string]groovyResult)
	r.groovyInFlight = make(map[string]int)
	r.runGroovy = r.runGroovyOnTarget
	return r
}

// SetupWithManager registers this reconciler with a controller-runtime Manager.
func (r *BroodOperationReconciler) SetupWithManager(mgr manager.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&v1alpha1.BroodOperation{}).
		Complete(r)
}

// SetOperatorTokenSigner sets the RS256 signer used to mint the short-lived
// system:varroa-operator JWT for direct-to-Jenkins executeGroovy dispatch.
func (r *BroodOperationReconciler) SetOperatorTokenSigner(s *mite.MiteTokenSigner) {
	r.operatorTokenSigner = s
}

// Reconcile implements reconcile.Reconciler.
//
// NOTE: reconciliation performs no caller-identity/claims authorization for any
// verb, including executeGroovy — a BroodOperation created directly via kubectl/GitOps
// is gated ONLY by Kubernetes RBAC on broodoperations.varroa.dev. BFF-path authz
// (broodOpAccess in internal/api) is the sole Varroa authz layer and is verb-agnostic;
// executeGroovy's elevated Jenkins-side privilege introduces no new reconcile-time gate.
func (r *BroodOperationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("broodop", req.Namespace+"/"+req.Name)

	var op v1alpha1.BroodOperation
	if err := r.client.Get(ctx, req.NamespacedName, &op); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Handle deletion (finalizer-based cleanup).
	if !op.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &op, logger)
	}

	// Ensure the finalizer is present.
	if !controllerutil.ContainsFinalizer(&op, broodFinalizer) {
		controllerutil.AddFinalizer(&op, broodFinalizer)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh v1alpha1.BroodOperation
			if err := r.client.Get(ctx, req.NamespacedName, &fresh); err != nil {
				return err
			}
			if !controllerutil.AddFinalizer(&fresh, broodFinalizer) {
				return nil
			}
			return r.client.Update(ctx, &fresh)
		}); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: broodPoll}, nil
	}

	// Store current generation for observedGeneration.
	if op.Status.ObservedGeneration != op.Generation {
		op.Status.ObservedGeneration = op.Generation
	}

	switch op.Status.Phase {
	case "", v1alpha1.BroodOperationPhasePending:
		if err := r.reconcilePending(ctx, &op); err != nil {
			return reconcile.Result{}, err
		}
	case v1alpha1.BroodOperationPhaseRunning:
		if op.Spec.Suspend {
			op.Status.Phase = v1alpha1.BroodOperationPhaseSuspended
			if err := r.patchStatus(ctx, &op); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{RequeueAfter: broodPoll}, nil
		}
		if err := r.reconcileRunning(ctx, &op, logger); err != nil {
			return reconcile.Result{}, err
		}
	case v1alpha1.BroodOperationPhaseSuspended:
		if !op.Spec.Suspend {
			op.Status.Phase = v1alpha1.BroodOperationPhaseRunning
			if err := r.patchStatus(ctx, &op); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{RequeueAfter: broodPoll}, nil
		}
		// While suspended, still evaluate in-flight targets and persist (+ack) any
		// partial completion on the same tick — same shape as reconcileRunning, no dispatch.
		r.evaluateTargets(ctx, &op, logger)
		if r.checkTerminal(ctx, &op, logger) {
			return reconcile.Result{RequeueAfter: broodPoll}, nil
		}
		if err := r.patchStatus(ctx, &op); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: broodPoll}, nil

	case v1alpha1.BroodOperationPhaseSucceeded, v1alpha1.BroodOperationPhaseFailed, v1alpha1.BroodOperationPhaseCanceled:
		return r.reconcileTerminal(ctx, &op, logger)
	}

	// Non-terminal phases requeue with the poll interval.
	if op.Status.Phase == v1alpha1.BroodOperationPhaseRunning ||
		op.Status.Phase == v1alpha1.BroodOperationPhaseSuspended {
		return reconcile.Result{RequeueAfter: broodPoll}, nil
	}
	return reconcile.Result{}, nil
}

// reconcileDelete handles the deletion finalizer: stop dispatch, wait for
// Dispatched targets to reach terminal (bounded by per-verb timeout), mark
// remaining Pending as Skipped(canceled), set phase Canceled, remove finalizer.
func (r *BroodOperationReconciler) reconcileDelete(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(op, broodFinalizer) {
		return reconcile.Result{}, nil
	}

	// Evaluate targets one last time — NotFound detection and timeouts.
	r.evaluateTargets(ctx, op, logger)

	// Mark all Pending targets as Skipped(canceled) immediately — they must
	// never be dispatched on deletion.
	now := metav1.NewTime(r.now())
	for i := range op.Status.Targets {
		if op.Status.Targets[i].State == v1alpha1.BroodTargetStatePending {
			op.Status.Targets[i].State = v1alpha1.BroodTargetStateSkipped
			op.Status.Targets[i].Reason = "canceled"
			op.Status.Targets[i].FinishedAt = &now
			r.emitTargetFinished(op, &op.Status.Targets[i])
		}
	}

	hasDispatched := false
	for i := range op.Status.Targets {
		if op.Status.Targets[i].State == v1alpha1.BroodTargetStateDispatched {
			// Check per-verb timeout.
			if r.isTargetTimedOut(op, &op.Status.Targets[i]) {
				op.Status.Targets[i].State = v1alpha1.BroodTargetStateFailed
				op.Status.Targets[i].Reason = "canceled: timed out"
				now := metav1.NewTime(r.now())
				op.Status.Targets[i].FinishedAt = &now
				r.emitTargetFinished(op, &op.Status.Targets[i])
				continue // settled this pass — don't hold the finalizer another poll
			}
			hasDispatched = true
		}
	}

	if hasDispatched {
		// Still waiting — requeue for bounded settle.
		if err := r.patchStatus(ctx, op); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: broodPoll}, nil
	}

	// Gate deletion on in-flight executeGroovy goroutines. A goroutine decrements
	// its counter only AFTER writing its groovyResults entry (§6.4), so proceeding
	// only at zero guarantees no goroutine can write into the map after the sweep
	// (and finalizer removal) below — closing the late-writer leak, not just narrowing
	// it. Costs up to groovyCallTimeout (60s) extra deletion latency worst case.
	if op.Spec.Action.Verb == v1alpha1.BroodVerbExecuteGroovy {
		r.groovyResultsMu.Lock()
		inFlight := r.groovyInFlight[groovyOpKey(op)]
		r.groovyResultsMu.Unlock()
		if inFlight > 0 {
			if err := r.patchStatus(ctx, op); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{RequeueAfter: broodPoll}, nil
		}
	}

	op.Status.Phase = v1alpha1.BroodOperationPhaseCanceled
	op.Status.FinishedAt = &now
	r.updateSummary(op)
	r.emitRunFinished(op)

	if op.Spec.Action.Verb == v1alpha1.BroodVerbExecuteGroovy {
		r.groovyResultsMu.Lock()
		for i := range op.Status.Targets {
			delete(r.groovyResults, groovyResultKey(op, &op.Status.Targets[i]))
		}
		delete(r.groovyInFlight, groovyOpKey(op))
		r.groovyResultsMu.Unlock()
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh v1alpha1.BroodOperation
		if err := r.client.Get(ctx, client.ObjectKeyFromObject(op), &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !controllerutil.RemoveFinalizer(&fresh, broodFinalizer) {
			return nil
		}
		return r.client.Update(ctx, &fresh)
	}); err != nil {
		return reconcile.Result{}, err
	}
	// CR may be auto-deleted by the fake client (or real GC) after the finalizer
	// is removed while DeletionTimestamp is set. Ignore NotFound on the status
	// patch in that case.
	if err := r.patchStatus(ctx, op); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// reconcilePending implements the Pending→Running transition (TASK 3.3).
func (r *BroodOperationReconciler) reconcilePending(ctx context.Context, op *v1alpha1.BroodOperation) error {
	// Re-validate tenancy (authoritative — catches direct kubectl creates).
	if err := r.validateTenancy(op); err != nil {
		op.Status.Phase = v1alpha1.BroodOperationPhaseFailed
		op.Status.Reason = "TenancyViolation"
		now := metav1.NewTime(r.now())
		op.Status.FinishedAt = &now
		r.updateSummary(op)
		r.emitRunFinished(op)
		return r.patchStatus(ctx, op)
	}

	// Verb policy gate. This is THE enforcement point: kubectl and GitOps create
	// BroodOperation objects directly and never traverse the BFF, so the BFF's
	// matching check is UX only. It runs before target resolution so a denied
	// operation never reads the brood.
	if allowed, why := r.verbPolicyAllows(ctx, op); !allowed {
		op.Status.Phase = v1alpha1.BroodOperationPhaseFailed
		op.Status.Reason = why
		now := metav1.NewTime(r.now())
		op.Status.FinishedAt = &now
		r.updateSummary(op)
		r.emitRunFinished(op)
		return r.patchStatus(ctx, op)
	}

	// Resolve targets.
	targets, err := ResolveTargets(ctx, r.client, op.Spec, op.Namespace, r.operatorNamespace)
	if err != nil {
		op.Status.Reason = err.Error()
		op.Status.Phase = v1alpha1.BroodOperationPhaseFailed
		now := metav1.NewTime(r.now())
		op.Status.FinishedAt = &now
		r.updateSummary(op)
		r.emitRunFinished(op)
		return r.patchStatus(ctx, op)
	}

	// Build snapshot.
	statusTargets := make([]v1alpha1.BroodTargetStatus, 0, len(targets))
	for _, t := range targets {
		ts := v1alpha1.BroodTargetStatus{
			Namespace: t.Namespace,
			Name:      t.Name,
			Wave:      t.Wave,
			State:     v1alpha1.BroodTargetStatePending,
		}
		if !t.Applicable {
			ts.State = v1alpha1.BroodTargetStateSkipped
			ts.Reason = t.SkipReason
		}
		statusTargets = append(statusTargets, ts)
	}

	op.Status.Targets = statusTargets
	r.updateSummary(op)

	// Zero dispatchable targets → immediate Succeeded.
	if op.Status.Summary.Total == op.Status.Summary.Skipped {
		op.Status.Phase = v1alpha1.BroodOperationPhaseSucceeded
		now := metav1.NewTime(r.now())
		op.Status.FinishedAt = &now
		r.emitRunFinished(op)
		return r.patchStatus(ctx, op)
	}

	// Transition to Running.
	op.Status.Phase = v1alpha1.BroodOperationPhaseRunning
	now := metav1.NewTime(r.now())
	op.Status.StartedAt = &now
	r.emitRunStarted(op)
	return r.patchStatus(ctx, op)
}

// reconcileRunning runs the dispatch loop and evaluates targets (TASK 3.4, 3.5, 3.6).
func (r *BroodOperationReconciler) reconcileRunning(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) error {
	// Phase 1: evaluate existing targets (completion predicates, delete detection, timeouts).
	//
	// This runs BEFORE the policy gate below on purpose. Targets already
	// dispatched have an HTTP call in flight and still need their completion
	// and timeout predicates evaluated; skipping straight to cancellation would
	// strand them Dispatched forever, which is a worse failure than the one the
	// gate exists to prevent.
	r.evaluateTargets(ctx, op, logger)

	// Phase 2: check if terminal after evaluation.
	if r.checkTerminal(ctx, op, logger) {
		return nil
	}

	// Policy can be revoked mid-run. Checking only at the Pending transition
	// meant a 100-target operation that started while allowed would execute all
	// 100 even after an admin set enabled:false — the switch would not switch
	// anything off. Re-check every tick and stop dispatching, exactly as the
	// fail-tidy path does: already-dispatched targets cannot be recalled, but
	// nothing new goes out.
	if allowed, why := r.verbPolicyAllows(ctx, op); !allowed {
		r.applyFailTidyPending(op, "canceled: "+why)
		op.Status.Reason = why
		if r.checkTerminal(ctx, op, logger) {
			return nil
		}
		return r.patchStatus(ctx, op)
	}

	// Phase 3: dispatch loop.
	r.dispatchLoop(ctx, op, logger)

	// Phase 4: check if terminal after dispatch (failure policies may have
	// marked all targets terminal).
	if r.checkTerminal(ctx, op, logger) {
		return nil
	}

	return r.patchStatus(ctx, op)
}

// evaluateTargets re-evaluates every non-terminal target using per-verb
// completion predicates (TASK 3.5) and delete detection.
func (r *BroodOperationReconciler) evaluateTargets(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) {
	verb := op.Spec.Action.Verb
	timeout := broodVerbTimeouts[verb]
	now := r.now()

	for i := range op.Status.Targets {
		t := &op.Status.Targets[i]

		// Skip already-terminal targets.
		if t.State != v1alpha1.BroodTargetStatePending && t.State != v1alpha1.BroodTargetStateDispatched {
			continue
		}

		// Re-get the Controller CR.
		var ctrl v1alpha1.Controller
		err := r.client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, &ctrl)

		if apierrors.IsNotFound(err) {
			t.State = v1alpha1.BroodTargetStateFailed
			t.Reason = "controller deleted"
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
			continue
		}
		if err != nil {
			logger.Warn("failed to get controller for evaluation",
				"controller", t.Namespace+"/"+t.Name, "error", err)
			continue
		}

		if t.State == v1alpha1.BroodTargetStatePending {
			continue // not dispatched yet, nothing to evaluate
		}

		// Dispatched — evaluate per-verb predicate.
		r.evaluateDispatchedTarget(ctx, op, t, &ctrl, verb, timeout, now)
	}
}

// evaluateDispatchedTarget runs the per-verb completion predicate (D4).
func (r *BroodOperationReconciler) evaluateDispatchedTarget(
	ctx context.Context,
	op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus,
	ctrl *v1alpha1.Controller, verb v1alpha1.BroodVerb,
	timeout time.Duration, now time.Time,
) {
	// Check per-verb timeout first.
	dispatchedAt := t.DispatchedAt
	if dispatchedAt != nil && now.Sub(dispatchedAt.Time) > timeout {
		if verb == v1alpha1.BroodVerbExecuteGroovy {
			r.groovyResultsMu.Lock()
			delete(r.groovyResults, groovyResultKey(op, t))
			r.groovyResultsMu.Unlock()
		}
		t.State = v1alpha1.BroodTargetStateFailed
		t.Reason = timeoutReason(verb)
		finished := metav1.NewTime(now)
		t.FinishedAt = &finished
		r.emitTargetFinished(op, t)
		return
	}

	switch verb {
	case v1alpha1.BroodVerbRestart:
		// Restart is an operator-driven pod delete (same mechanism as the
		// single-controller restart; the mite has no SAFE_RESTART handler).
		// Success evidence is the recreated pod being
		// Ready: creationTimestamp after dispatch AND kubelet readiness
		// (Jenkins actually serving). Controller phase is deliberately NOT
		// part of the predicate — it lags the reconciler tick in both
		// directions: a fast bounce may never be observed off Connected,
		// and a stale Connected right after the delete would open the next
		// wave while Jenkins is still booting.
		pod, err := r.resourceClient.GetControllerPod(ctx, t.Namespace, t.Name)
		if err != nil {
			return // transient lookup failure — retry next poll
		}
		recreated := pod != nil && dispatchedAt != nil && pod.CreationTimestamp.After(dispatchedAt.Time)
		if recreated && isPodReady(pod) {
			t.State = v1alpha1.BroodTargetStateSucceeded
			t.Reason = ""
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
			return
		}
		// Old pod still standing past the observe window: the delete never
		// took effect. (A nil pod means the delete worked and the
		// StatefulSet is still recreating — keep waiting on the verb
		// timeout.)
		if !recreated && pod != nil &&
			dispatchedAt != nil && now.Sub(dispatchedAt.Time) > restartObserveWindow {
			t.State = v1alpha1.BroodTargetStateFailed
			t.Reason = "restart not observed"
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		}

	case v1alpha1.BroodVerbReprovision:
		// Phase must be observed leaving {Connected,Running} within 3m of
		// dispatch (the "never-left" check), then return to Connected. The
		// departure is recorded as a transient Reason marker on the
		// Dispatched target so a still-Connected first poll never counts as
		// success.
		left := t.Reason == reprovisionLeftMarker
		if !left && ctrl.Status.Phase != v1alpha1.ControllerPhaseConnected && ctrl.Status.Phase != v1alpha1.ControllerPhaseRunning {
			t.Reason = reprovisionLeftMarker
			left = true
		}
		if !left {
			if dispatchedAt != nil && now.Sub(dispatchedAt.Time) > reprovisionObserveWindow {
				t.State = v1alpha1.BroodTargetStateFailed
				t.Reason = "reprovision not observed"
				finished := metav1.NewTime(now)
				t.FinishedAt = &finished
				r.emitTargetFinished(op, t)
			}
			return
		}
		// Success: returned to Connected after leaving.
		if ctrl.Status.Phase == v1alpha1.ControllerPhaseConnected {
			t.State = v1alpha1.BroodTargetStateSucceeded
			t.Reason = ""
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		}

	case v1alpha1.BroodVerbReconcile:
		// Success evidence wins the race with a controller transitioning to a
		// non-running phase in the same observation.
		if ctrl.Status.LastReconciledAt != nil && dispatchedAt != nil &&
			ctrl.Status.LastReconciledAt.After(dispatchedAt.Time) {
			t.State = v1alpha1.BroodTargetStateSucceeded
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		} else {
			switch ctrl.Status.Phase {
			case v1alpha1.ControllerPhaseHibernated:
				t.State = v1alpha1.BroodTargetStateSkipped
				t.Reason = "target hibernated during operation"
			case v1alpha1.ControllerPhaseStopped:
				t.State = v1alpha1.BroodTargetStateSkipped
				t.Reason = "target stopped during operation"
			default:
				return
			}
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		}

	case v1alpha1.BroodVerbStop:
		if ctrl.Status.Phase == v1alpha1.ControllerPhaseStopped {
			t.State = v1alpha1.BroodTargetStateSucceeded
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		}

	case v1alpha1.BroodVerbStart:
		if ctrl.Status.Phase == v1alpha1.ControllerPhaseConnected {
			t.State = v1alpha1.BroodTargetStateSucceeded
			finished := metav1.NewTime(now)
			t.FinishedAt = &finished
			r.emitTargetFinished(op, t)
		}

	case v1alpha1.BroodVerbExecuteGroovy:
		key := groovyResultKey(op, t)
		r.groovyResultsMu.Lock()
		res, ok := r.groovyResults[key] // PEEK only — never delete here
		r.groovyResultsMu.Unlock()
		if !ok {
			return // goroutine hasn't completed; the next 5s poll re-checks
		}
		finished := metav1.NewTime(now)
		t.FinishedAt = &finished
		if res.err != nil {
			t.State = v1alpha1.BroodTargetStateFailed
			t.Reason = truncateOutput(res.err.Error(), groovyReasonCap)
			t.Output = truncateOutput(res.output, groovyOutputCap)
		} else {
			t.State = v1alpha1.BroodTargetStateSucceeded
			t.Reason = ""
			t.Output = truncateOutput(res.output, groovyOutputCap)
		}
		r.emitTargetFinished(op, t)
		return
	}
}

// dispatchLoop dispatches new targets subject to wave gate, maxParallel,
// failure policy, and suspend (TASK 3.4).
func (r *BroodOperationReconciler) dispatchLoop(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) {
	if op.Spec.Suspend {
		return
	}

	maxParallel := int32(1)
	if exec := op.Spec.Execution; exec != nil && exec.MaxParallel != nil && *exec.MaxParallel > 0 {
		maxParallel = *exec.MaxParallel
	}

	// Count in-flight (Dispatched).
	inFlight := int32(0)
	hasFailed := false
	for i := range op.Status.Targets {
		switch op.Status.Targets[i].State {
		case v1alpha1.BroodTargetStateDispatched:
			inFlight++
		case v1alpha1.BroodTargetStateFailed:
			hasFailed = true
		}
	}

	// Failure policy gates.
	if hasFailed {
		policy := v1alpha1.BroodFailurePolicyFailTidy // default per design
		if op.Spec.Execution != nil && op.Spec.Execution.FailurePolicy != "" {
			policy = op.Spec.Execution.FailurePolicy
		}
		switch policy {
		case v1alpha1.BroodFailurePolicyFailFast:
			r.applyFailFast(op)
			return
		case v1alpha1.BroodFailurePolicyFailTidy:
			// No new dispatch on failure (fail-tidy).
			r.applyFailTidyPending(op, "canceled")
			return
		case v1alpha1.BroodFailurePolicyFailAtEnd:
			// Continue dispatching — failures don't gate.
		}
	}

	// Find currentWave: min wave with any Pending or Dispatched target.
	currentWave := int32(math.MaxInt32)
	for _, t := range op.Status.Targets {
		if t.State == v1alpha1.BroodTargetStatePending || t.State == v1alpha1.BroodTargetStateDispatched {
			if t.Wave < currentWave {
				currentWave = t.Wave
			}
		}
	}
	if currentWave == math.MaxInt32 {
		return // all terminal
	}

	// Dispatch pending targets in current wave up to maxParallel.
	for i := range op.Status.Targets {
		if inFlight >= maxParallel {
			break
		}
		t := &op.Status.Targets[i]
		if t.State != v1alpha1.BroodTargetStatePending {
			continue
		}
		if t.Wave > currentWave {
			continue // wave gate: N+1 not yet open
		}

		// Write-before-send: mark Dispatched.
		t.State = v1alpha1.BroodTargetStateDispatched
		dispatchedAt := metav1.NewTime(r.now())
		t.DispatchedAt = &dispatchedAt

		// Write status first.
		if err := r.patchStatus(ctx, op); err != nil {
			logger.Warn("status write failed before dispatch, aborting loop",
				"target", t.Namespace+"/"+t.Name, "error", err)
			// Revert to Pending since we couldn't persist.
			t.State = v1alpha1.BroodTargetStatePending
			t.DispatchedAt = nil
			return
		}

		// Now dispatch (write-before-send).
		if err := r.dispatchTarget(ctx, op, t); err != nil {
			logger.Warn("dispatch failed, target will timeout",
				"target", t.Namespace+"/"+t.Name, "error", err)
			// The target stays Dispatched in status; the predicate will
			// never match because the command was never sent, so it times out.
		}

		inFlight++
	}
}

// dispatchTarget sends the actual command per verb (D5).
func (r *BroodOperationReconciler) dispatchTarget(ctx context.Context, op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) error {
	verb := op.Spec.Action.Verb
	ns := t.Namespace
	name := t.Name

	switch verb {
	case v1alpha1.BroodVerbRestart:
		// Operator-driven pod delete, same as the single-controller restart.
		// The StatefulSet recreates the pod; the mite-owned SIGTERM drain
		// quiets Jenkins down within the pod's termination grace.
		return r.resourceClient.DeleteControllerPod(ctx, ns, name)

	case v1alpha1.BroodVerbReconcile:
		// Enqueue a reconcile event for the target controller.
		r.wakeFn(ns, name)
		return nil

	case v1alpha1.BroodVerbStop:
		// ApplyControllerSpecSSAIfExists performs its own GET immediately before
		// applying and never creates the object, so a target deleted between
		// ResolveTargets and dispatch fails here (NotFound) rather than being
		// resurrected as a phantom Controller carrying only spec.powerState. A
		// separate Get-then-Apply here would reopen that race: the GET this
		// existence guard relies on must be the one taken immediately before
		// the write, not an earlier one from a different client.
		//
		// Apply ONLY spec.powerState via server-side apply as varroa-ui, then
		// clear status.hibernated as part of the same dispatch. The two are
		// separate writes — spec and status are different subresources — and
		// the clear goes through SetHibernated because the flag is excluded
		// from the end-of-reconcile status patch.
		if _, _, err := r.resourceClient.ApplyControllerSpecSSAIfExists(ctx, ns, name, map[string]any{"powerState": "Stopped"}, "varroa-ui", false); err != nil {
			return fmt.Errorf("stop controller: %w", err)
		}
		if _, err := r.resourceClient.SetHibernated(ctx, name, ns, false); err != nil {
			return fmt.Errorf("clear hibernation on stop: %w", err)
		}
		return nil

	case v1alpha1.BroodVerbStart:
		// Same existence guard as Stop: a missing or concurrently-deleted
		// controller must fail the dispatch, never be created by the SSA below.
		if _, _, err := r.resourceClient.ApplyControllerSpecSSAIfExists(ctx, ns, name, map[string]any{"powerState": "Running"}, "varroa-ui", false); err != nil {
			return fmt.Errorf("start controller: %w", err)
		}
		if _, err := r.resourceClient.SetHibernated(ctx, name, ns, false); err != nil {
			return fmt.Errorf("clear hibernation on start: %w", err)
		}
		return nil

	case v1alpha1.BroodVerbReprovision:
		r.reprovisionTarget(ns, name)
		return nil

	case v1alpha1.BroodVerbExecuteGroovy:
		script, err := r.resolveGroovyScript(ctx, op, t)
		if err != nil {
			// Resolution failure: leave the target Dispatched with NO goroutine and
			// NO map entry. It resolves via the standard 5-minute verb timeout, exactly
			// like a goroutine that never completes — no resolution-specific state.
			r.Logger.Warn("executeGroovy script resolution failed; target will resolve via timeout",
				"operation", op.Name, "namespace", op.Namespace, "target", name, "error", err)
			return nil //nolint:nilerr // intentional: an unresolvable script leaves the target Dispatched to time out, not a dispatch error
		}
		key := groovyResultKey(op, t)
		opKey := groovyOpKey(op)
		r.groovyResultsMu.Lock()
		r.groovyInFlight[opKey]++ // reconcileDelete (7.4) gates its sweep on this being 0
		r.groovyResultsMu.Unlock()
		go func() {
			jenkinsCtx, cancel := context.WithTimeout(context.Background(), groovyCallTimeout)
			defer cancel()
			var output string
			var execErr error
			// Record the result and decrement in a defer so a panic in runGroovy (e.g. an
			// unexpectedly-nil operatorTokenSigner) can never skip the decrement — otherwise
			// groovyInFlight would stay >0 forever and permanently gate reconcileDelete's
			// finalizer removal. A recovered panic is surfaced as the target's failure reason.
			defer func() {
				if rec := recover(); rec != nil {
					execErr = fmt.Errorf("executeGroovy dispatch panicked: %v", rec)
				}
				r.groovyResultsMu.Lock()
				r.groovyResults[key] = groovyResult{output: output, err: execErr}
				r.groovyInFlight[opKey]-- // unconditional: success, error, AND panic flow through here
				r.groovyResultsMu.Unlock()
			}()
			output, execErr = r.runGroovy(jenkinsCtx, ns, name, script)
		}()
		return nil
	}
	return nil
}

// reprovisionTarget calls the reprovision function or falls back to wakeFn.
func (r *BroodOperationReconciler) reprovisionTarget(ns, name string) {
	if r.reprovisionFn != nil {
		r.reprovisionFn(ns, name)
	} else {
		// Fallback: just wake the controller.
		r.wakeFn(ns, name)
	}
}

// checkTerminal checks if all targets are terminal and transitions to
// Succeeded or Failed. Returns true if the operation became terminal.
func (r *BroodOperationReconciler) checkTerminal(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) bool {
	r.updateSummary(op)
	if op.Status.Summary.Total == 0 {
		return false
	}
	for _, t := range op.Status.Targets {
		if t.State == v1alpha1.BroodTargetStatePending || t.State == v1alpha1.BroodTargetStateDispatched {
			return false
		}
	}

	// All terminal.
	now := metav1.NewTime(r.now())
	op.Status.FinishedAt = &now
	r.updateSummary(op)

	if op.Status.Summary.Failed > 0 {
		op.Status.Phase = v1alpha1.BroodOperationPhaseFailed
	} else {
		op.Status.Phase = v1alpha1.BroodOperationPhaseSucceeded
	}

	r.emitRunFinished(op)
	if err := r.patchStatus(ctx, op); err != nil {
		logger.Warn("failed to patch terminal status", "error", err)
		return false // not durably terminal — let the caller's fallthrough patch retry this tick
	}
	return true
}

// applyFailFast marks all Pending targets as Skipped(canceled) and all
// Dispatched targets as Failed(abandoned (FailFast)).
func (r *BroodOperationReconciler) applyFailFast(op *v1alpha1.BroodOperation) {
	now := metav1.NewTime(r.now())
	for i := range op.Status.Targets {
		switch op.Status.Targets[i].State {
		case v1alpha1.BroodTargetStatePending:
			op.Status.Targets[i].State = v1alpha1.BroodTargetStateSkipped
			op.Status.Targets[i].Reason = "canceled"
			op.Status.Targets[i].FinishedAt = &now
			r.emitTargetFinished(op, &op.Status.Targets[i])
		case v1alpha1.BroodTargetStateDispatched:
			op.Status.Targets[i].State = v1alpha1.BroodTargetStateFailed
			op.Status.Targets[i].Reason = "abandoned (FailFast)"
			op.Status.Targets[i].FinishedAt = &now
			r.emitTargetFinished(op, &op.Status.Targets[i])
		}
	}
}

// applyFailTidyPending marks Pending targets as Skipped(canceled) but leaves
// Dispatched targets to complete naturally (or time out).
func (r *BroodOperationReconciler) applyFailTidyPending(op *v1alpha1.BroodOperation, reason string) {
	if reason == "" {
		reason = "canceled"
	}
	now := metav1.NewTime(r.now())
	for i := range op.Status.Targets {
		if op.Status.Targets[i].State == v1alpha1.BroodTargetStatePending {
			op.Status.Targets[i].State = v1alpha1.BroodTargetStateSkipped
			op.Status.Targets[i].Reason = reason
			op.Status.Targets[i].FinishedAt = &now
			r.emitTargetFinished(op, &op.Status.Targets[i])
		}
	}
}

// reconcileTerminal handles terminal-phase CRs for TTL GC (TASK 3.8).
func (r *BroodOperationReconciler) reconcileTerminal(ctx context.Context, op *v1alpha1.BroodOperation, logger *slog.Logger) (reconcile.Result, error) {
	ttl := int32(604800) // default 7 days
	if op.Spec.TTLSecondsAfterFinished != nil && *op.Spec.TTLSecondsAfterFinished >= 0 {
		ttl = *op.Spec.TTLSecondsAfterFinished
	}
	if ttl == 0 {
		// Immediate deletion.
		logger.Info("deleting terminal brood operation (TTL=0)")
		if err := r.client.Delete(ctx, op); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	elapsed := r.now().Sub(op.Status.FinishedAt.Time)
	if elapsed >= time.Duration(ttl)*time.Second {
		logger.Info("deleting expired brood operation",
			"ttl", ttl, "elapsed", elapsed)
		if err := r.client.Delete(ctx, op); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	requeueAfter := time.Duration(ttl)*time.Second - elapsed
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// validateTenancy validates tenancy rules per D2/D3.
// verbPolicyAllows evaluates ProvisioningDefaults.broodPolicy against this
// operation's verb. It returns (true, "") for every verb the policy does not
// govern, and for every failure to read the policy.
//
// Failing open on a read error is deliberate. ProvisioningDefaults is an
// optional singleton whose absence already means "no policy"; a transient API
// error is indistinguishable from absence at this layer, and failing closed
// would turn an apiserver blip into a cluster-wide outage of a verb that is
// enabled by default. The gate exists to enforce an operator's explicit
// decision, not to be a second authorization layer — Varroa RBAC is that, and it
// has already run by the time a BroodOperation object exists.
func (r *BroodOperationReconciler) verbPolicyAllows(ctx context.Context, op *v1alpha1.BroodOperation) (bool, string) {
	if op.Spec.Action.Verb != v1alpha1.BroodVerbExecuteGroovy {
		return true, ""
	}
	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil || defaults == nil {
		return true, ""
	}
	return defaults.Spec.BroodPolicy.ExecuteGroovyAllowed(op.Namespace)
}

func (r *BroodOperationReconciler) validateTenancy(op *v1alpha1.BroodOperation) error {
	return ValidateBroodTenancy(op.Spec, op.Namespace, r.operatorNamespace)
}

// ValidateBroodTenancy checks the targeting-mode rules (D2) for a spec created
// in crNamespace. Shared by the executor, ResolveTargets, and the BFF create
// pre-check.
func ValidateBroodTenancy(spec v1alpha1.BroodOperationSpec, crNamespace, operatorNamespace string) error {
	isOperatorNS := crNamespace == operatorNamespace

	if !isOperatorNS {
		// Team mode: namespaces must not be set; names are bare.
		if len(spec.Targets.Namespaces) > 0 {
			return fmt.Errorf("namespaces not allowed in team namespace")
		}
		// Names must be bare (no "/").
		if spec.Targets.Selector == nil {
			for _, n := range spec.Targets.Names {
				if strings.Contains(n, "/") {
					return fmt.Errorf("qualified name %q not allowed in team namespace", n)
				}
			}
		}
	} else {
		// Operator mode.
		if spec.Targets.Selector != nil {
			// Selector mode requires namespaces.
			if len(spec.Targets.Namespaces) == 0 {
				return fmt.Errorf("namespaces required with selector in operator namespace")
			}
		} else {
			// Names mode: must be "ns/name" qualified.
			for _, n := range spec.Targets.Names {
				if !strings.Contains(n, "/") {
					return fmt.Errorf("unqualified name %q not allowed in operator namespace", n)
				}
			}
		}
	}
	return nil
}

// patchStatus performs a conflict-safe full status replacement. The operator is
// the sole status writer, so replacing the status from a fresh object preserves
// intentional clears of omitempty fields such as Reason.
func (r *BroodOperationReconciler) patchStatus(ctx context.Context, op *v1alpha1.BroodOperation) error {
	desired := op.Status.DeepCopy()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh v1alpha1.BroodOperation
		if err := r.client.Get(ctx, client.ObjectKeyFromObject(op), &fresh); err != nil {
			return err
		}
		fresh.Status = *desired.DeepCopy()
		return r.client.Status().Update(ctx, &fresh)
	})
	if err != nil {
		return err
	}
	r.ackGroovyResults(op)
	return nil
}

// ackGroovyResults, after a target's terminal state is durably persisted, deletes
// its in-memory groovyResults entry. Only runs for executeGroovy. This is the single
// hook every patchStatus call site picks up — no per-call-site enumeration. A failed
// Status().Update never reaches here, so a result is only dropped once durably recorded
// (the peek/ack contract: a failed patch leaves the entry for the next tick's re-peek).
func (r *BroodOperationReconciler) ackGroovyResults(op *v1alpha1.BroodOperation) {
	if op.Spec.Action.Verb != v1alpha1.BroodVerbExecuteGroovy {
		return
	}
	r.groovyResultsMu.Lock()
	defer r.groovyResultsMu.Unlock()
	for i := range op.Status.Targets {
		t := &op.Status.Targets[i]
		if t.State == v1alpha1.BroodTargetStateSucceeded || t.State == v1alpha1.BroodTargetStateFailed {
			delete(r.groovyResults, groovyResultKey(op, t))
		}
	}
}

// updateSummary recomputes the summary from status.targets.
func (r *BroodOperationReconciler) updateSummary(op *v1alpha1.BroodOperation) {
	total := len(op.Status.Targets)
	succeeded := 0
	failed := 0
	skipped := 0
	for _, t := range op.Status.Targets {
		switch t.State {
		case v1alpha1.BroodTargetStateSucceeded:
			succeeded++
		case v1alpha1.BroodTargetStateFailed:
			failed++
		case v1alpha1.BroodTargetStateSkipped:
			skipped++
		}
	}
	// Total = all resolved targets (Pending + Dispatched + Succeeded + Failed + Skipped)
	op.Status.Summary = v1alpha1.BroodSummary{
		Total:     total,
		Succeeded: succeeded,
		Failed:    failed,
		Skipped:   skipped,
	}
}

// resolveGroovyScript resolves the Groovy source for an executeGroovy target: either
// the inline script verbatim, or a catalog-sourced script materialized once into a
// deterministically-named, owner-referenced ConfigMap snapshot and reused byte-identical
// by every subsequent target and reconcile. This whole sequence runs synchronously,
// before any async HTTP call is fired. The t parameter matches the per-target
// dispatch signature; the snapshot is per-operation, so it is intentionally unused.
//
//nolint:unparam // t retained for per-target dispatch symmetry; snapshot is per-operation
func (r *BroodOperationReconciler) resolveGroovyScript(ctx context.Context, op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) (string, error) {
	groovy := op.Spec.Action.Groovy

	if groovy.Script != "" {
		return groovy.Script, nil
	}

	ref := groovy.ItemRef
	if ref == nil {
		// Defense in depth: the CEL exactly-one-of rule uses has(self.script), which treats
		// an explicitly-submitted script:"" as present, so an object with an empty script and
		// no itemRef can pass admission. Fail resolution gracefully (the target stays
		// Dispatched and resolves via the verb timeout) rather than nil-dereferencing ref.
		return "", fmt.Errorf("executeGroovy: empty script and no itemRef to resolve")
	}
	cmName := groovyScriptConfigMapName(op)

	// Idempotent-read-first: if a snapshot already exists (from an earlier target in this
	// reconcile, or a prior reconcile that crashed before persisting
	// op.Status.ScriptSnapshotRef), reuse it verbatim rather than re-resolving from the
	// catalog. This does NOT require op.Status.ScriptSnapshotRef to have been durably
	// persisted — it checks the ConfigMap's existence by name directly.
	existing, readErr := r.readScriptConfigMap(ctx, op, cmName)
	if readErr == nil {
		op.Status.ScriptSnapshotRef = cmName
		return existing, nil
	}
	if !apierrors.IsNotFound(readErr) {
		return "", fmt.Errorf("read groovy script snapshot %s/%s: %w", op.Namespace, cmName, readErr)
	}

	// Local-first, operator-namespace-fallback lookup, with a correction to use crdstore, not r.client.
	var item *v1alpha1.CatalogItem
	var err error
	if ref.Namespace != "" {
		item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, ref.Namespace)
	} else {
		item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, op.Namespace)
		if (err != nil || item == nil) && r.operatorNamespace != "" && r.operatorNamespace != op.Namespace {
			item, err = crdstore.Get[v1alpha1.CatalogItem](ctx, r.store, ref.Name, r.operatorNamespace)
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve groovy catalog item %s: %w", ref.Name, err)
	}
	if item == nil {
		return "", fmt.Errorf("resolve groovy catalog item %s: not found", ref.Name)
	}

	if item.Spec.Type != v1alpha1.CatalogItemGroovy {
		return "", fmt.Errorf("catalog item %q has type %q, not groovy", ref.Name, item.Spec.Type)
	}

	if ref.PinnedContentHash != "" && item.Status.ContentHash != ref.PinnedContentHash {
		return "", fmt.Errorf("catalog item %q content hash drift: pinned %s, current %s", ref.Name, ref.PinnedContentHash, item.Status.ContentHash)
	}

	resolved := bundle.ResolveVars(item.Status.Content, bundle.Variables(ref.Variables))

	winner, err := r.writeScriptConfigMap(ctx, op, cmName, resolved)
	if err != nil {
		return "", err
	}
	op.Status.ScriptSnapshotRef = cmName // persisted opportunistically by the next patchStatus
	return winner, nil
}

// groovyScriptConfigMapName is the deterministic snapshot name for an operation.
// Deterministic so that the audit pointer can be reconstructed without reading
// status, and so a crashed reconcile reuses the same snapshot rather than
// resolving the catalog item again.
func groovyScriptConfigMapName(op *v1alpha1.BroodOperation) string {
	return op.Name + "-groovy-script"
}

// writeScriptConfigMap get-or-creates the owner-referenced ConfigMap snapshot for a
// catalog-sourced executeGroovy script. On a losing create race (another caller won),
// it re-reads and returns the winner's persisted content rather than erroring — the
// snapshot must be byte-identical for every target regardless of which caller's Create
// actually landed.
func (r *BroodOperationReconciler) writeScriptConfigMap(ctx context.Context, op *v1alpha1.BroodOperation, cmName, content string) (string, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cmName,
			Namespace:       op.Namespace,
			OwnerReferences: []metav1.OwnerReference{broodOperationOwnerRef(op)},
		},
		Data: map[string]string{"script": content},
	}

	err := r.client.Create(ctx, cm)
	if err == nil {
		return content, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create groovy script snapshot %s/%s: %w", op.Namespace, cmName, err)
	}

	winner, readErr := r.readScriptConfigMap(ctx, op, cmName)
	if readErr != nil {
		return "", fmt.Errorf("read groovy script snapshot %s/%s after create race: %w", op.Namespace, cmName, readErr)
	}
	return winner, nil
}

// readScriptConfigMap reads a previously-written script snapshot ConfigMap, verifying it
// is owned by this exact BroodOperation (by UID) and carries the expected data key.
// Returns a NotFound-wrapped error (checkable with apierrors.IsNotFound) when the
// ConfigMap doesn't exist yet — the caller treats that as "no snapshot yet", not failure.
// Any other read error propagates directly. An owner-UID mismatch or a missing "script"
// data key is treated as resolution failure, not silently overwritten or an empty script.
func (r *BroodOperationReconciler) readScriptConfigMap(ctx context.Context, op *v1alpha1.BroodOperation, name string) (string, error) {
	cm := &corev1.ConfigMap{}
	err := r.client.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: name}, cm)
	if err != nil {
		return "", err // includes NotFound, propagated as-is for apierrors.IsNotFound checks upstream
	}

	owned := false
	for _, o := range cm.OwnerReferences {
		if o.UID == op.UID {
			owned = true
			break
		}
	}
	if !owned {
		return "", fmt.Errorf("groovy script snapshot %s/%s not owned by %s", op.Namespace, name, op.Name)
	}

	script, ok := cm.Data["script"]
	if !ok {
		return "", fmt.Errorf("groovy script snapshot %s/%s missing script key", op.Namespace, name)
	}

	return script, nil
}

// runGroovyOnTarget mints a short-lived operator JWT and runs the script directly
// against the target's Jenkins /scriptText (bypassing the mite). It is the default
// value of the r.runGroovy seam; tests override the seam instead of calling this.
func (r *BroodOperationReconciler) runGroovyOnTarget(ctx context.Context, ns, name, script string) (string, error) {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, ns)
	if err != nil {
		return "", fmt.Errorf("get controller %s/%s for executeGroovy: %w", ns, name, err)
	}
	if cr == nil {
		return "", fmt.Errorf("controller %s/%s not found for executeGroovy", ns, name)
	}
	token, err := r.operatorTokenSigner.GenerateOperatorJenkinsToken(name, ns, groovyTokenTTL)
	if err != nil {
		return "", fmt.Errorf("mint operator jenkins token: %w", err)
	}
	baseEndpoint := fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", controllerPrefix(cr), ns)
	client := jenkins.NewClient(baseEndpoint, "varroa-operator", token)
	// /scriptText answers 200 even when the script fails to compile or throws,
	// so the raw call can't distinguish "ran" from "never executed". The
	// harness wraps the script and prints a sentinel only on completion.
	raw, err := client.ScriptConsoleOnce(ctx, wrapGroovyForClassification(script))
	if err != nil {
		return raw, err
	}
	return classifyGroovyOutput(raw)
}

func broodOperationOwnerRef(op *v1alpha1.BroodOperation) metav1.OwnerReference {
	controller := true
	blockDeletion := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.SchemeGroupVersion.String(),
		Kind:               "BroodOperation",
		Name:               op.Name,
		UID:                op.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockDeletion,
	}
}

// --- Activity events (TASK 4.1) ---

func (r *BroodOperationReconciler) emitRunStarted(op *v1alpha1.BroodOperation) {
	e := r.buildRunStartedEvent(op)
	if r.eventSink != nil {
		r.eventSink(e)
		return
	}
	if r.activityPublisher != nil {
		r.activityPublisher.Publish(e)
	}
}

func (r *BroodOperationReconciler) buildRunStartedEvent(op *v1alpha1.BroodOperation) activity.Event {
	waveSet := make(map[int32]bool)
	for _, t := range op.Status.Targets {
		waveSet[t.Wave] = true
	}
	return activity.Event{
		Type:       "broodop.run.started",
		Source:     "operator",
		Controller: "",
		Namespace:  op.Namespace,
		Message:    fmt.Sprintf("brood operation %s/%s: verb=%s startedBy=%s total=%d waves=%d", op.Namespace, op.Name, op.Spec.Action.Verb, op.Status.StartedBy, op.Status.Summary.Total, len(waveSet)),
	}
}

func (r *BroodOperationReconciler) emitTargetFinished(op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) {
	e := r.buildTargetFinishedEvent(op, t)
	if r.eventSink != nil {
		r.eventSink(e)
		return
	}
	if r.activityPublisher != nil {
		r.activityPublisher.Publish(e)
	}
}

func (r *BroodOperationReconciler) buildTargetFinishedEvent(op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) activity.Event {
	msg := fmt.Sprintf("brood operation %s/%s: verb=%s target %s/%s %s",
		op.Namespace, op.Name, op.Spec.Action.Verb, t.Namespace, t.Name, string(t.State))
	if prov := groovyProvenance(op); prov != "" {
		msg += " " + prov
	}
	return activity.Event{
		Type:       "broodop.target.finished",
		Source:     "operator",
		Actor:      op.Status.StartedBy,
		Controller: t.Name,
		Namespace:  t.Namespace,
		Message:    msg,
		Reason:     eventReason(op, t.Reason),
	}
}

// eventReason strips the Jenkins response body out of an executeGroovy failure
// before it reaches the activity stream.
//
// The Jenkins client embeds up to 4 KiB of a non-2xx response body in its error
// (internal/jenkins/client.go), and a script-console compilation failure echoes
// the submitted source back in that body. Publishing it verbatim would put the
// script into a stream retained for up to 90 days and readable by anyone who can
// read activity — defeating the point of carrying only a pointer.
//
// The full reason stays on status.targets[].reason, which lives and dies with
// the BroodOperation object and is gated by that object's RBAC.
//
// Only the HTTP status is kept: it is the part an operator acts on, and it
// cannot contain script text. Errors with no HTTP status came from our own
// client (transport, timeout) and carry a URL rather than a body, so they pass
// through capped.
func eventReason(op *v1alpha1.BroodOperation, reason string) string {
	if reason == "" || op.Spec.Action.Verb != v1alpha1.BroodVerbExecuteGroovy {
		return reason
	}
	if m := httpStatusInError.FindStringSubmatch(reason); m != nil {
		return "script console returned HTTP " + m[1]
	}
	const maxLen = 200
	if len(reason) > maxLen {
		return reason[:maxLen] + "…"
	}
	return reason
}

// httpStatusInError matches the "HTTP <code>" the Jenkins client emits ahead of
// the response body.
var httpStatusInError = regexp.MustCompile(`HTTP (\d{3})`)

// groovyProvenance renders which script ran, for executeGroovy only.
//
// It carries a POINTER, never a body. The activity stream is retained for up to
// 90 days and is readable by anyone who can read activity; Groovy scripts
// routinely contain credentials. The ConfigMap snapshot named here is the
// script's home, is owner-referenced by the operation, and is subject to the
// operation's TTL — so the pointer neither dangles nor outlives its target.
//
// An inline script has no snapshot to point at, so it is identified by digest
// and length instead. That is enough to prove two runs executed the same script,
// or to match a run against a script held elsewhere, without publishing it.
//
// This is emitted on target.finished rather than run.started deliberately:
// buildRunStartedEvent fires before resolveGroovyScript has run, so the
// provenance would be empty there.
func groovyProvenance(op *v1alpha1.BroodOperation) string {
	if op.Spec.Action.Verb != v1alpha1.BroodVerbExecuteGroovy || op.Spec.Action.Groovy == nil {
		return ""
	}
	g := op.Spec.Action.Groovy
	if g.Script != "" {
		sum := sha256.Sum256([]byte(g.Script))
		return fmt.Sprintf("scriptSource=inline scriptSha256=%s scriptBytes=%d",
			hex.EncodeToString(sum[:]), len(g.Script))
	}
	parts := "scriptSource=catalog"
	if g.ItemRef != nil {
		ns := g.ItemRef.Namespace
		if ns == "" {
			ns = op.Namespace
		}
		parts += fmt.Sprintf(" scriptItemRef=%s/%s", ns, g.ItemRef.Name)
	}
	// Fall back to the deterministic name rather than omitting the pointer.
	// resolveGroovyScript sets ScriptSnapshotRef in memory and starts execution
	// before the reconciler-wide status patch; if that patch fails, a later
	// reconcile rebuilds this event from persisted status with the field empty,
	// and an audit record that silently drops its pointer is worse than one
	// naming a ConfigMap the reader can check for.
	snapshot := op.Status.ScriptSnapshotRef
	if snapshot == "" {
		snapshot = groovyScriptConfigMapName(op)
	}
	parts += fmt.Sprintf(" scriptSnapshotRef=%s/%s", op.Namespace, snapshot)
	return parts
}

func (r *BroodOperationReconciler) emitRunFinished(op *v1alpha1.BroodOperation) {
	e := r.buildRunFinishedEvent(op)
	if r.eventSink != nil {
		r.eventSink(e)
		return
	}
	if r.activityPublisher != nil {
		r.activityPublisher.Publish(e)
	}
}

func (r *BroodOperationReconciler) buildRunFinishedEvent(op *v1alpha1.BroodOperation) activity.Event {
	duration := ""
	if op.Status.StartedAt != nil && op.Status.FinishedAt != nil {
		duration = op.Status.FinishedAt.Time.Sub(op.Status.StartedAt.Time).Round(time.Second).String()
	}
	return activity.Event{
		Type:       "broodop.run.finished",
		Source:     "operator",
		Controller: "",
		Namespace:  op.Namespace,
		Message:    fmt.Sprintf("brood operation %s/%s: verb=%s phase=%s total=%d succeeded=%d failed=%d skipped=%d duration=%s", op.Namespace, op.Name, op.Spec.Action.Verb, op.Status.Phase, op.Status.Summary.Total, op.Status.Summary.Succeeded, op.Status.Summary.Failed, op.Status.Summary.Skipped, duration),
	}
}

// timeoutReason returns the timeout reason string for a verb (D4).
func timeoutReason(verb v1alpha1.BroodVerb) string {
	switch verb {
	case v1alpha1.BroodVerbRestart:
		return "restart result timeout"
	case v1alpha1.BroodVerbReprovision:
		return "reprovision timeout"
	case v1alpha1.BroodVerbReconcile:
		return "reconcile timeout"
	case v1alpha1.BroodVerbStop:
		return "stop timeout"
	case v1alpha1.BroodVerbStart:
		return "start timeout"
	case v1alpha1.BroodVerbExecuteGroovy:
		return "groovy script timeout"
	}
	return "operation timeout"
}

// isTargetTimedOut checks if a dispatched target has exceeded its verb timeout.
func (r *BroodOperationReconciler) isTargetTimedOut(op *v1alpha1.BroodOperation, t *v1alpha1.BroodTargetStatus) bool {
	if t.DispatchedAt == nil {
		return false
	}
	timeout := broodVerbTimeouts[op.Spec.Action.Verb]
	return r.now().Sub(t.DispatchedAt.Time) > timeout
}

// --- ResolveTargets (TASK 3.2) ---

// ResolveTargets resolves the target controllers for a BroodOperation spec.
// Exported for BFF preview reuse. Tenancy-mode violations are returned as
// errors so preview enforces the same rules as the executor.
func ResolveTargets(ctx context.Context, c client.Client, spec v1alpha1.BroodOperationSpec, crNamespace, operatorNamespace string) ([]ResolvedTarget, error) {
	if err := ValidateBroodTenancy(spec, crNamespace, operatorNamespace); err != nil {
		return nil, err
	}

	var targets []ResolvedTarget

	isOperatorNS := crNamespace == operatorNamespace

	if len(spec.Targets.Names) > 0 {
		// Names mode.
		for _, n := range spec.Targets.Names {
			var ns, name string
			if isOperatorNS && strings.Contains(n, "/") {
				parts := strings.SplitN(n, "/", 2)
				ns, name = parts[0], parts[1]
			} else {
				ns = crNamespace
				name = n
			}

			var ctrl v1alpha1.Controller
			err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ctrl)
			if apierrors.IsNotFound(err) {
				targets = append(targets, ResolvedTarget{
					Namespace:  ns,
					Name:       name,
					Applicable: false,
					SkipReason: "not found",
				})
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("get controller %s/%s: %w", ns, name, err)
			}
			wave := int32(0)
			if ctrl.Spec.ReconciliationPolicy != nil {
				wave = int32(ctrl.Spec.ReconciliationPolicy.RolloutWave)
			}
			targets = append(targets, ResolvedTarget{
				Namespace:  ns,
				Name:       name,
				Wave:       wave,
				Applicable: true,
				Controller: &ctrl,
			})
		}
	} else if spec.Targets.Selector != nil {
		// Selector mode.
		sel, err := metav1.LabelSelectorAsSelector(spec.Targets.Selector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector: %w", err)
		}

		var namespaces []string
		if len(spec.Targets.Namespaces) > 0 {
			if len(spec.Targets.Namespaces) == 1 && spec.Targets.Namespaces[0] == "all" {
				// "all" sentinel: list Controllers cluster-wide, project to unique namespaces.
				all, err := listControllersClusterWide(ctx, c)
				if err != nil {
					return nil, fmt.Errorf("list controllers cluster-wide: %w", err)
				}
				nsSet := make(map[string]bool)
				for _, ctrl := range all {
					nsSet[ctrl.Namespace] = true
				}
				for ns := range nsSet {
					namespaces = append(namespaces, ns)
				}
				sort.Strings(namespaces)
			} else {
				namespaces = spec.Targets.Namespaces
			}
		} else {
			namespaces = []string{crNamespace}
		}

		// Collect all controllers matching selector across target namespaces.
		ctrlMap := make(map[string]*v1alpha1.Controller) // "ns/name" -> controller
		for _, ns := range namespaces {
			controllers, err := listControllersByNamespace(ctx, c, ns, sel)
			if err != nil {
				return nil, fmt.Errorf("list controllers in namespace %s: %w", ns, err)
			}
			for _, ctrl := range controllers {
				ctrlMap[ctrl.Namespace+"/"+ctrl.Name] = ctrl
			}
		}

		for key, ctrl := range ctrlMap {
			_ = key
			wave := int32(0)
			if ctrl.Spec.ReconciliationPolicy != nil {
				wave = int32(ctrl.Spec.ReconciliationPolicy.RolloutWave)
			}
			targets = append(targets, ResolvedTarget{
				Namespace:  ctrl.Namespace,
				Name:       ctrl.Name,
				Wave:       wave,
				Applicable: true,
				Controller: ctrl,
			})
		}
	}

	if len(targets) == 0 {
		return nil, nil
	}

	// Apply filters.
	targets = applyFilters(targets, spec.Targets.Filters, operatorNamespace)

	// Apply verb applicability.
	verb := spec.Action.Verb
	filtered := make([]ResolvedTarget, 0, len(targets))
	for _, t := range targets {
		if !t.Applicable {
			// Already skipped (not found).
			filtered = append(filtered, t)
			continue
		}
		if t.Controller == nil {
			filtered = append(filtered, t)
			continue
		}
		if reason := checkApplicability(verb, t.Controller); reason != "" {
			t.Applicable = false
			t.SkipReason = reason
		}
		filtered = append(filtered, t)
	}
	targets = filtered

	// order: name collapses waves — every target lands in wave 0 so the
	// (wave, namespace, name) sort below degenerates to pure name order.
	if spec.Execution != nil && spec.Execution.Order == v1alpha1.BroodOrderName {
		for i := range targets {
			targets[i].Wave = 0
		}
	}

	// Sort by (wave, namespace, name).
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Wave != targets[j].Wave {
			return targets[i].Wave < targets[j].Wave
		}
		if targets[i].Namespace != targets[j].Namespace {
			return targets[i].Namespace < targets[j].Namespace
		}
		return targets[i].Name < targets[j].Name
	})

	return targets, nil
}

// applyFilters applies target filters, removing non-matching controllers.
// Non-matching controllers are silently excluded (not Skipped).
// applyFilters narrows resolved targets. operatorNamespace is needed because the
// bundle filter compares against the controller's EFFECTIVE bundle: a Controller
// with no composedBundleRef runs the starter bundle, and reading the spec field
// directly would exclude every zero-config controller from a
// `bundle: varroa-starter` operation — silently, since filters drop rather than
// report.
func applyFilters(targets []ResolvedTarget, filters *v1alpha1.BroodTargetFilters, operatorNamespace string) []ResolvedTarget {
	if filters == nil {
		return targets
	}

	var result []ResolvedTarget
	for _, t := range targets {
		if !t.Applicable {
			result = append(result, t)
			continue
		}
		if t.Controller == nil {
			result = append(result, t)
			continue
		}

		if filters.Phase != nil && string(*filters.Phase) != string(t.Controller.Status.Phase) {
			continue // silently excluded
		}
		if filters.Version != nil && *filters.Version != t.Controller.Spec.Version {
			continue
		}
		if filters.Bundle != nil {
			effective, _ := v1alpha1.EffectiveBundleRef(t.Controller, operatorNamespace)
			if *filters.Bundle != effective {
				continue
			}
		}
		result = append(result, t)
	}
	return result
}

// checkApplicability checks whether the verb applies to the controller.
// Returns "" if applicable, or a skip reason.
func checkApplicability(verb v1alpha1.BroodVerb, ctrl *v1alpha1.Controller) string {
	if ctrl == nil {
		return ""
	}
	switch verb {
	case v1alpha1.BroodVerbRestart:
		if ctrl.Status.Phase != v1alpha1.ControllerPhaseConnected {
			return "not Connected"
		}
	case v1alpha1.BroodVerbStart:
		if ctrl.Status.Phase != v1alpha1.ControllerPhaseStopped && ctrl.Status.Phase != v1alpha1.ControllerPhaseHibernated {
			return "not Stopped or Hibernated"
		}
	case v1alpha1.BroodVerbStop:
		if ctrl.Status.Phase == v1alpha1.ControllerPhaseStopped {
			return "already Stopped"
		}
	case v1alpha1.BroodVerbReprovision:
		if ctrl.Status.Phase == v1alpha1.ControllerPhaseStopped {
			return "Stopped"
		}
	case v1alpha1.BroodVerbReconcile:
		switch ctrl.Status.Phase {
		case v1alpha1.ControllerPhaseHibernated:
			return "target hibernated"
		case v1alpha1.ControllerPhaseStopped:
			return "target stopped"
		}

	case v1alpha1.BroodVerbExecuteGroovy:
		if ctrl.Status.Phase != v1alpha1.ControllerPhaseConnected {
			return "not Connected"
		}
	}
	return ""
}

// listControllersClusterWide lists all Controller CRs across all namespaces.
func listControllersClusterWide(ctx context.Context, c client.Client) ([]*v1alpha1.Controller, error) {
	var list v1alpha1.ControllerList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	result := make([]*v1alpha1.Controller, len(list.Items))
	for i := range list.Items {
		result[i] = &list.Items[i]
	}
	return result, nil
}

// listControllersByNamespace lists Controller CRs in a namespace matching a label selector.
func listControllersByNamespace(ctx context.Context, c client.Client, ns string, sel labels.Selector) ([]*v1alpha1.Controller, error) {
	var list v1alpha1.ControllerList
	if err := c.List(ctx, &list, &client.ListOptions{
		Namespace:     ns,
		LabelSelector: sel,
	}); err != nil {
		return nil, err
	}
	result := make([]*v1alpha1.Controller, len(list.Items))
	for i := range list.Items {
		result[i] = &list.Items[i]
	}
	return result, nil
}

// Ensure interfaces.
var _ reconcile.Reconciler = (*BroodOperationReconciler)(nil)
