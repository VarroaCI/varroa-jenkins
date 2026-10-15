package controller

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// closureCompleteCondition reports whether the update center read the
// dependency list of every member of a derived item's closure.
const closureCompleteCondition = "ClosureComplete"

// closureCompleteness reports the ClosureComplete condition for a resolved
// closure. The second result is the invalid message for an incomplete closure
// and is empty when the closure is complete.
func closureCompleteness(cl *ucClosure) (v1alpha1.TemplateCatalogCondition, string) {
	now := metav1.Now()
	if len(cl.UnknownDeps) == 0 {
		return v1alpha1.TemplateCatalogCondition{
			Type:               closureCompleteCondition,
			Status:             metav1.ConditionTrue,
			Reason:             reasonAllDependenciesKnown,
			Message:            "the update center reads the dependency list of every closure member",
			LastTransitionTime: now,
		}, ""
	}
	msg := fmt.Sprintf("the dependency closure is incomplete: the update center cannot read the dependency list of %s",
		strings.Join(cl.UnknownDeps, ", "))
	return v1alpha1.TemplateCatalogCondition{
		Type:               closureCompleteCondition,
		Status:             metav1.ConditionFalse,
		Reason:             reasonUnknownDependencies,
		Message:            msg,
		LastTransitionTime: now,
	}, msg
}
