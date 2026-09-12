package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrateLockKey is the pg_advisory_lock key the migration runner holds
// while it applies migrations. The retention ticker uses a different fixed
// key on its own connection pool, so the two never contend.
const migrateLockKey = 8271340192837465

// goosePackageMu serializes every call into the goose package's global
// setters (SetTableName) together with the goose.Up call that depends on
// them. goose keeps that state in unsynchronized package variables, so two
// concurrent RunMigrations calls must not set and read them at the same
// time.
var goosePackageMu sync.Mutex

func init() {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		// "postgres" is a goose-supported dialect name. A build where this
		// fails cannot run any migration, so it fails at package load.
		panic(err)
	}
}

// RunMigrations creates schema "varroa" when it is absent, applies every
// pending migration under a session-scoped Postgres advisory lock, then
// grants the read-only client role its privileges. Two callers racing
// against the same database apply the migrations exactly once between
// them; the loser observes the schema already at the target version and
// returns nil.
func RunMigrations(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}

	// The advisory lock belongs to one Postgres session, and goose takes a
	// *sql.DB rather than a single connection. Capping the pool at one
	// connection pins the lock, the migration, and the unlock to that same
	// session.
	db := sql.OpenDB(stdlib.GetConnector(*cfg))
	defer func() {
		_ = db.Close()
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS varroa"); err != nil && !isConcurrentSchemaCreate(err) {
		return fmt.Errorf("create schema varroa: %w", err)
	}

	goosePackageMu.Lock()
	defer goosePackageMu.Unlock()

	goose.SetTableName("varroa.goose_db_version")

	if _, err := db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	if err := goose.Up(db, migrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	if err := EnsureGrants(ctx, db); err != nil {
		return fmt.Errorf("ensure grants: %w", err)
	}

	return nil
}

// isConcurrentSchemaCreate reports whether err is the unique-key violation
// Postgres raises when two sessions run CREATE SCHEMA IF NOT EXISTS for the
// same schema name at the same time. Postgres does not make the existence
// check and the create atomic across sessions, so a race loser sees this
// error even though the schema now exists. A loser in that race treats the
// error as success.
func isConcurrentSchemaCreate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
