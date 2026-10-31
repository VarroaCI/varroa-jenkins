// Package bootsecret reads the Secrets a component needs before it starts.
// Only a not-found answer lets a caller mint a new value. A mint creates the
// Secret exclusively, so every replica uses one stored copy. A read error
// never mints.
package bootsecret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrNotFound reports that the API server answered that the Secret does not exist.
var ErrNotFound = errors.New("secret not found")

var errNoDeadline = errors.New("bootsecret: the context has no deadline")

// Ref names a Secret and the keys that must hold a value.
type Ref struct {
	Namespace string
	Name      string
	Keys      []string // each key must be present and non-empty
}

// Options tunes the retry loop.
type Options struct {
	Poll   time.Duration // pause between attempts; <= 0 means one second
	Logger *slog.Logger  // nil means slog.Default()
}

// IncompleteError reports a stored Secret that lacks a required key.
type IncompleteError struct {
	Namespace string
	Name      string
	Key       string
}

// Error returns the message for the incomplete Secret.
func (e *IncompleteError) Error() string {
	return fmt.Sprintf("secret %s/%s exists but has no %q key; it is not replaced", e.Namespace, e.Name, e.Key)
}

// MintFunc creates the data for a Secret that does not exist.
type MintFunc func() (map[string][]byte, error)

func (o Options) poll() time.Duration {
	if o.Poll <= 0 {
		return time.Second
	}
	return o.Poll
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// Wait reads the Secret until it exists with every required key, or until the
// context deadline passes. It never writes. A deadline error wraps ErrNotFound
// when the last answer was not-found, and wraps *IncompleteError when the last
// answer was a Secret that lacked a required key.
func Wait(ctx context.Context, client kubernetes.Interface, ref Ref, opts Options) (map[string][]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errNoDeadline
	}
	logger := opts.logger()
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := pause(ctx, opts.poll()); err != nil {
				return nil, expired(ref, lastErr, err)
			}
		}
		secret, err := client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			if key := missingKey(secret.Data, ref.Keys); key != "" {
				// The writer may still be filling the Secret. Keep polling;
				// the deadline error names the incomplete key.
				lastErr = &IncompleteError{Namespace: ref.Namespace, Name: ref.Name, Key: key}
				logger.Info("secret is incomplete; waiting", "namespace", ref.Namespace, "secret", ref.Name, "key", key)
				continue
			}
			return secret.Data, nil
		}
		if ctx.Err() != nil {
			return nil, expired(ref, lastErr, ctx.Err())
		}
		if apierrors.IsNotFound(err) {
			lastErr = ErrNotFound
			logger.Info("waiting for the secret", "namespace", ref.Namespace, "secret", ref.Name)
			continue
		}
		lastErr = err
		logger.Warn("read the secret failed; retrying", "namespace", ref.Namespace, "secret", ref.Name, "error", err)
	}
}

// LoadOrCreate returns the stored Secret data. It mints and creates the Secret
// only after the API server answers not-found. The create is exclusive: when
// another replica wins the race, this call reads the stored copy. The bool is
// true only when this call created the Secret. A Secret that lacks a required
// key is an error, and LoadOrCreate never replaces it.
func LoadOrCreate(ctx context.Context, client kubernetes.Interface, ref Ref, mint MintFunc, opts Options) (map[string][]byte, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, false, errNoDeadline
	}
	logger := opts.logger()
	var (
		minted   map[string][]byte
		haveMint bool
		lastErr  error
	)
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := pause(ctx, opts.poll()); err != nil {
				return nil, false, expired(ref, lastErr, err)
			}
		}
		secret, err := client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			if key := missingKey(secret.Data, ref.Keys); key != "" {
				return nil, false, &IncompleteError{Namespace: ref.Namespace, Name: ref.Name, Key: key}
			}
			return secret.Data, false, nil
		}
		if ctx.Err() != nil {
			return nil, false, expired(ref, lastErr, ctx.Err())
		}
		if !apierrors.IsNotFound(err) {
			// A read error is not absence. Retry and never mint.
			lastErr = err
			logger.Warn("read the startup secret failed; retrying", "namespace", ref.Namespace, "secret", ref.Name, "error", err)
			continue
		}

		if !haveMint {
			minted, err = mint()
			if err != nil {
				return nil, false, fmt.Errorf("mint secret %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			haveMint = true
		}
		_, err = client.CoreV1().Secrets(ref.Namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
			Data:       minted,
		}, metav1.CreateOptions{})
		if err == nil {
			return minted, true, nil
		}
		if apierrors.IsAlreadyExists(err) {
			lastErr = err
			logger.Info("another replica created the startup secret; reading it", "namespace", ref.Namespace, "secret", ref.Name)
			continue
		}
		if ctx.Err() != nil {
			return nil, false, expired(ref, lastErr, ctx.Err())
		}
		// A lost response can hide a Create that succeeded. The next Get covers it.
		lastErr = err
		logger.Warn("create the startup secret failed; retrying", "namespace", ref.Namespace, "secret", ref.Name, "error", err)
	}
}

// missingKey returns the first required key with an empty value, or "".
func missingKey(data map[string][]byte, keys []string) string {
	for _, key := range keys {
		if len(data[key]) == 0 {
			return key
		}
	}
	return ""
}

// pause waits for d or until ctx is done.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// expired builds the deadline error. It names the last answer when one exists.
func expired(ref Ref, lastErr, ctxErr error) error {
	cause := lastErr
	if cause == nil {
		cause = ctxErr
	}
	return fmt.Errorf("secret %s/%s is not usable before the deadline: %w", ref.Namespace, ref.Name, cause)
}
