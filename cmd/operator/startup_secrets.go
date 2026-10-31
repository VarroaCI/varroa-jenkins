package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/varroaci/varroa-jenkins/internal/bootsecret"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

const (
	caSecretName             = "varroa-ca"
	miteSigningKeySecretName = "varroa-mite-signing-key"
	// startupSecretTimeout is shared by both Secrets. It stays under the chart startupProbe budget of 240 s.
	startupSecretTimeout = 2 * time.Minute
	startupSecretPoll    = 3 * time.Second
)

// mintCA creates a new CA and returns its PEM data for the varroa-ca Secret.
func mintCA() (map[string][]byte, error) {
	authority, err := ca.NewCA()
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}
	certPEM, keyPEM, err := authority.Persist()
	if err != nil {
		return nil, fmt.Errorf("export CA: %w", err)
	}
	return map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}, nil
}

// mintMiteSigningKey creates a new RS256 signing key and returns its PEM data.
func mintMiteSigningKey() (map[string][]byte, error) {
	signer, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		return nil, fmt.Errorf("generate mite signing key: %w", err)
	}
	privPEM, err := signer.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("export mite signing key: %w", err)
	}
	return map[string][]byte{"private-key": []byte(privPEM)}, nil
}

// loadOrCreateCA returns the CA in the varroa-ca Secret. The bool is true when
// this call created the Secret.
func loadOrCreateCA(ctx context.Context, client kubernetes.Interface, namespace string, opts bootsecret.Options) (*ca.CA, bool, error) {
	data, created, err := bootsecret.LoadOrCreate(ctx, client,
		bootsecret.Ref{Namespace: namespace, Name: caSecretName, Keys: []string{"tls.crt", "tls.key"}},
		mintCA, opts)
	if err != nil {
		return nil, false, err
	}
	authority, err := ca.LoadCA(data["tls.crt"], data["tls.key"])
	if err != nil {
		return nil, false, fmt.Errorf("parse CA in Secret %s/%s: %w", namespace, caSecretName, err)
	}
	return authority, created, nil
}

// loadOrCreateMiteSigningKey returns the signer in the varroa-mite-signing-key
// Secret. The bool is true when this call created the Secret.
func loadOrCreateMiteSigningKey(ctx context.Context, client kubernetes.Interface, namespace string, opts bootsecret.Options) (*mitesrv.MiteTokenSigner, bool, error) {
	data, created, err := bootsecret.LoadOrCreate(ctx, client,
		bootsecret.Ref{Namespace: namespace, Name: miteSigningKeySecretName, Keys: []string{"private-key"}},
		mintMiteSigningKey, opts)
	if err != nil {
		return nil, false, err
	}
	signer, err := mitesrv.NewMiteTokenSignerFromPEM(data["private-key"])
	if err != nil {
		return nil, false, fmt.Errorf("parse mite signing key in Secret %s/%s: %w", namespace, miteSigningKeySecretName, err)
	}
	return signer, created, nil
}

// loadStartupSecrets loads or creates the CA and the mite signing key before
// the operator starts. Both calls share one deadline.
func loadStartupSecrets(client kubernetes.Interface, namespace string, logger *slog.Logger) (*ca.CA, *mitesrv.MiteTokenSigner, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startupSecretTimeout)
	defer cancel()
	opts := bootsecret.Options{Poll: startupSecretPoll, Logger: logger}

	authority, caCreated, err := loadOrCreateCA(ctx, client, namespace, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("startup secret %s: %w", caSecretName, err)
	}
	if caCreated {
		logger.Info("CA created and persisted to Secret", "namespace", namespace)
	} else {
		logger.Info("loaded existing CA from Secret", "namespace", namespace)
	}

	signer, keyCreated, err := loadOrCreateMiteSigningKey(ctx, client, namespace, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("startup secret %s: %w", miteSigningKeySecretName, err)
	}
	if keyCreated {
		logger.Info("mite signing key created and persisted to Secret",
			"namespace", namespace, "kid", signer.KID())
	} else {
		logger.Info("loaded existing mite signing key from Secret",
			"namespace", namespace, "kid", signer.KID())
	}
	return authority, signer, nil
}
