package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
	"github.com/varroaci/varroa-jenkins/internal/agent/sealedkey"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api"
)

// buildAssistant returns the assistant runtime for this BFF replica. It
// returns nil and no error when the assistant is off or cannot start: its
// config is invalid, its DSN is unset, or its providers do not load. A
// broken assistant config is logged, and the BFF starts without the
// assistant. A store that does not answer at startup does not turn the
// assistant off. The store connects on its first call, and the calls answer
// 503 until it answers. Every store call runs under that bound.
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
		st.Close()
		logger.Error("assistant providers did not load; the assistant is off", "error", err.Error())
		return nil, nil
	}
	var sealer *sealedkey.Sealer
	if signingPEM != "" {
		if sealer, err = sealedkey.New(signingPEM); err != nil {
			st.Close()
			return nil, fmt.Errorf("assistant key sealer: %w", err)
		}
	}
	go logAssistantStoreState(ctx, startupSchemaTimeout, st, logger)
	return api.NewAssistantRuntime(cfg, reg, store.WithTimeout(st, assistantStoreCallTimeout), sealer, assistantOwner(), cluster, publish, logger), nil
}

// assistantStoreCallTimeout bounds each assistant store call. A variable so a
// test can shorten it.
var assistantStoreCallTimeout = store.DefaultCallTimeout

// logAssistantStoreState checks the store schema once in the background and
// logs the result. It runs only for the operator's log, so the BFF does not
// wait for it. The timeout is passed in so a test can shorten it.
func logAssistantStoreState(ctx context.Context, timeout time.Duration, st *store.Postgres, logger *slog.Logger) {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := st.CheckSchema(checkCtx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The BFF stops before the check ends. The store has no fault to report.
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		logger.Warn("assistant store unreachable at startup; assistant calls answer 503 until it returns", "error", err.Error())
	default:
		logger.Warn("assistant store schema check failed; assistant calls answer 503 until it passes", "error", err.Error())
	}
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
