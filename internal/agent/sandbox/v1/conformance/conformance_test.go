package conformance

import (
	"context"
	"errors"
	"io"
	"testing"

	v1 "github.com/varroaci/varroa-jenkins/internal/agent/sandbox/v1"
)

// fakeRuntime starts a session that honours Stop and refuses every egress
// target, because the fake grants no egress.
type fakeRuntime struct{}

func (fakeRuntime) Name() string { return "fake" }

func (fakeRuntime) Start(_ context.Context, spec v1.Spec) (v1.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &fakeSession{}, nil
}

type fakeSession struct {
	stopped bool
}

func (s *fakeSession) Exec(_ context.Context, _ []string, _ io.Reader) (v1.ExecResult, error) {
	if s.stopped {
		return v1.ExecResult{}, v1.ErrStopped
	}
	return v1.ExecResult{}, nil
}

func (s *fakeSession) Logs(_ context.Context) (io.ReadCloser, error) {
	return nil, errors.New("no logs in the fake")
}

func (s *fakeSession) Stop(_ context.Context) error {
	s.stopped = true
	return nil
}

func (s *fakeSession) ProbeEgress(_ context.Context, _ string, _ int) error {
	return errors.New("egress refused")
}

// TestRunAcceptsStartingRuntime runs the checks against a runtime that starts
// sessions and honours Stop and egress.
func TestRunAcceptsStartingRuntime(t *testing.T) {
	t.Parallel()
	Run(t, func() v1.Runtime { return fakeRuntime{} })
}
