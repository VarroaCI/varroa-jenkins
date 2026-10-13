package recordstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestApplyRunStartedFillsDownstreamStoredBeforeProducer asserts a child
// that started before its producer keeps the producer's run id with a NULL
// tenant, and that the producer's own start fills the NULL columns.
func TestApplyRunStartedFillsDownstreamStoredBeforeProducer(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	producer := uuid.Must(uuid.NewV7())
	childKey := "upstream:" + producer.String() + ":1"
	seedUpstreamOrigin(t, pool, childKey)
	childID := seedGrantedIntent(t, pool, childKey)
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, childKey, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	before := readUpstreamColumns(t, db, childID)
	if !before.runID.Valid || before.runID.String != producer.String() || before.tenant.Valid {
		t.Fatalf("child before the producer starts = %+v, want the run id alone", before)
	}

	req := newTestClaimRequest("manual:late-producer", 0)
	req.RunID = producer.String()
	if resp := NewLedger(pool, nil).Claim(context.Background(), req); resp.Decision != DecisionGranted {
		t.Fatalf("producer claim = %+v, want granted", resp)
	}
	number := int64(7)
	mustApply(t, g, newTestEnvelope(t, producer, "manual:late-producer", KindRunStarted, RunStartedPayload{BuildNumber: &number}))

	after := readUpstreamColumns(t, db, childID)
	if !after.runID.Valid || after.runID.String != producer.String() {
		t.Fatalf("upstream_run_id = %+v, want %s", after.runID, producer)
	}
	if after.tenant.String != testTenant || after.job.String != testJobPath || after.number.Int64 != number {
		t.Fatalf("filled tenant/job/number = %q/%q/%d, want %q/%q/%d",
			after.tenant.String, after.job.String, after.number.Int64, testTenant, testJobPath, number)
	}
}
