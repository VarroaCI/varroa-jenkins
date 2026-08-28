package controller

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// GroupReconciler validates Group CRD members and logs warnings for
// unknown users. It runs on a 60-second ticker in local auth mode.
type GroupReconciler struct {
	client ResourceClient
	store  crdstore.Backend
	ns     string
	Logger *slog.Logger
}

// NewGroupReconciler creates a GroupReconciler.
func NewGroupReconciler(client ResourceClient, store crdstore.Backend, ns string) *GroupReconciler {
	return &GroupReconciler{client: client, store: store, ns: ns, Logger: slog.Default()}
}

// Reconcile lists all Groups and checks that each member has a
// corresponding User CRD. Missing or lookup-failed users are logged
// as warnings (non-blocking).
func (r *GroupReconciler) Reconcile(ctx context.Context) error {
	groups, err := crdstore.List[v1alpha1.Group](ctx, r.store, "", "")
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}

	for _, g := range groups {
		for _, member := range g.Spec.Members {
			_, err := crdstore.Get[v1alpha1.User](ctx, r.store, member, r.ns)
			if err != nil {
				if apierrors.IsNotFound(err) {
					r.Logger.Warn("group references unknown user", "group", g.Name, "member", member)
				} else {
					r.Logger.Warn("failed to look up group member", "group", g.Name, "member", member, "error", err)
				}
				continue
			}
		}
	}

	return nil
}
