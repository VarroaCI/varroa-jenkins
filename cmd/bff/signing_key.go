package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/varroaci/varroa-jenkins/internal/bootsecret"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

const (
	miteSigningKeySecretName = "varroa-mite-signing-key"
	// signingKeyWaitTimeout stays under the chart startupProbe budget of 240 s.
	signingKeyWaitTimeout = 2 * time.Minute
	signingKeyPoll        = 3 * time.Second
)

// loadMiteSigningKey reads the operator-created varroa-mite-signing-key
// Secret until it holds a private key, or until timeout. It never writes
// the Secret. A deadline error wraps bootsecret.ErrNotFound only when the
// last answer was not-found. A read error is never absence.
func loadMiteSigningKey(ctx context.Context, client kubernetes.Interface, namespace string,
	timeout, poll time.Duration, logger *slog.Logger) (*mitesrv.MiteTokenSigner, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := bootsecret.Wait(ctx, client,
		bootsecret.Ref{Namespace: namespace, Name: miteSigningKeySecretName, Keys: []string{"private-key"}},
		bootsecret.Options{Poll: poll, Logger: logger})
	if err != nil {
		return nil, err
	}
	signer, err := mitesrv.NewMiteTokenSignerFromPEM(data["private-key"])
	if err != nil {
		return nil, fmt.Errorf("parse mite signing key in Secret %s/%s: %w", namespace, miteSigningKeySecretName, err)
	}
	return signer, nil
}
