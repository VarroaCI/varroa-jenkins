// Package conformance holds the checks that every sandbox runtime must pass.
// A runtime package runs Run from its own test.
package conformance

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/varroaci/varroa-jenkins/internal/agent/sandbox/v1"
)

// EgressProber is implemented by a session that can test one egress target.
// The probe must fail for every destination outside Spec.Egress.
type EgressProber interface {
	ProbeEgress(ctx context.Context, host string, port int) error
}

// Run checks the contract of the runtime that factory returns. The factory
// is called once for each check, so each check starts from a fresh runtime.
func Run(t *testing.T, factory func() v1.Runtime) {
	t.Helper()

	if factory().Name() == "" {
		t.Fatal("runtime name is empty")
	}

	bad := map[string]v1.Spec{
		"empty image":         {Image: "   "},
		"credential env":      {Image: "conformance-image", Env: map[string]string{"TOKEN": "gh" + "p_" + "aaaaaaaaaaaaaaaaaaaaaaaa"}},
		"rule with no target": {Image: "conformance-image", Egress: []v1.Rule{{Port: 443}}},
	}
	for name, spec := range bad {
		if err := spec.Validate(); !errors.Is(err, v1.ErrInvalidSpec) {
			t.Fatalf("%s: Validate = %v, want ErrInvalidSpec", name, err)
		}
		sess, err := factory().Start(context.Background(), spec)
		if err == nil || sess != nil {
			t.Fatalf("%s: Start = (%v, %v), want an error and no session", name, sess, err)
		}
	}

	valid := v1.Spec{Image: "conformance-image"}
	sess, err := factory().Start(context.Background(), valid)
	if errors.Is(err, v1.ErrUnsupported) {
		if sess != nil {
			t.Fatal("ErrUnsupported came with a session")
		}
		return
	}
	if err != nil {
		t.Fatalf("Start on a valid spec: %v", err)
	}
	if sess == nil {
		t.Fatal("Start returned no session and no error")
	}

	ctx := context.Background()
	if err := sess.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sess.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, err := sess.Exec(ctx, []string{"true"}, nil); err == nil {
		t.Fatal("Exec after Stop succeeded")
	}

	prober, ok := sess.(EgressProber)
	if !ok {
		t.Fatal("session does not implement EgressProber")
	}
	if err := prober.ProbeEgress(ctx, "egress-probe.invalid", 443); err == nil {
		t.Fatal("egress outside Spec.Egress was allowed")
	}
}
