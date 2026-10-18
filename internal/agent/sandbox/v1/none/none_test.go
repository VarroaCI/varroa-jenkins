package none

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/varroaci/varroa-jenkins/internal/agent/sandbox/v1"
	"github.com/varroaci/varroa-jenkins/internal/agent/sandbox/v1/conformance"
)

// TestNoneConformance runs the shared contract checks against the none runtime.
func TestNoneConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func() v1.Runtime { return New() })
}

// TestStartReturnsUnsupported asserts that a valid spec gets ErrUnsupported and
// no session, and that an invalid spec is refused before that.
func TestStartReturnsUnsupported(t *testing.T) {
	t.Parallel()

	sess, err := New().Start(context.Background(), v1.Spec{Image: "x"})
	if !errors.Is(err, v1.ErrUnsupported) || sess != nil {
		t.Fatalf("Start = (%v, %v), want ErrUnsupported and nil session", sess, err)
	}
	sess, err = New().Start(context.Background(), v1.Spec{})
	if !errors.Is(err, v1.ErrInvalidSpec) || sess != nil {
		t.Fatalf("empty image Start = (%v, %v), want ErrInvalidSpec", sess, err)
	}
}
