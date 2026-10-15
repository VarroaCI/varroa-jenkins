package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// Defaults for the log writer's env-configured values, matching
// design.md D4 and the C3 registry.
const (
	defaultLogPendingBytes  = 2147483648 // 2 GiB, VARROA_GATEWAY_LOG_PENDING_BYTES
	defaultFinalizeWaitMS   = 60000      // VARROA_RECORDSTORE_LOG_FINALIZE_WAIT
	defaultRunOwnerCacheMax = 50000      // VARROA_GATEWAY_RUN_OWNER_CACHE
)

// openGatewayBlobStore opens the blob store the log writer appends to.
// dsn is VARROA_RECORDSTORE_DSN (set only in postgres blob mode); blobURL
// is VARROA_BLOBSTORE_URL. A dsn with no blobURL means the record store
// is enabled but misconfigured, and startup fails with a clear message.
// Neither set means the record store is disabled: it returns a nil store
// and no error, and the gateway's OnLogChunk becomes a no-op.
func openGatewayBlobStore(ctx context.Context, dsn, blobURL string, pool *pgxpool.Pool) (blob.Store, error) {
	if blobURL == "" {
		if dsn != "" {
			return nil, fmt.Errorf("VARROA_RECORDSTORE_DSN is set but VARROA_BLOBSTORE_URL is empty: " +
				"the record store is enabled with no blob store configured")
		}
		return nil, nil
	}
	return blob.Open(ctx, blobURL, pool)
}

// logPendingBytesFromEnv resolves VARROA_GATEWAY_LOG_PENDING_BYTES,
// defaulting to 2 GiB.
func logPendingBytesFromEnv() int64 {
	raw := os.Getenv("VARROA_GATEWAY_LOG_PENDING_BYTES")
	if raw == "" {
		return defaultLogPendingBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultLogPendingBytes
	}
	return n
}

// logFinalizeWaitFromEnv resolves VARROA_RECORDSTORE_LOG_FINALIZE_WAIT
// (milliseconds), defaulting to 60000 ms. The gateway reads the exact
// same variable the recorder's finalize sweep does, so the two never
// disagree about the deadline.
func logFinalizeWaitFromEnv() time.Duration {
	raw := os.Getenv("VARROA_RECORDSTORE_LOG_FINALIZE_WAIT")
	if raw == "" {
		return time.Duration(defaultFinalizeWaitMS) * time.Millisecond
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return time.Duration(defaultFinalizeWaitMS) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// runOwnerCacheSizeFromEnv resolves VARROA_GATEWAY_RUN_OWNER_CACHE,
// defaulting to 50000 entries.
func runOwnerCacheSizeFromEnv() int {
	raw := os.Getenv("VARROA_GATEWAY_RUN_OWNER_CACHE")
	if raw == "" {
		return defaultRunOwnerCacheMax
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultRunOwnerCacheMax
	}
	return n
}
