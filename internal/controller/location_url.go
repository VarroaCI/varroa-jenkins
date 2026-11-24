package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// headlessLocationURL answers the Jenkins location URL that the operator
// injects for a headless controller: the dashboard door when the operator
// has a dashboard origin, else the in-cluster endpoint.
func (r *Reconciler) headlessLocationURL(cr *v1alpha1.Controller) string {
	if u := v1alpha1.HeadlessLocationURL(varroaBaseURL(r.varroaRedirectURL), r.clusterName, cr.Namespace, cr.Name); u != "" {
		return u
	}
	return inClusterEndpoint(cr) + "/"
}

// JenkinsLocationURL answers the Jenkins location URL that the operator
// injects now for the headless controller namespace/name. The branch-source
// plugins build each commit-status target URL under it. It answers "" when
// the controller is gone, cannot be read, or is not headless. The caller then
// cannot tell the statuses of this Jenkins from those of another Jenkins, so
// it must write no status.
func (r *Reconciler) JenkinsLocationURL(ctx context.Context, namespace, name string) string {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Warn("read controller for its location url failed",
				"controller", namespace+"/"+name, "error", err)
		}
		return ""
	}
	if !v1alpha1.IsHeadless(cr) {
		return ""
	}
	return r.headlessLocationURL(cr)
}
