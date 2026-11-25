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

// locationController reads the controller namespace/name for its location
// URL. gone is true only when the controller does not exist. Any other read
// error answers nil and false, and logs a warning.
func (r *Reconciler) locationController(ctx context.Context, namespace, name string) (cr *v1alpha1.Controller, gone bool) {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true
		}
		r.Logger.Warn("read controller for its location url failed",
			"controller", namespace+"/"+name, "error", err)
		return nil, false
	}
	return cr, false
}

// JenkinsLocationURL answers the Jenkins location URL that the operator
// injects now for the headless controller namespace/name. The branch-source
// plugins build each commit-status target URL under it. It answers "" when
// the controller is gone, cannot be read, or is not headless. The caller then
// cannot tell the statuses of this Jenkins from those of another Jenkins, so
// it must write no status.
func (r *Reconciler) JenkinsLocationURL(ctx context.Context, namespace, name string) string {
	cr, _ := r.locationController(ctx, namespace, name)
	if cr == nil || !v1alpha1.IsHeadless(cr) {
		return ""
	}
	return r.headlessLocationURL(cr)
}

// EndStatusLocation answers the two URLs that the end write of a commit
// status compares for a run that the controller namespace/name owned.
//
// cell is JenkinsLocationURL for a controller that exists. For a controller
// that is gone, cell is its dashboard door. HeadlessLocationURL reads only the
// origin, the cluster, the namespace, and the name, so the door of a deleted
// cell is the URL that the operator injected while the cell existed. Without
// a dashboard origin a gone controller answers "", because its in-cluster
// endpoint names the UID of the deleted object.
//
// doors is HeadlessDoorPrefix of this cluster, or "" without a dashboard
// origin. A controller that cannot be read answers "" for both, and so does
// a controller that is not headless.
func (r *Reconciler) EndStatusLocation(ctx context.Context, namespace, name string) (cell, doors string) {
	origin := varroaBaseURL(r.varroaRedirectURL)
	cr, gone := r.locationController(ctx, namespace, name)
	switch {
	case gone:
		cell = v1alpha1.HeadlessLocationURL(origin, r.clusterName, namespace, name)
	case cr != nil && v1alpha1.IsHeadless(cr):
		cell = r.headlessLocationURL(cr)
	}
	if cell == "" {
		return "", ""
	}
	return cell, v1alpha1.HeadlessDoorPrefix(origin, r.clusterName)
}
