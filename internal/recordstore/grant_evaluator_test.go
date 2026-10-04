package recordstore

import (
	"context"
	"database/sql"
	"testing"
)

// seedGrantPlacement inserts one varroa.tenant_placements row with an
// explicit namespace, cell, generation, and policy, for a GrantEvaluator
// test that needs full control over every compared field.
func seedGrantPlacement(t *testing.T, db *sql.DB, cluster, tenant, namespace, cell string, generation int64, policyJSON string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, generation, policy)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
	`, cluster, tenant, namespace, cell, generation, policyJSON); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// TestCheckPlacementOrders covers the placement-fencing capability's five
// orders across the four call-site paths, including the path distinction
// on order 1. The member order runs before the generation order, and the
// generation order answers the retryable NOT_READY.
func TestCheckPlacementOrders(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	evaluator := NewGrantEvaluator(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, "core", "acme", "ns-a", "cell-a", 3, "{}")
	seedGrantPlacement(t, db, "core", "zero-gen", "ns-zero", "cell-zero", 0, "{}")

	baseline := func(path GrantPath) PlacementInput {
		return PlacementInput{
			Path:                path,
			Cluster:             "core",
			Namespace:           "ns-a",
			Cell:                "cell-a",
			Tenant:              "acme",
			PlacementGeneration: 3,
		}
	}

	tests := []struct {
		name      string
		in        PlacementInput
		wantAllow bool
		wantCode  string
	}{
		{
			name:      "order 1: a claim with no placement row continues",
			in:        PlacementInput{Path: GrantPathClaim, Cluster: "core", Tenant: "ghost"},
			wantAllow: true,
		},
		{
			name:      "order 1: a request with no placement row is vetoed",
			in:        PlacementInput{Path: GrantPathRequest, Cluster: "core", Tenant: "ghost"},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name:      "order 1: a lock with no placement row is vetoed",
			in:        PlacementInput{Path: GrantPathLock, Cluster: "core", Tenant: "ghost"},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name:      "order 1: a fan-out with no placement row is a retryable skip",
			in:        PlacementInput{Path: GrantPathFanout, Cluster: "core", Tenant: "ghost"},
			wantAllow: false,
			wantCode:  ReasonNotReady,
		},
		{
			name: "a single-tenant controller with no row at all is never fenced",
			in: PlacementInput{
				Path: GrantPathClaim, Cluster: "core", Namespace: "ns-zero", Cell: "cell-zero",
				Tenant: "no-row-at-all", PlacementGeneration: 0,
			},
			wantAllow: true,
		},
		{
			name: "order 2: a generation-zero row on both sides allows",
			in: PlacementInput{
				Path: GrantPathRequest, Cluster: "core", Namespace: "ns-zero", Cell: "cell-zero",
				Tenant: "zero-gen", PlacementGeneration: 0,
			},
			wantAllow: true,
		},
		{
			name:      "order 5: the claim path allows a matching row",
			in:        baseline(GrantPathClaim),
			wantAllow: true,
		},
		{
			name:      "order 5: the request path allows a matching row",
			in:        baseline(GrantPathRequest),
			wantAllow: true,
		},
		{
			name: "order 4: the claim path holds a stale generation",
			in: func() PlacementInput {
				p := baseline(GrantPathClaim)
				p.PlacementGeneration = 2
				return p
			}(),
			wantAllow: false,
			wantCode:  ReasonNotReady,
		},
		{
			name: "order 4: the lock path holds a stale generation",
			in: func() PlacementInput {
				p := baseline(GrantPathLock)
				p.PlacementGeneration = 2
				return p
			}(),
			wantAllow: false,
			wantCode:  ReasonNotReady,
		},
		{
			name: "order 3: the request path vetoes a cell of no member",
			in: func() PlacementInput {
				p := baseline(GrantPathRequest)
				p.Cell = "cell-b"
				return p
			}(),
			wantAllow: false,
			wantCode:  ReasonFenced,
		},
		{
			name: "order 3: the claim path vetoes a namespace of no member",
			in: func() PlacementInput {
				p := baseline(GrantPathClaim)
				p.Namespace = "ns-b"
				return p
			}(),
			wantAllow: false,
			wantCode:  ReasonFenced,
		},
		{
			name: "the fan-out path ignores generation and cell once placed",
			in: func() PlacementInput {
				p := baseline(GrantPathFanout)
				p.PlacementGeneration = 999
				p.Cell = "wrong-cell"
				return p
			}(),
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluator.CheckPlacement(ctx, tc.in)
			if err != nil {
				t.Fatalf("CheckPlacement: %v", err)
			}
			if got.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (result %+v)", got.Allow, tc.wantAllow, got)
			}
			if !tc.wantAllow && got.ReasonCode != tc.wantCode {
				t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, tc.wantCode)
			}
		})
	}
}

// TestCheckOrders covers the tenant-export-grant capability's eight
// grant orders on both resource kinds.
func TestCheckOrders(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	evaluator := NewGrantEvaluator(pool)
	ctx := context.Background()

	// The caller's own cell hosts tenant "alpha".
	seedGrantPlacement(t, db, "core", "alpha", "ns-caller", "cell-caller", 0, "{}")
	// Tenant "beta" exports the job "api" and the lock glob "deploy-*" to
	// "alpha".
	seedGrantPlacement(t, db, "core", "beta", "ns-target", "cell-target", 0,
		`{"exports":[{"job":"api","callers":["alpha"]},{"locks":["deploy-*"],"callers":["alpha"]}]}`)

	callerPlacement := PlacementInput{
		Path: GrantPathRequest, Cluster: "core", Namespace: "ns-caller",
		Cell: "cell-caller", Tenant: "alpha", PlacementGeneration: 0,
	}

	tests := []struct {
		name      string
		in        GrantInput
		wantAllow bool
		wantCode  string
	}{
		{
			name: "order 6: an ordinary build costs no grant lookup",
			in: GrantInput{
				PlacementInput: callerPlacement,
				CallerTenant:   "",
				TargetTenant:   "no-such-tenant-anywhere",
				ResourceKind:   GrantResourceJob,
				Resource:       "beta/api",
			},
			wantAllow: true,
		},
		{
			name: "order 7: a same-tenant call needs no grant",
			in: GrantInput{
				PlacementInput: callerPlacement,
				CallerTenant:   "alpha",
				TargetTenant:   "alpha",
				ResourceKind:   GrantResourceJob,
				Resource:       "alpha/anything",
			},
			wantAllow: true,
		},
		{
			name: "order 8: a job with no sanctioned scheme is refused despite a grant",
			in: GrantInput{
				PlacementInput:  callerPlacement,
				CallerTenant:    "alpha",
				TargetTenant:    "beta",
				ResourceKind:    GrantResourceJob,
				Resource:        "beta/api",
				IntentKeyScheme: IntentSchemeManual,
			},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name: "order 8: a retry scheme is accepted",
			in: GrantInput{
				PlacementInput:  callerPlacement,
				CallerTenant:    "alpha",
				TargetTenant:    "beta",
				ResourceKind:    GrantResourceJob,
				Resource:        "beta/api",
				IntentKeyScheme: IntentSchemeRetry,
			},
			wantAllow: true,
		},
		{
			name: "order 8 does not apply to a lock",
			in: GrantInput{
				PlacementInput: callerPlacement,
				CallerTenant:   "alpha",
				TargetTenant:   "beta",
				ResourceKind:   GrantResourceLock,
				Resource:       "deploy-prod",
			},
			wantAllow: true,
		},
		{
			name: "order 11: a job outside every export pattern is refused",
			in: GrantInput{
				PlacementInput:  callerPlacement,
				CallerTenant:    "alpha",
				TargetTenant:    "beta",
				ResourceKind:    GrantResourceJob,
				Resource:        "beta/web",
				IntentKeyScheme: IntentSchemeUpstream,
			},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name: "order 11: a lock outside every export glob is refused",
			in: GrantInput{
				PlacementInput: callerPlacement,
				CallerTenant:   "alpha",
				TargetTenant:   "beta",
				ResourceKind:   GrantResourceLock,
				Resource:       "staging-db",
			},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name: "order 12: an unlisted caller is refused for a job",
			in: GrantInput{
				PlacementInput:  callerPlacement,
				CallerTenant:    "payments",
				TargetTenant:    "beta",
				ResourceKind:    GrantResourceJob,
				Resource:        "beta/api",
				IntentKeyScheme: IntentSchemeUpstream,
			},
			wantAllow: false,
			wantCode:  ReasonCrossTenant,
		},
		{
			name: "order 13: a granted job caller passes",
			in: GrantInput{
				PlacementInput:  callerPlacement,
				CallerTenant:    "alpha",
				TargetTenant:    "beta",
				ResourceKind:    GrantResourceJob,
				Resource:        "beta/api",
				IntentKeyScheme: IntentSchemeUpstream,
			},
			wantAllow: true,
		},
		{
			name: "order 13: a granted lock caller passes",
			in: GrantInput{
				PlacementInput: callerPlacement,
				CallerTenant:   "alpha",
				TargetTenant:   "beta",
				ResourceKind:   GrantResourceLock,
				Resource:       "deploy-prod",
			},
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluator.Check(ctx, tc.in)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (result %+v)", got.Allow, tc.wantAllow, got)
			}
			if !tc.wantAllow && got.ReasonCode != tc.wantCode {
				t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, tc.wantCode)
			}
		})
	}
}

// TestCheckOrder12UnionOfOverlappingExportEntries asserts order 12 grants
// a caller listed on any matching entry, not only the first one a
// broader glob also matches.
func TestCheckOrder12UnionOfOverlappingExportEntries(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	evaluator := NewGrantEvaluator(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, "core", "payments", "ns-caller", "cell-caller", 0, "{}")
	// "beta" exports "release/*" to "search" and, in a second, more
	// specific entry, "release/prod" to "payments". Both entries match
	// "release/prod".
	seedGrantPlacement(t, db, "core", "beta", "ns-target", "cell-target", 0,
		`{"exports":[{"job":"release/*","callers":["search"]},{"job":"release/prod","callers":["payments"]}]}`)

	got, err := evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path: GrantPathRequest, Cluster: "core", Namespace: "ns-caller",
			Cell: "cell-caller", Tenant: "payments", PlacementGeneration: 0,
		},
		CallerTenant:    "payments",
		TargetTenant:    "beta",
		ResourceKind:    GrantResourceJob,
		Resource:        "beta/release/prod",
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !got.Allow {
		t.Fatalf("Allow = false, want true (result %+v): payments is granted through the second matching entry", got)
	}
}

// TestCheckNoSuchTargetTenant asserts order 9: a target tenant with no
// placement row anywhere is refused, naming the tenant.
func TestCheckNoSuchTargetTenant(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	evaluator := NewGrantEvaluator(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, "core", "alpha", "ns-caller", "cell-caller", 0, "{}")

	got, err := evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path: GrantPathRequest, Cluster: "core", Namespace: "ns-caller",
			Cell: "cell-caller", Tenant: "alpha", PlacementGeneration: 0,
		},
		CallerTenant:    "alpha",
		TargetTenant:    "ghost",
		ResourceKind:    GrantResourceJob,
		Resource:        "ghost/api",
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Allow {
		t.Fatal("want a veto for a target tenant that does not exist, got granted")
	}
	if got.ReasonCode != ReasonCrossTenant {
		t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, ReasonCrossTenant)
	}
}

// TestCheckAmbiguousTargetTenant asserts order 10: a target tenant name
// that exists in two clusters, and not in the caller's own, is refused
// rather than guessed.
func TestCheckAmbiguousTargetTenant(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	evaluator := NewGrantEvaluator(pool)
	ctx := context.Background()

	seedGrantPlacement(t, db, "core-x", "gamma", "ns-x", "cell-x", 0, "{}")
	seedGrantPlacement(t, db, "core-y", "gamma", "ns-y", "cell-y", 0, "{}")

	got, err := evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			// The claim path never vetoes on a missing row, so the
			// caller's own cluster needs no placement row of its own
			// for this test to reach the grant orders.
			Path: GrantPathClaim, Cluster: "core-caller", Tenant: "gamma", PlacementGeneration: 0,
		},
		CallerTenant:    "alpha",
		TargetTenant:    "gamma",
		ResourceKind:    GrantResourceJob,
		Resource:        "gamma/api",
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Allow {
		t.Fatal("want a veto for an ambiguous target tenant, got granted")
	}
	if got.ReasonCode != ReasonCrossTenant {
		t.Fatalf("ReasonCode = %q, want %q", got.ReasonCode, ReasonCrossTenant)
	}
}
