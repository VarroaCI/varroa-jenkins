package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
	"github.com/varroaci/varroa-jenkins/internal/agent/sealedkey"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api"
)

// buildAssistant returns the assistant runtime for this BFF replica. It
// returns nil and no error when the assistant is off or cannot start. A
// broken assistant config is logged, and the BFF starts without the
// assistant.
func buildAssistant(ctx context.Context, cfgRaw, dsn, cluster, signingPEM string, publish func(string, []byte) error, logger *slog.Logger) (*api.AssistantRuntime, error) {
	if cfgRaw == "" {
		return nil, nil
	}
	cfg, err := provider.Decode([]byte(cfgRaw))
	if err != nil {
		logger.Error("assistant config is invalid; the assistant is off", "error", err.Error())
		return nil, nil
	}
	if dsn == "" {
		logger.Error("assistant needs VARROA_RECORDSTORE_DSN; the assistant is off")
		return nil, nil
	}
	st, err := store.Open(ctx, dsn, store.Options{})
	if err != nil {
		logger.Error("assistant store did not open; the assistant is off", "error", err.Error())
		return nil, nil
	}
	reg, err := provider.NewRegistry(cfg, factories.DefaultFactories(), provider.DefaultKeyDir)
	if err != nil {
		logger.Error("assistant providers did not load; the assistant is off", "error", err.Error())
		return nil, nil
	}
	var sealer *sealedkey.Sealer
	if signingPEM != "" {
		if sealer, err = sealedkey.New(signingPEM); err != nil {
			return nil, fmt.Errorf("assistant key sealer: %w", err)
		}
	}
	return api.NewAssistantRuntime(cfg, reg, st, sealer, assistantOwner(), cluster, publish, logger), nil
}

// assistantOwner names this replica as a lease owner. The hostname keeps the
// name readable, and the random suffix keeps two replicas apart.
func assistantOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "bff"
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return host
	}
	return host + "-" + hex.EncodeToString(b[:])
}
