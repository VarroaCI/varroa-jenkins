package recordstore

import (
	"context"
	"slices"
	"testing"
	"time"
)

const rerunTestSHA = "0123456789abcdef0123456789abcdef01234567"

// applyRerunInventory stores one cell snapshot that holds a live change
// request, an orphaned change request, a branch head, and a second tenant
// that holds the same pull-request URL. One envelope carries the whole cell,
// because a later envelope replaces the cell's inventory.
func applyRerunInventory(t *testing.T, g *Ingester) {
	t.Helper()
	if _, err := g.Apply(context.Background(), newJobBranchesEnvelope(t, jsTestCell, time.Now().UTC(), JobBranchesPayload{
		Hash: "h1",
		Projects: []JobBranchProjectInput{
			{
				Tenant:              "acme",
				PlacementGeneration: 1,
				JobPath:             "acme/app",
				IndexedAt:           "2026-10-25T10:00:00Z",
				IndexResult:         "SUCCESS",
				Heads: []JobBranchHeadInput{
					{Name: "PR-7", JobPath: "acme/app/PR-7", Kind: "changeRequest",
						ChangeRequest: &ChangeRequestBranchInput{ID: "7", URL: "https://github.com/Acme/App/pull/7"}},
					{Name: "PR-8", JobPath: "acme/app/PR-8", Kind: "changeRequest", Orphaned: true,
						ChangeRequest: &ChangeRequestBranchInput{ID: "8", URL: "https://github.com/acme/app/pull/8"}},
					{Name: "main", JobPath: "acme/app/main", Kind: "branch", Revision: rerunTestSHA},
				},
			},
			{
				Tenant:              "beta",
				PlacementGeneration: 1,
				JobPath:             "beta/app",
				IndexedAt:           "2026-10-25T10:00:00Z",
				IndexResult:         "SUCCESS",
				Heads: []JobBranchHeadInput{
					{Name: "PR-7", JobPath: "beta/app/PR-7", Kind: "changeRequest",
						ChangeRequest: &ChangeRequestBranchInput{ID: "7", URL: "https://github.com/acme/app/pull/7"}},
				},
			},
		},
	})); err != nil {
		t.Fatalf("apply cell inventory: %v", err)
	}
}

func TestChangeRequestJobs_MatchesURLCaseInsensitiveAndSkipsOrphaned(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()
	applyRerunInventory(t, g)

	got, err := reader.ChangeRequestJobs(ctx, jsTestCluster, "acme", "https://github.com/acme/app/pull/7")
	if err != nil {
		t.Fatalf("change request jobs: %v", err)
	}
	if !slices.Equal(got, []string{"acme/app/PR-7"}) {
		t.Fatalf("jobs = %v, want the live PR-7 head", got)
	}

	got, err = reader.ChangeRequestJobs(ctx, jsTestCluster, "acme", "https://github.com/acme/app/pull/8")
	if err != nil {
		t.Fatalf("orphaned change request jobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("jobs = %v, want none for an orphaned head", got)
	}
}

func TestChangeRequestJobs_OtherTenantNotMatched(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()
	applyRerunInventory(t, g)

	got, err := reader.ChangeRequestJobs(ctx, jsTestCluster, "acme", "https://github.com/acme/app/pull/7")
	if err != nil {
		t.Fatalf("change request jobs: %v", err)
	}
	if slices.Contains(got, "beta/app/PR-7") {
		t.Fatalf("jobs = %v, the tenant beta head leaked into tenant acme", got)
	}
}

func TestRevisionJobs_MatchesBranchHeadOnly(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	reader := NewReader(pool)
	ctx := context.Background()
	applyRerunInventory(t, g)

	got, err := reader.RevisionJobs(ctx, jsTestCluster, "acme", rerunTestSHA)
	if err != nil {
		t.Fatalf("revision jobs: %v", err)
	}
	if !slices.Equal(got, []string{"acme/app/main"}) {
		t.Fatalf("jobs = %v, want the branch head only", got)
	}
}

func TestRevisionJobs_RejectsNonHexRevision(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	for _, revision := range []string{"", "main", "0123", rerunTestSHA + "0"} {
		if _, err := reader.RevisionJobs(context.Background(), jsTestCluster, "acme", revision); err == nil {
			t.Fatalf("revision %q: want an error", revision)
		}
	}
}
