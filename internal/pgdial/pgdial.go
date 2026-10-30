// Package pgdial opens Postgres pools and connections for the record store
// and the agent store. Production code opens its database through
// this package. It applies DefaultConnectTimeout when the DSN sets no
// connect_timeout of its own, so a silent or unreachable server fails
// within seconds. The DSN's own connect_timeout wins.
package pgdial

import (
	"context"
	"database/sql"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// DefaultConnectTimeout bounds one connect attempt when the DSN sets no
// connect_timeout.
const DefaultConnectTimeout = 5 * time.Second

// PoolConfig parses dsn into a pool config and applies the default connect
// timeout when the DSN sets none.
func PoolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	applyDefault(&cfg.ConnConfig.Config)
	return cfg, nil
}

// ConnConfig parses dsn into a connection config and applies the default
// connect timeout when the DSN sets none.
func ConnConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	applyDefault(&cfg.Config)
	return cfg, nil
}

// NewPool opens a lazy pool for dsn with the default connect timeout. It
// does not connect until the first use.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := PoolConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// OpenDB returns a database/sql handle for dsn with the default connect
// timeout. It does not connect until the first use.
func OpenDB(dsn string) (*sql.DB, error) {
	cfg, err := ConnConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg), nil
}

// applyDefault sets DefaultConnectTimeout as the connect timeout when the
// config has none.
func applyDefault(cfg *pgconn.Config) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
}
