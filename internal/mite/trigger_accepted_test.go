package mite

import (
	"errors"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// acceptedTriggerResult is the result an accepted TRIGGER_BUILD command
// reports.
func acceptedTriggerResult() *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    true,
		HttpStatus: 202,
	}
}

// TestAcceptedTriggerPublishesEnvelope asserts a successful TRIGGER_BUILD
// result publishes one run.trigger_accepted envelope, and acknowledges the
// pending imperative message after the publish.
func TestAcceptedTriggerPublishesEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, acceptedTriggerResult())

	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunTriggerAccepted {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunTriggerAccepted)
	}
	if env.RunID != "0199d0b7-0000-7000-8000-000000000010" {
		t.Fatalf("run_id = %q", env.RunID)
	}
	if env.IntentKey != "manual:token-1" {
		t.Fatalf("intent_key = %q", env.IntentKey)
	}
	if env.JobPath != triggerFailedJob {
		t.Fatalf("job_path = %q", env.JobPath)
	}
	if env.Tenant != "team-a" {
		t.Fatalf("tenant = %q, want team-a", env.Tenant)
	}
	if env.Cluster != h.cluster || env.Namespace != triggerFailedNS || env.Cell != triggerFailedCell {
		t.Fatalf("identity = %q/%q/%q", env.Cluster, env.Namespace, env.Cell)
	}
	if env.OccurredAt.IsZero() {
		t.Fatal("occurred_at is zero")
	}
	if got := env.Payload["http_status"]; got != float64(202) {
		t.Fatalf("http_status = %v, want 202", got)
	}
	if got := env.Payload["actor"]; got != "user:alice" {
		t.Fatalf("actor = %v", got)
	}
	if _, ok := env.Payload["reason_code"]; ok {
		t.Fatal("payload carries a reason_code, want none")
	}
	if _, ok := env.Payload["error"]; ok {
		t.Fatal("payload carries an error, want none")
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestAcceptedPublishLeavesMessageUnacknowledged asserts the gateway acks the
// imperative only after a successful publish, so the stream redelivers it.
func TestAcceptedPublishLeavesMessageUnacknowledged(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("stream not found")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, acceptedTriggerResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	requireStaysUnacknowledged(t, conn, stream, consumer)
}

// TestAcceptedTriggerWithNoRecordStorePublishesNothing asserts an install with
// no record store acknowledges the command and publishes nothing.
func TestAcceptedTriggerWithNoRecordStorePublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	h.SetRecordStoreEnabled(false)
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, acceptedTriggerResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestAcceptedUnresolvedTenantPublishesNothing asserts a cell that hosts no
// tenant for the job path writes no record, and acknowledges the pending
// message.
func TestAcceptedUnresolvedTenantPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, stubJobTenantResolver{})
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, acceptedTriggerResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestAcceptedCancelPublishesNothing asserts the gateway keys on the command
// type, so a successful CANCEL_BUILD writes no record.
func TestAcceptedCancelPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	cmd := &mitev1.ImperativeCommand{
		CommandId:   "cmd-1",
		Type:        mitev1.CommandTypeCancelBuild,
		CancelBuild: &mitev1.CancelBuildPayload{RunId: "r1", JobPath: triggerFailedJob},
	}
	stream, consumer, _ := seedPendingImperative(t, h, conn, cmd)

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    true,
		HttpStatus: 200,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}
