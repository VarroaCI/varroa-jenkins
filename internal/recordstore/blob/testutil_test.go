package blob_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// testDSNEnv is the environment variable a blob store database test
// reads to find a live Postgres instance. A test skips when it is unset,
// so make test stays green with no database present.
const testDSNEnv = "VARROA_TEST_DSN"

// requireTestDSN returns the test database DSN, or skips the calling
// test when the environment gives none.
func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping blob store database test", testDSNEnv)
	}
	return dsn
}

// setupTestPool ensures schema "varroa" exists and holds every migration,
// then returns a pool over it. It never drops the schema: the parent
// package's own tests reset the same physical schema throughout their
// run, and a blob test that dropped it too would race that reset instead
// of merely reusing it. RunMigrations is idempotent and lock-protected,
// so two packages calling it at the same time are safe. A blob test
// cleans up the keys it wrote instead, so a repeated run starts clean.
//
// This file imports internal/recordstore for RunMigrations, and
// internal/recordstore's production code imports blob for its retention
// key helpers. Declaring this file's package blob_test, not blob, keeps
// that import out of the cycle: an in-package blob test file cannot
// import a package that imports blob back, but an external blob_test
// file can.
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := requireTestDSN(t)
	ctx := context.Background()

	if err := recordstore.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
