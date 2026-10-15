package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedGrantedIntent claims intentKey against ledger and returns the
// granted run_id as a uuid.UUID. It fails the test unless the claim is
// granted, so a caller can build an ingest envelope for a run the ledger
// already knows about.
func seedGrantedIntent(t *testing.T, pool *pgxpool.Pool, intentKey string) uuid.UUID {
	t.Helper()
	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest(intentKey, 0)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted claim, got %+v", resp)
	}
	return uuid.MustParse(resp.RunID)
}

// newTestEnvelope returns a well-formed IngestEnvelope for runID and kind,
// with the test coordinates newTestClaimRequest uses and the given
// payload marshaled into the envelope's Payload field.
func newTestEnvelope(t *testing.T, runID uuid.UUID, intentKey, kind string, payload any) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          kind,
		RunID:         runID.String(),
		IntentKey:     intentKey,
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}
}
