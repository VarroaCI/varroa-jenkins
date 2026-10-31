package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
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

// TestSchemaGateRefusesSchema37UntilMigrated asserts that the schema gate
// refuses a schema below the minimum, lets a migrated schema through, and
// keeps the pass when the schema is lowered again.
func TestSchemaGateRefusesSchema37UntilMigrated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, err := Open(ctx, freshDSN(t), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(p.Close)
	setApplied := func(applied bool) {
		t.Helper()
		if _, err := p.pool.Exec(ctx, "UPDATE varroa.goose_db_version SET is_applied = $1 WHERE version_id >= 38", applied); err != nil {
			t.Fatalf("set applied for version 38 and up: %v", err)
		}
	}

	setApplied(false)
	if _, err := p.GetSettings(ctx, agent.ScopeGlobal); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("GetSettings at 37 = %v, want ErrSchemaTooOld", err)
	}
	if err := p.CheckSchema(ctx); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("CheckSchema at 37 = %v, want ErrSchemaTooOld", err)
	}

	setApplied(true)
	if _, err := p.GetSettings(ctx, agent.ScopeGlobal); err != nil {
		t.Fatalf("GetSettings after migration: %v", err)
	}

	setApplied(false)
	if _, err := p.GetSettings(ctx, agent.ScopeGlobal); err != nil {
		t.Fatalf("GetSettings after a pass, with the schema lowered again: %v", err)
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
