package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/pgdial"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// startupSchemaTimeout bounds the schema check that runs at BFF startup. A
// database that does not answer within it does not stop the BFF from
// starting. It is a variable so a test can shorten it.
var startupSchemaTimeout = 10 * time.Second

// openRecordStoreReader opens the record-store pool and the Reader over it.
// Every read goes through recordstore.WithTimeout. The reader is returned
// even when the schema check fails: an unreachable store recovers on its
// own, because each Reader call checks the schema again.
func openRecordStoreReader(ctx context.Context, dsn string, retentionDays int, logger *slog.Logger) (recordstore.Reader, *pgxpool.Pool, error) {
	pool, err := pgdial.NewPool(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	reader := recordstore.WithTimeout(recordstore.NewReader(pool, retentionDays), recordstore.DefaultReadTimeout)
	checkCtx, cancel := context.WithTimeout(ctx, startupSchemaTimeout)
	defer cancel()
	if _, err := reader.SchemaVersion(checkCtx); err != nil {
		if errors.Is(err, recordstore.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("record store unreachable at startup; record-store reads answer 503 until it returns", "error", err)
		} else {
			logger.Warn("record store schema is older than this BFF requires; record-store reads will return an error", "error", err)
		}
	}
	return reader, pool, nil
}
