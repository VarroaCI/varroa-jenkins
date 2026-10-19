package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSelectMarkedLostIntentsCarriesSourceFacts checks that a marked intent
// whose run row is a QUEUED placeholder carries the commit and repository
// that the placeholder recorded.
func TestSelectMarkedLostIntentsCarriesSourceFacts(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", true)
	seedMarkedPlacement(t, db, true, `{}`)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
		                         status, scm_revision, payload)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'QUEUED', 'abc123',
		        '{"accepted_at": "2026-10-19T00:00:00Z", "run.source": {"scm_url": "https://git.example.org/team/app"}}'::jsonb)
	`, runID, "manual:"+runID.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed queued placeholder: %v", err)
	}

	got, err := selectMarkedLostIntents(context.Background(), pool, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("select marked lost intents: %v", err)
	}
	for _, c := range got {
		if c.runID != runID {
			continue
		}
		if c.scmRevision != "abc123" || c.scmURL != "https://git.example.org/team/app" {
			t.Fatalf("revision %q url %q", c.scmRevision, c.scmURL)
		}
		if cand := c.candidate(); cand.scmRevision != "abc123" || cand.scmURL == "" {
			t.Fatalf("candidate lost the source facts: %+v", cand)
		}
		return
	}
	t.Fatalf("marked intent for %s not selected; got %d rows", runID, len(got))
}
