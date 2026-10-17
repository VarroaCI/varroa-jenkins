package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// jenkinsUpstreamReader is implemented by ResourceClient backends that can
// read the upstream stamp and JAVA_OPTS of the live StatefulSet. It is a
// type-assertion seam for the same reason as artifactSecretHashReader.
type jenkinsUpstreamReader interface {
	GetStatefulSetJenkinsUpstream(ctx context.Context, name, namespace string) (stamp, javaOpts string, found bool, err error)
}

// reconcileJenkinsUpstreamRoll compares the upstream mode the operator wants
// with the mode the live StatefulSet carries. The stamp on the object
// metadata is the authority. A StatefulSet with no stamp was built before the
// stamp existed, so its live JAVA_OPTS decides the mode. On a difference it
// transitions a Connected controller to Provisioning, which re-renders the
// JVM flags. The roll writes the stamp, so the check settles after one roll.
// It returns true when it transitioned the phase. A read error, a missing
// StatefulSet, or a backend without the seam is a no-op.
func (r *Reconciler) reconcileJenkinsUpstreamRoll(ctx context.Context, cr *v1alpha1.Controller) bool {
	reader, ok := r.client.(jenkinsUpstreamReader)
	if !ok {
		return false
	}
	desired := jenkinsUpstreamStamp(r.jenkinsUpstreamURL == "")
	stamp, javaOpts, found, err := reader.GetStatefulSetJenkinsUpstream(ctx, controllerPrefix(cr), cr.Namespace)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("jenkins-upstream-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return false
	}
	if !found {
		return false
	}
	live := stamp
	if live == "" {
		live = liveUpstreamFromJavaOpts(javaOpts)
	}
	if live == desired {
		return false
	}
	now := metav1Now()
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
		Message:            fmt.Sprintf("re-provisioning to apply jenkins upstream change: %s -> %s", live, desired),
	})
	return true
}

// liveUpstreamFromJavaOpts reads the upstream mode from a JAVA_OPTS value.
// The mode is "off" only when both never flags are tokens of the value,
// because buildStatefulSet writes them together.
func liveUpstreamFromJavaOpts(javaOpts string) string {
	var updateOff, downloadOff bool
	for _, tok := range strings.Fields(javaOpts) {
		updateOff = updateOff || tok == neverUpdateCenterFlag
		downloadOff = downloadOff || tok == neverDownloadServiceFlag
	}
	if updateOff && downloadOff {
		return "off"
	}
	return "on"
}
