// Package testdb gives the Team agent database tests a migrated database of
// their own. It is for tests only; it never runs in the product.
package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// FreshDB creates a migrated database that only the calling test uses, and
// drops it when the test ends. It returns the pool and the DSN of the new
// database. It skips when VARROA_TEST_DSN is not set.
func FreshDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("VARROA_TEST_DSN")
	if dsn == "" {
		t.Skip("VARROA_TEST_DSN is not set; skipping Team agent database test")
	}
	ctx := context.Background()
	name := "tadb_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:20]
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	target := u.String()
	if err := recordstore.RunMigrations(ctx, target); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, target)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, target
}
