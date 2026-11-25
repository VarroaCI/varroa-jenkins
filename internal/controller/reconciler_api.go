package controller

import "context"

// ReconcilerAPI defines the operations the API layer uses on the operator of
// a cluster. The BFF's NATSReconcilerProxy implements this interface. Each
// method returns the bus error when no operator replies, so the caller can
// tell a request that no operator received from one that the operator took.
type ReconcilerAPI interface {
	// TriggerReconcile asks the operator of cluster to enqueue an on-demand
	// reconcile for the given controller. A nil error means that the operator
	// recorded the request. The reconcile itself runs later. A refusal is an
	// *ActionError: code "not_found" for a missing controller.
	TriggerReconcile(ctx context.Context, cluster, name, namespace string) error

	// WakeController asks the operator of cluster to signal the
	// per-controller goroutine to reconcile immediately.
	WakeController(ctx context.Context, cluster, namespace, name string) error

	// Reprovision asks the operator of cluster to make the next reconcile
	// re-push the full desired state to the mite, bypassing the convergence
	// short-circuit. A nil error means that the operator stamped the
	// request on the controller. A refusal is an *ActionError: code
	// "not_found" for a missing controller.
	Reprovision(ctx context.Context, cluster, namespace, name string) error

	// ApproveRestart applies a pending configuration change. action must be
	// "reload" (JCasC reload) or "restart" (Jenkins safe restart). A
	// non-empty expectedContentHash is the content hash of the pending change
	// that the caller reviewed. A refusal for changed content is a
	// *PendingRestartChangedError.
	ApproveRestart(ctx context.Context, cluster, namespace, name, action, expectedContentHash string) error

	// ApproveDeletion authorizes deleting a previously-deferred item, issuing a
	// targeted delete to the mite. The path must match a pending deletion entry.
	ApproveDeletion(ctx context.Context, cluster, namespace, name, path string) error

	// Hibernate parks the controller via the status compare-and-swap helper,
	// independent of spec.hibernation.enabled. It returns a non-nil error when
	// the target operator refuses the action (e.g. a Stopped controller).
	Hibernate(ctx context.Context, cluster, namespace, name string) error

	// Wake clears the controller's hibernation via the status compare-and-swap
	// helper and re-provisions it. It returns a non-nil error when the target
	// operator refuses the action (e.g. a Stopped controller).
	Wake(ctx context.Context, cluster, namespace, name string) error
}

var _ ReconcilerAPI = (*NATSReconcilerProxy)(nil)
