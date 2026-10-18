package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPostgresContract runs the shared contract against the Postgres store.
// It skips when VARROA_TEST_DSN is unset.
func TestPostgresContract(t *testing.T) {
	t.Parallel()
	runContract(t, func(t *testing.T, o Options) Store {
		p, err := Open(context.Background(), freshDSN(t), o)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(p.Close)
		return p
	})
}

// TestDaySpendUsesPrincipalIndex asserts that the day-spend query reads through
// agent_sessions_principal_started_idx when the planner may use the index.
func TestDaySpendUsesPrincipalIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, err := Open(ctx, freshDSN(t), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(p.Close)
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}
	start := startOfDay(time.Now().UTC())
	rows, err := conn.Query(ctx, "EXPLAIN "+daySpendSQL, "alice", start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "agent_sessions_principal_started_idx") {
		t.Fatalf("day spend plan does not use the principal index:\n%s", plan.String())
	}
}

// TestOpenRefusesSchema37 asserts that Open refuses a schema below the minimum.
func TestOpenRefusesSchema37(t *testing.T) {
	t.Parallel()
	dsn := freshDSN(t)
	ctx := context.Background()
	p, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := p.pool.Exec(ctx, "DELETE FROM varroa.goose_db_version WHERE version_id >= 38"); err != nil {
		t.Fatalf("lower schema: %v", err)
	}
	p.Close()
	if _, err := Open(ctx, dsn, Options{}); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("Open at 37 = %v, want ErrSchemaTooOld", err)
	}
}

// TestCheckSchemaVersion is pure: it checks the version gate only.
func TestCheckSchemaVersion(t *testing.T) {
	t.Parallel()
	if err := checkSchemaVersion(37); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("version 37 = %v, want ErrSchemaTooOld", err)
	}
	for _, v := range []int64{38, 39} {
		if err := checkSchemaVersion(v); err != nil {
			t.Fatalf("version %d = %v, want nil", v, err)
		}
	}
}
