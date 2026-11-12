package activity

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestAckedBudgetOutlastsDeadConnection proves the retry budget covers a
// silent bus partition plus one reconnect.
func TestAckedBudgetOutlastsDeadConnection(t *testing.T) {
	const reconnectAllowance = 30 * time.Second
	if budget := defaultAckedConfig().budget; bus.DeadConnectionBound+reconnectAllowance > budget {
		t.Fatalf("bus.DeadConnectionBound %s plus %s for a reconnect exceeds the %s retry budget", bus.DeadConnectionBound, reconnectAllowance, budget)
	}
}
