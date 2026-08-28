package controller

import (
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// FieldConflict is one SSA field-ownership conflict extracted from a Kubernetes
// 409 Conflict response.
type FieldConflict struct {
	Field   string // JSON path from the StatusCause, e.g. ".spec.resources.limits.cpu"
	Manager string // best-effort extraction from the cause message; "" if not parseable
	Message string // raw cause message, always populated — UI falls back to this
}

// conflictManagerRE matches the manager name in a cause message.
// Message shape verified against k8s.io/apimachinery@v0.36.0
// pkg/util/managedfields/internal/conflict.go (see design.md §1.2 / task 0.2).
var conflictManagerRE = regexp.MustCompile(`conflict with "([^"]+)"`)

// SSAConflicts extracts FieldConflict entries from a 409 Conflict error returned
// by an ApplyPatchType Patch call. Returns nil if err is not a conflict or carries
// no parseable causes (caller should still surface err.Error() as a fallback message
// in that case).
func SSAConflicts(err error) []FieldConflict {
	statusErr, ok := err.(*apierrors.StatusError)
	if !ok || !apierrors.IsConflict(err) {
		return nil
	}
	details := statusErr.ErrStatus.Details
	if details == nil {
		return nil
	}
	out := make([]FieldConflict, 0, len(details.Causes))
	for _, cause := range details.Causes {
		fc := FieldConflict{Field: cause.Field, Message: cause.Message}
		if m := conflictManagerRE.FindStringSubmatch(cause.Message); m != nil {
			fc.Manager = m[1]
		}
		out = append(out, fc)
	}
	return out
}
