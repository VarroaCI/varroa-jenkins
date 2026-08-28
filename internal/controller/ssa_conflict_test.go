package controller

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSSAConflicts(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantLen int
		wantF0  FieldConflict // expected FieldConflict at index 0; zero = skip check
	}{
		{
			name:    "nil error",
			err:     nil,
			wantLen: 0,
		},
		{
			name:    "non-conflict error",
			err:     apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}, "test"),
			wantLen: 0,
		},
		{
			name:    "non-status error",
			err:     apierrors.NewServiceUnavailable("server unavailable"),
			wantLen: 0,
		},
		{
			name: "single conflict with manager",
			err: apierrors.NewApplyConflict(
				[]metav1.StatusCause{
					{
						Type:    metav1.CauseTypeFieldManagerConflict,
						Field:   ".spec.resources",
						Message: `conflict with "other-manager" using varroa.dev/v1alpha1`,
					},
				},
				"Apply failed with 1 conflict: conflict with \"other-manager\": .spec.resources",
			),
			wantLen: 1,
			wantF0:  FieldConflict{Field: ".spec.resources", Manager: "other-manager", Message: `conflict with "other-manager" using varroa.dev/v1alpha1`},
		},
		{
			name: "multiple conflicts",
			err: apierrors.NewApplyConflict(
				[]metav1.StatusCause{
					{
						Type:    metav1.CauseTypeFieldManagerConflict,
						Field:   ".spec.resources.limits.cpu",
						Message: `conflict with "varroa-operator" using varroa.dev/v1alpha1`,
					},
					{
						Type:    metav1.CauseTypeFieldManagerConflict,
						Field:   ".spec.version",
						Message: `conflict with "kubectl" using varroa.dev/v1alpha1`,
					},
				},
				"Apply failed with 2 conflicts: conflicts with \"varroa-operator\": ...",
			),
			wantLen: 2,
			wantF0:  FieldConflict{Field: ".spec.resources.limits.cpu", Manager: "varroa-operator", Message: `conflict with "varroa-operator" using varroa.dev/v1alpha1`},
		},
		{
			name: "unmatched message — no quoted substring",
			err: apierrors.NewApplyConflict(
				[]metav1.StatusCause{
					{
						Type:    metav1.CauseTypeFieldManagerConflict,
						Field:   ".spec.someField",
						Message: "this message has no quotes at all",
					},
				},
				"Apply failed with 1 conflict",
			),
			wantLen: 1,
			wantF0:  FieldConflict{Field: ".spec.someField", Manager: "", Message: "this message has no quotes at all"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SSAConflicts(tt.err)
			if len(got) != tt.wantLen {
				t.Fatalf("SSAConflicts() length = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && tt.wantF0.Field != "" {
				if got[0].Field != tt.wantF0.Field {
					t.Errorf("Field = %q, want %q", got[0].Field, tt.wantF0.Field)
				}
				if got[0].Manager != tt.wantF0.Manager {
					t.Errorf("Manager = %q, want %q", got[0].Manager, tt.wantF0.Manager)
				}
				if got[0].Message != tt.wantF0.Message {
					t.Errorf("Message = %q, want %q", got[0].Message, tt.wantF0.Message)
				}
			}
		})
	}
}
