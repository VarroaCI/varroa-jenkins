package mite

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestRecorderClaimBudgetIsInsideTheGatewayBudget pins the inward budget:
// the recorder finishes a claim before the gateway stops waiting for it.
func TestRecorderClaimBudgetIsInsideTheGatewayBudget(t *testing.T) {
	if recordstore.ClaimBudget >= defaultLedgerClaimTimeout {
		t.Fatalf("recorder claim budget %s must be below the gateway timeout %s",
			recordstore.ClaimBudget, defaultLedgerClaimTimeout)
	}
}
