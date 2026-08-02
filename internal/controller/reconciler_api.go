package controller

import "context"

// ReconcilerAPI defines the operations the API layer uses on the reconciler.
// The operator's *Reconciler and the BFF's NATSReconcilerProxy implement this interface.
type ReconcilerAPI interface {
	// TriggerReconcile enqueues an on-demand reconcile for the given controller.
	TriggerReconcile(cluster, name, namespace string)

	// WakeController signals the per-controller goroutine to reconcile immediately.
	WakeController(cluster, namespace, name string)

	// Reprovision forces the controller to re-push its full desired state to
	// the mite on the next reconcile, bypassing the convergence short-circuit.
	Reprovision(cluster, namespace, name string)

	// ApproveRestart applies a pending configuration change. action must be
	// "reload" (JCasC reload) or "restart" (Jenkins safe restart).
	ApproveRestart(ctx context.Context, cluster, namespace, name, action string) error

	// ApproveDeletion authorizes deleting a previously-deferred item, issuing a
	// targeted delete to the mite. The path must match a pending deletion entry.
	ApproveDeletion(ctx context.Context, cluster, namespace, name, path string) error
}

// Compile-time check that *Reconciler satisfies ReconcilerAPI.
var _ ReconcilerAPI = (*Reconciler)(nil)
