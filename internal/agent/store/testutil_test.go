package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// testDSNEnv names the environment variable that holds the test database DSN.
// A test that needs Postgres skips when it is unset.
const testDSNEnv = "VARROA_TEST_DSN"

// The agent store owns the databases "<db>_agent_tmpl" and
// "<db>_agent_t<pid>_<n>". The record-store harness owns "<db>_tmpl" and
// "<db>_t<pid>_<n>", so the two binaries never drop each other's databases.
const (
	templateSuffix = "_agent_tmpl"
	testInfix      = "_agent_t"
)

// agentDB holds the migrated template of this test binary.
var agentDB struct {
	once     sync.Once
	err      error
	baseDSN  string
	baseName string
	template string
	admin    *sql.DB
	mu       sync.Mutex
	next     int
}

// requirePostgres returns the base DSN, or skips the test when none is set.
func requirePostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping agent store database test", testDSNEnv)
	}
	return dsn
}

// dsnFor rewrites the base DSN to name database.
func dsnFor(base, database string) string {
	u, _ := url.Parse(base)
	u.Path = "/" + database
	return u.String()
}

// prepareTemplate creates and migrates the template database once per binary.
func prepareTemplate(t *testing.T) {
	t.Helper()
	dsn := requirePostgres(t)
	agentDB.once.Do(func() {
		agentDB.err = buildTemplate(dsn)
	})
	if agentDB.err != nil {
		t.Fatalf("prepare agent store template: %v", agentDB.err)
	}
}

func buildTemplate(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	base := strings.TrimPrefix(u.Path, "/")
	if base == "" {
		return fmt.Errorf("dsn %q names no database", dsn)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open admin connection: %w", err)
	}
	agentDB.baseDSN = dsn
	agentDB.baseName = base
	agentDB.template = base + templateSuffix
	agentDB.admin = admin

	ctx := context.Background()
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(base+testInfix) + `[0-9]+_[0-9]+$`)
	rows, err := admin.QueryContext(ctx, "SELECT datname FROM pg_database WHERE NOT datistemplate")
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	var leftovers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan database name: %w", err)
		}
		if pattern.MatchString(name) {
			leftovers = append(leftovers, name)
		}
	}
	_ = rows.Close()
	for _, name := range leftovers {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}
	tmpl := pgx.Identifier{agentDB.template}.Sanitize()
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+tmpl+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop template database: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+tmpl); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	if err := recordstore.RunMigrations(ctx, dsnFor(dsn, agentDB.template)); err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	return nil
}

// freshDSN clones the migrated template into a database that only t uses,
// and returns its DSN. The database is dropped when t ends.
func freshDSN(t *testing.T) string {
	t.Helper()
	prepareTemplate(t)
	agentDB.mu.Lock()
	agentDB.next++
	n := agentDB.next
	agentDB.mu.Unlock()
	name := fmt.Sprintf("%s%s%d_%d", agentDB.baseName, testInfix, os.Getpid(), n)
	ctx := context.Background()
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " + pgx.Identifier{agentDB.template}.Sanitize()
	if _, err := agentDB.admin.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("clone test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = agentDB.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return dsnFor(agentDB.baseDSN, name)
}

// testClock is a clock that the test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

// Now returns the current test time.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the test time forward.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
