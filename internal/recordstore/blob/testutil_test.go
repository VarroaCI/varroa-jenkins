package blob_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// testDSNEnv is the environment variable a blob store database test
// reads to find a live Postgres instance. A test skips when it is unset,
// so make test stays green with no database present.
const testDSNEnv = "VARROA_TEST_DSN"

// blobDatabaseSuffix is appended to the DSN's database name to form the
// database this test binary owns.
const blobDatabaseSuffix = "_blob"

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

var (
	blobDSNOnce sync.Once
	blobDSN     string
	blobDSNErr  error
)

// blobTestDSN returns a DSN for this binary's own database, named after
// the DSN's database plus blobDatabaseSuffix. It creates the database
// when it is absent. The parent package's tests drop and recreate schema
// "varroa" in the DSN's database throughout their run, and go test runs
// the two test binaries at the same time, so a blob test that shared that
// database would lose its tables mid-test. The DSN must be a URL
// (postgres://...), the form every documented invocation uses, and its
// role must hold CREATEDB.
func blobTestDSN(t *testing.T, dsn string) string {
	t.Helper()
	blobDSNOnce.Do(func() {
		blobDSN, blobDSNErr = ensureBlobDatabase(context.Background(), dsn)
	})
	if blobDSNErr != nil {
		t.Fatalf("prepare blob test database: %v", blobDSNErr)
	}
	return blobDSN
}

// ensureBlobDatabase creates the blob database when it is absent and
// returns dsn rewritten to name it.
func ensureBlobDatabase(ctx context.Context, dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn as url: %w", err)
	}
	base := strings.TrimPrefix(u.Path, "/")
	if base == "" {
		return "", fmt.Errorf("dsn %q names no database", dsn)
	}
	name := base + blobDatabaseSuffix

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look up database %q: %w", name, err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			return "", fmt.Errorf("create database %q: %w", name, err)
		}
	}

	u.Path = "/" + name
	return u.String(), nil
}

// setupTestPool drops and recreates schema "varroa" in this binary's own
// database, applies every migration, and returns a pool over the fresh
// schema. The database persists between runs, and the initial migration
// creates only the partitions around the month it runs in, so a schema
// kept from an earlier month would reject every new row.
//
// This file imports internal/recordstore for RunMigrations, and
// internal/recordstore's production code imports blob for its retention
// key helpers. Declaring this file's package blob_test, not blob, keeps
// that import out of the cycle: an in-package blob test file cannot
// import a package that imports blob back, but an external blob_test
// file can.
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := blobTestDSN(t, requireTestDSN(t))
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS varroa CASCADE"); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("drop schema varroa: %v", err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}

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
