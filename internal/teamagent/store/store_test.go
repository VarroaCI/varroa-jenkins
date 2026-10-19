package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// freshPool creates a migrated database that only this test uses, and drops it
// when the test ends. It skips when VARROA_TEST_DSN is not set.
func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VARROA_TEST_DSN")
	if dsn == "" {
		t.Skip("VARROA_TEST_DSN is not set; skipping Team agent store database test")
	}
	ctx := context.Background()
	name := "tastore_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:20]
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	_ = admin.Close(ctx)

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
	return pool
}

func TestUpsertSpend_RollsUpOneUTCDay(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 18, 23, 59, 0, 0, time.UTC)
	_, err := pool.Exec(ctx, `
		INSERT INTO varroa.agent_sessions (id, kind, principal, team, cluster, trigger, context,
		  provider, model, key_source, data_sharing, instructions_sha, team_agent_generation,
		  state, skip_reason, prompt_tokens, completion_tokens, steps, lease_owner,
		  lease_expires_at, started_at, ended_at)
		VALUES ($1, 'team_agent', 'agent:ns/t/x', 't', 'core', 'run.failed', '{}',
		  'p', 'm', 'chart', 'logs', '', 1, 'completed', NULL, 100, 50, 1, NULL, NULL, $2, $2)`,
		uuid.Must(uuid.NewV7()), day)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	s := New(pool, func() time.Time { return day })
	if err := s.UpsertSpend(ctx, "t", day); err != nil {
		t.Fatalf("UpsertSpend: %v", err)
	}
	if err := s.UpsertSpend(ctx, "t", day); err != nil {
		t.Fatalf("second UpsertSpend: %v", err)
	}
	got, err := s.TeamDaySpend(ctx, "t", day)
	if err != nil {
		t.Fatalf("TeamDaySpend: %v", err)
	}
	if got != 150 {
		t.Fatalf("TeamDaySpend = %d, want 150 (prompt plus completion, counted once)", got)
	}
	next, err := s.TeamDaySpend(ctx, "t", day.AddDate(0, 0, 1))
	if err != nil || next != 0 {
		t.Fatalf("next UTC day = %d, %v; want 0", next, err)
	}
}

func TestLookups_EmptyAnswers(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	s := New(pool, nil)
	if id, err := s.RunID(ctx, "t", "build", 7); err != nil || id != "" {
		t.Fatalf("RunID on an empty table = %q, %v", id, err)
	}
	if _, ok, err := s.LivePlacement(ctx, "t"); err != nil || ok {
		t.Fatalf("LivePlacement on an empty table ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LastSession(ctx, "agent:ns/t/x"); err != nil || ok {
		t.Fatalf("LastSession on an empty table ok=%v err=%v", ok, err)
	}
	ids, err := s.ResumeCandidates(ctx, []string{"t"}, time.Now().Add(-time.Hour))
	if err != nil || len(ids) != 0 {
		t.Fatalf("ResumeCandidates on an empty table = %v, %v", ids, err)
	}
}
