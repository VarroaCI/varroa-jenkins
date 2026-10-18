// Package none holds the runtime that starts no session. The assistant
// ships this runtime, so a deployment without a sandbox refuses a sandbox
// start with a clear error.
package none

import (
	"context"

	v1 "github.com/varroaci/varroa-jenkins/internal/agent/sandbox/v1"
)

// Runtime is the runtime that starts no session.
type Runtime struct{}

// New returns the runtime that starts no session.
func New() Runtime { return Runtime{} }

// Name returns the runtime name.
func (Runtime) Name() string { return "none" }

// Start validates the spec, then returns ErrUnsupported and no session.
func (Runtime) Start(_ context.Context, spec v1.Spec) (v1.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return nil, v1.ErrUnsupported
}
