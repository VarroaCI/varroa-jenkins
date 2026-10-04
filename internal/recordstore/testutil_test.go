package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// testDSNEnv is the environment variable a record-store database test reads
// to find a live Postgres instance. A test skips when it is unset, so
// `make test` stays green in a container with no database.
const testDSNEnv = "VARROA_TEST_DSN"

// Database name suffixes this test binary owns, after the DSN's own
// database name. The blob test binary owns "_blob", so no suffix here may
// start with "_blob".
const (
	templateDatabaseSuffix = "_tmpl"
	testDatabaseInfix      = "_t"
)

// runLockMargin is the time before the test binary's own deadline at which
// the wait for the run lock stops. A held lock then fails with its own
// message, not with the generic test timeout panic.
const runLockMargin = time.Minute

// requireTestDSN returns the test database DSN, or skips the calling test
// when the environment gives none. A test that needs a migrated schema
// uses setupTestDB or setupTestDBDSN instead: the DSN's own database holds
// no schema this binary prepares.
func requireTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping record-store database test", testDSNEnv)
	}
	return dsn
}

// testDBState is the per-binary state of the test databases: the admin
// connection to the DSN's database, the session that holds the run lock,
// the migrated template, and a counter for the per-test database names.
type testDBState struct {
	baseDSN  string
	baseName string
	admin    *sql.DB
	lock     *sql.Conn
	template string
	next     int
}

var (
	testDBOnce sync.Once
	testDBInit *testDBState
	testDBErr  error
	// testDBMu guards the per-test database counter only. Parallel tests
	// create and drop their databases at the same time, because a lock
	// around those statements makes every test wait on a drop checkpoint.
	testDBMu sync.Mutex
)

// cloneRetryAttempts and cloneRetryDelay bound the retry of a clone that
// Postgres refuses with SQLSTATE 55006. Postgres refuses a clone while a
// session is connected to the source database.
const (
	cloneRetryAttempts = 20
	cloneRetryDelay    = 250 * time.Millisecond
)

// testDatabases prepares the template database once per test binary and
// returns the shared state. It takes the run lock, drops every per-test
// database an earlier run left behind, then migrates a fresh template. The
// DSN must be a URL and its role must hold CREATEDB.
func testDatabases(t *testing.T) *testDBState {
	t.Helper()
	dsn := requireTestDSN(t)
	testDBOnce.Do(func() {
		deadline, ok := t.Deadline()
		if ok {
			deadline = deadline.Add(-runLockMargin)
		}
		testDBInit, testDBErr = prepareTestDatabases(context.Background(), dsn, deadline)
	})
	if testDBErr != nil {
		t.Fatalf("prepare record-store test databases: %v", testDBErr)
	}
	return testDBInit
}

// prepareTestDatabases waits for the run lock until lockDeadline. A zero
// lockDeadline waits with no limit.
func prepareTestDatabases(ctx context.Context, dsn string, lockDeadline time.Time) (*testDBState, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn as url: %w", err)
	}
	base := strings.TrimPrefix(u.Path, "/")
	if base == "" {
		return nil, fmt.Errorf("dsn %q names no database", dsn)
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open admin connection: %w", err)
	}
	s := &testDBState{baseDSN: dsn, baseName: base, admin: admin, template: base + templateDatabaseSuffix}

	// The run lock keeps a second test binary on the same DSN from
	// dropping the template while this binary clones it. The session holds
	// the lock until the binary exits, so the second binary waits until
	// its own test deadline is close.
	s.lock, err = admin.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open run lock session: %w", err)
	}
	for {
		var got bool
		if err := s.lock.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", s.template).Scan(&got); err != nil {
			return nil, fmt.Errorf("take run lock: %w", err)
		}
		if got {
			break
		}
		if !lockDeadline.IsZero() && time.Now().After(lockDeadline) {
			return nil, fmt.Errorf("another test binary still holds the run lock on %s at the test deadline", s.template)
		}
		time.Sleep(time.Second)
	}

	if err := s.dropLeftovers(ctx); err != nil {
		return nil, err
	}
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{s.template}.Sanitize()+" WITH (FORCE)"); err != nil {
		return nil, fmt.Errorf("drop template database: %w", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{s.template}.Sanitize()); err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	if err := RunMigrations(ctx, s.dsnFor(s.template)); err != nil {
		return nil, fmt.Errorf("migrate template database: %w", err)
	}
	return s, nil
}

// dropLeftovers drops the per-test databases of an earlier run. A
// database that still has a session belongs to a live run, so its drop
// error is ignored.
func (s *testDBState) dropLeftovers(ctx context.Context) error {
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(s.baseName+testDatabaseInfix) + `[0-9]+_[0-9]+$`)
	rows, err := s.admin.QueryContext(ctx, "SELECT datname FROM pg_database WHERE NOT datistemplate")
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan database name: %w", err)
		}
		if pattern.MatchString(name) {
			names = append(names, name)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close database list: %w", err)
	}
	for _, name := range names {
		_, _ = s.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
	}
	return nil
}

// dsnFor returns the base DSN rewritten to name database.
func (s *testDBState) dsnFor(database string) string {
	u, _ := url.Parse(s.baseDSN)
	u.Path = "/" + database
	return u.String()
}

// newDatabase creates one database for t from source and registers a
// cleanup that drops it. The drop forces every session closed, so a
// goroutine that still holds a connection cannot keep the database.
func (s *testDBState) newDatabase(t *testing.T, source string) string {
	t.Helper()
	ctx := context.Background()

	testDBMu.Lock()
	s.next++
	n := s.next
	testDBMu.Unlock()
	name := fmt.Sprintf("%s%s%d_%d", s.baseName, testDatabaseInfix, os.Getpid(), n)
	if err := s.cloneDatabase(ctx, name, source); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}

	t.Cleanup(func() {
		if _, err := s.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return s.dsnFor(name)
}

// cloneDatabase creates database name from source. It retries a clone
// that Postgres refuses because a session is connected to source.
func (s *testDBState) cloneDatabase(ctx context.Context, name, source string) error {
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " + pgx.Identifier{source}.Sanitize()
	var err error
	for attempt := 0; attempt < cloneRetryAttempts; attempt++ {
		if _, err = s.admin.ExecContext(ctx, stmt); err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return err
		}
		if attempt == cloneRetryAttempts-1 {
			break
		}
		time.Sleep(cloneRetryDelay)
	}
	return err
}

// setupTestDBDSN gives t its own database, cloned from the migrated
// template, and returns a connection to it plus its DSN. A test that opens
// a second connection or a pool uses that DSN, never the environment's.
func setupTestDBDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	s := testDatabases(t)
	dsn := s.newDatabase(t, s.template)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db, dsn
}

// setupTestDB gives t its own migrated database and returns a connection
// to it.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := setupTestDBDSN(t)
	return db
}

// emptyTestDB gives t its own empty database, with no schema "varroa", and
// returns its DSN. A migration test applies the migrations itself.
func emptyTestDB(t *testing.T) string {
	t.Helper()
	return testDatabases(t).newDatabase(t, "template0")
}

// serialGuardEnv is the environment variable requireSerialTest sets. Its
// value has no meaning.
const serialGuardEnv = "VARROA_TEST_SERIAL_GUARD"

// requireSerialTest fails t when t runs in parallel. t.Setenv panics in
// a parallel test, so a helper that changes process-wide state calls this
// first.
func requireSerialTest(t *testing.T) {
	t.Helper()
	t.Setenv(serialGuardEnv, "1")
}

// useTestMeterProvider installs a meter provider with a manual reader as
// the global provider until t ends, and returns the reader. Every metrics
// constructor in this package reads the global provider, so the calling
// test must not run in parallel. List it in serialDatabaseTests when it
// uses the database.
func useTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	requireSerialTest(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}
