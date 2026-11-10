package rbac

import (
	"fmt"
	"strings"
)

// FederatedCopyMessage answers the refusal text for a change to a federated
// JenkinsRole or JenkinsRoleBinding copy. kind is "JenkinsRole" or
// "JenkinsRoleBinding". It answers "" when labels hold no LabelFederatedFrom
// value. The text names the sources from the owner annotation, or the source
// VarroaRole from the label when the annotation is absent.
func FederatedCopyMessage(kind, name string, labels, annotations map[string]string) string {
	from := labels[LabelFederatedFrom]
	if from == "" {
		return ""
	}
	sourceKind := "VarroaRole"
	sources := from
	switch kind {
	case "JenkinsRole":
		if list := annotations[AnnotationFederatedRoles]; list != "" {
			sources = list
		}
	case "JenkinsRoleBinding":
		if list := annotations[AnnotationFederatedBindings]; list != "" {
			sourceKind = "VarroaRoleBinding"
			sources = list
		}
	}
	return fmt.Sprintf("%s %s is a federated copy of %s %s. Federation replaces each change to the copy. Change the source on the core cluster.",
		kind, name, sourceKind, strings.ReplaceAll(sources, ",", ", "))
}
