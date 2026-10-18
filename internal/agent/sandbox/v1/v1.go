// Package v1 holds the sandbox contract of the agent runtime. A runtime
// starts a session that runs one image with bounded egress. The contract
// carries Secret references and never credential values. A later version
// lives beside this one, and this package never changes its exported shape.
package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Runtime starts sandbox sessions.
type Runtime interface {
	Name() string
	Start(ctx context.Context, spec Spec) (Session, error)
}

// Spec describes one sandbox session.
type Spec struct {
	// Image is the container image to run.
	Image string
	// Env holds plain values. Validate rejects a value that looks like a
	// credential.
	Env map[string]string
	// Egress lists the hosts or CIDRs and ports that the session may reach.
	// An empty list means no egress.
	Egress []Rule
	// Credentials lists Secret references. A reference never holds a value.
	Credentials []Ref
	// Limits bounds the CPU, the memory, and the wall time.
	Limits Limits
}

// Rule names one egress destination: a host or a CIDR, and a port.
type Rule struct {
	Host string
	CIDR string
	Port int
}

// Ref names one Secret key and where the session mounts it.
type Ref struct {
	Namespace string
	Name      string
	Key       string
	MountAs   string
}

// Limits bounds one session.
type Limits struct {
	CPUMillis   int64
	MemoryBytes int64
	WallTime    time.Duration
}

// Session is one running sandbox.
type Session interface {
	Exec(ctx context.Context, argv []string, stdin io.Reader) (ExecResult, error)
	Logs(ctx context.Context) (io.ReadCloser, error)
	// Stop ends the session. Calling Stop again returns nil.
	Stop(ctx context.Context) error
}

// ExecResult is the outcome of one Exec call.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// ErrUnsupported is returned by a runtime that does not start sessions.
var ErrUnsupported = errors.New("sandbox: runtime does not start sessions")

// ErrInvalidSpec is the parent of every Validate refusal.
var ErrInvalidSpec = errors.New("sandbox: invalid spec")

// ErrEmptyImage refuses a spec with no image.
var ErrEmptyImage = fmt.Errorf("%w: image is empty", ErrInvalidSpec)

// ErrCredentialEnv refuses a plain env value that looks like a credential.
var ErrCredentialEnv = fmt.Errorf("%w: env value looks like a credential", ErrInvalidSpec)

// ErrRuleTarget refuses an egress rule with neither a host nor a CIDR.
var ErrRuleTarget = fmt.Errorf("%w: egress rule names no host or CIDR", ErrInvalidSpec)

// ErrStopped is returned by Exec and Logs after Stop.
var ErrStopped = errors.New("sandbox: session is stopped")

// Validate refuses a spec that the contract never allows. It checks the
// image, every env value, and every egress rule.
func (s Spec) Validate() error {
	if trimmedEmpty(s.Image) {
		return ErrEmptyImage
	}
	for _, v := range s.Env {
		if looksLikeCredential(v) {
			return ErrCredentialEnv
		}
	}
	for _, r := range s.Egress {
		if r.Host == "" && r.CIDR == "" {
			return ErrRuleTarget
		}
	}
	return nil
}

func trimmedEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// credentialPrefixes lists the prefixes of token shapes that a plain env
// value must not carry.
var credentialPrefixes = []string{
	"ghp_", "gho_", "ghs_", "github_pat_", "glpat-", "sk-", "xoxb-", "xoxp-",
	"-----BEGIN ", "Bearer ",
}

// looksLikeCredential reports whether a value has the shape of a token. The
// check is a shape test. It does not prove that a value is a secret.
func looksLikeCredential(v string) bool {
	for _, p := range credentialPrefixes {
		if len(v) >= len(p) && v[:len(p)] == p {
			return true
		}
	}
	if len(v) >= 20 && v[:4] == "AKIA" && allUpperAlnum(v[4:20]) {
		return true
	}
	if len(v) >= 3 && v[:3] == "eyJ" && countDots(v) == 2 {
		return true
	}
	return false
}

func allUpperAlnum(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func countDots(s string) int {
	n := 0
	for _, r := range s {
		if r == '.' {
			n++
		}
	}
	return n
}
