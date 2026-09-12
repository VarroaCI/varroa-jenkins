package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupLedgerPool prepares a fresh schema through setupTestDB, then opens
// a pgxpool.Pool over the same DSN for the ledger under test. It returns
// both so a test can assert on rows through db while exercising the
// ledger through pool.
func setupLedgerPool(t *testing.T) (*pgxpool.Pool, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	dsn := requireTestDSN(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool, db
}
