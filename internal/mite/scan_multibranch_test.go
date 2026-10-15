package mite

import (
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// scanMultibranchCommand returns one SCAN_MULTIBRANCH imperative for the job
// the trigger tests already resolve.
func scanMultibranchCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "scan-multibranch-0199d0b7-0000-7000-8000-000000000012",
		Type:      mitev1.CommandTypeScanMultibranch,
		ScanMultibranch: &mitev1.ScanMultibranchPayload{
			ScanId:  "0199d0b7-0000-7000-8000-000000000012",
			JobPath: triggerFailedJob,
		},
	}
}

// TestRecordCommandOutcomeScanAcks asserts a refused scan publishes no
// record and still acknowledges the command. The plugin tracker reports
// the refusal through the branch inventory.
func TestRecordCommandOutcomeScanAcks(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, scanMultibranchCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId: "scan-multibranch-0199d0b7-0000-7000-8000-000000000012",
		Success:   false,
		Error:     "scan multibranch: HTTP 403: Forbidden",
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}
