package recordstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// writeBadgeTokenPlacement writes one announce for testTenant with token as
// its badge token, in its own transaction.
func writeBadgeTokenPlacement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, token string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	req := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", CellInstance: "pod-a",
		BadgeToken: token,
	}
	if err := writeTenantPlacement(ctx, tx, req, 1, "", nil, nil, nil, nil, placementIdentity{}); err != nil {
		t.Fatalf("write tenant placement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestWriteTenantPlacement_StoresAndClearsBadgeToken asserts an announce stores
// its badge token, and that an announce with an empty token stores NULL.
func TestWriteTenantPlacement_StoresAndClearsBadgeToken(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := t.Context()
	read := func() (bool, string) {
		t.Helper()
		var set bool
		var token string
		if err := pool.QueryRow(ctx, `
			SELECT badge_token IS NOT NULL, COALESCE(badge_token, '')
			FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
		`, testCluster, testTenant).Scan(&set, &token); err != nil {
			t.Fatalf("read tenant placement: %v", err)
		}
		return set, token
	}

	writeBadgeTokenPlacement(ctx, t, pool, "tok-stored-0123456789")
	if set, token := read(); !set || token != "tok-stored-0123456789" {
		t.Fatalf("stored badge token = (set %v, %q), want the announced token", set, token)
	}
	writeBadgeTokenPlacement(ctx, t, pool, "")
	if set, _ := read(); set {
		t.Fatalf("badge token is set after the switch went off, want NULL")
	}
}

// TestTenantBadgeToken_MissingRowAndNullAnswerEmpty asserts a missing row and a
// NULL column both answer "" with a nil error.
func TestTenantBadgeToken_MissingRowAndNullAnswerEmpty(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := t.Context()

	got, err := reader.TenantBadgeToken(ctx, testCluster, testTenant)
	if err != nil || got != "" {
		t.Fatalf("missing row = (%q, %v), want empty and nil", got, err)
	}
	writeBadgeTokenPlacement(ctx, t, pool, "")
	got, err = reader.TenantBadgeToken(ctx, testCluster, testTenant)
	if err != nil || got != "" {
		t.Fatalf("NULL column = (%q, %v), want empty and nil", got, err)
	}
}

// TestTenantBadgeToken_TornDownRowAnswersEmpty asserts a placement row that a
// teardown ended, at generation 0, answers no token.
func TestTenantBadgeToken_TornDownRowAnswersEmpty(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := t.Context()

	writeBadgeTokenPlacement(ctx, t, pool, "tok-torn-0123456789")
	if _, err := pool.Exec(ctx, `
		UPDATE varroa.tenant_placements SET generation = 0
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("tear down placement: %v", err)
	}
	got, err := reader.TenantBadgeToken(ctx, testCluster, testTenant)
	if err != nil || got != "" {
		t.Fatalf("torn-down row = (%q, %v), want empty and nil", got, err)
	}
}

// TestTenantBadgeToken_AnswersStoredToken asserts the reader answers the token
// that the announce stored.
func TestTenantBadgeToken_AnswersStoredToken(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := t.Context()

	writeBadgeTokenPlacement(ctx, t, pool, "tok-answered-0123456789")
	got, err := reader.TenantBadgeToken(ctx, testCluster, testTenant)
	if err != nil {
		t.Fatalf("TenantBadgeToken: %v", err)
	}
	if got != "tok-answered-0123456789" {
		t.Fatalf("token = %q, want the stored token", got)
	}
}
