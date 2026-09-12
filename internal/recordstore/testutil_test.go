package recordstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// testDSNEnv is the environment variable a record-store database test reads
// to find a live Postgres instance. A test skips when it is unset, so
// `make test` stays green in a container with no database.
const testDSNEnv = "VARROA_TEST_DSN"

// requireTestDSN returns the test database DSN, or skips the calling test
// when the environment gives none.
func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping record-store database test", testDSNEnv)
	}
	return dsn
}

// setupTestDB drops and recreates schema "varroa" against the test
// database, applies every migration, and returns a connection to the fresh
// schema. It registers a cleanup that closes the connection, so a test
// leaves the database ready for the next test run.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := requireTestDSN(t)
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		_ = admin.Close()
		t.Fatalf("drop schema varroa: %v", err)
	}
	if err := admin.Close(); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}

	if err := RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}
