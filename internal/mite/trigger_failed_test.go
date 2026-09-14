package mite

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	triggerFailedNS   = "ns1"
	triggerFailedCell = "cell1"
	triggerFailedJob  = "team-a/build"
)

// triggerFailedResolver resolves the one job path these tests use.
func triggerFailedResolver() CellTenantResolver {
	return stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		triggerFailedJob: {tenant: "team-a", generation: 3},
	}}
}

// seedPendingImperative publishes one imperative command onto the mite out
// subject, fetches it through a durable pull consumer, and files the fetched
// message in pendingAcks. It returns the stream name, the consumer name, and
// the fetched message, so a test can read the consumer's pending count and
// model a redelivery.
func seedPendingImperative(t *testing.T, h *BusHandler, conn *bus.Conn, cmd *mitev1.ImperativeCommand) (string, string, *nats.Msg) {
	t.Helper()
	subject := bus.MiteOutSubject(h.cluster, triggerFailedNS, triggerFailedCell)
	stream := "imp_test"
	consumer := "imp_test_consumer"
	if err := conn.EnsureStream(&nats.StreamConfig{
		Name:     stream,
		Subjects: []string{subject},
		Storage:  nats.MemoryStorage,
	}); err != nil {
		t.Fatalf("ensure imperative stream: %v", err)
	}
	if err := conn.EnsureConsumer(stream, consumer, &nats.ConsumerConfig{
		Durable:       consumer,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    -1,
		FilterSubject: subject,
	}); err != nil {
		t.Fatalf("ensure imperative consumer: %v", err)
	}

	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd},
	})
	if err != nil {
		t.Fatalf("marshal imperative: %v", err)
	}
	if _, err := conn.PublishJetStream(subject, data); err != nil {
		t.Fatalf("publish imperative: %v", err)
	}

	sub, err := conn.PullSubscribe(subject, stream, consumer)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	msgs, err := conn.DrainConsumer(sub, 1)
	if err != nil {
		t.Fatalf("fetch imperative: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(msgs))
	}

	filePendingImperative(h, cmd.CommandId, msgs[0])
	return stream, consumer, msgs[0]
}

// filePendingImperative files msg as the pending imperative message of
// commandID, as the gateway's own imperative consumer does on every delivery.
func filePendingImperative(h *BusHandler, commandID string, msg *nats.Msg) {
	key := triggerFailedNS + "/" + triggerFailedCell
	h.mu.Lock()
	h.pendingAcks[key] = map[string]pendingImperative{commandID: {msg: msg}}
	h.mu.Unlock()
}

// newTriggerFailedHandler builds a BusHandler over a live NATS server, with
// pub as the build-subject publish seam and the record store declared enabled.
func newTriggerFailedHandler(t *testing.T, pub buildEventPublisher, resolver CellTenantResolver) (*BusHandler, *bus.Conn) {
	t.Helper()
	h, conn := newJobScheduleTestHandler(t, pub, resolver)
	h.pendingAcks = make(map[string]map[string]pendingImperative)
	h.replayCmds = make(map[string]map[string]bool)
	h.SetRecordStoreEnabled(true)
	return h, conn
}

// failedTriggerResult is the result a refused TRIGGER_BUILD command reports.
func failedTriggerResult() *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    false,
		Error:      "trigger build team-a/build: HTTP 404: no such job",
		HttpStatus: 404,
	}
}

func triggerBuildCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "cmd-1",
		Type:      mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{
			RunId:     "0199d0b7-0000-7000-8000-000000000010",
			IntentKey: "manual:token-1",
			JobPath:   triggerFailedJob,
			Actor:     "user:alice",
		},
	}
}

// ackPending reports the consumer's unacknowledged message count.
func ackPending(t *testing.T, conn *bus.Conn, stream, consumer string) int {
	t.Helper()
	info, err := conn.ConsumerInfo(stream, consumer)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	return info.NumAckPending
}

// waitAckAll polls the consumer until it holds no unacknowledged message. A
// JetStream acknowledgement travels on its own subject, so one read can
// observe the count before the server applied that acknowledgement.
func waitAckAll(t *testing.T, conn *bus.Conn, stream, consumer string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := -1
	for time.Now().Before(deadline) {
		got = ackPending(t, conn, stream, consumer)
		if got == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("unacknowledged messages = %d, want 0", got)
}

// requireStaysUnacknowledged asserts the consumer keeps exactly one
// unacknowledged message over a short window, so a late acknowledgement cannot
// pass the assertion.
func requireStaysUnacknowledged(t *testing.T, conn *bus.Conn, stream, consumer string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := ackPending(t, conn, stream, consumer); got != 1 {
			t.Fatalf("unacknowledged messages = %d, want 1", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFailedTriggerPublishesEnvelope asserts a failed TRIGGER_BUILD result
// publishes one run.trigger_failed envelope, and acknowledges the pending
// imperative message after the publish.
func TestFailedTriggerPublishesEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    false,
		Error:      "trigger build team-a/build: HTTP 404: no such job",
		HttpStatus: 404,
	})

	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunTriggerFailed {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunTriggerFailed)
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
	if got := env.Payload["reason_code"]; got != reasonJobNotFound {
		t.Fatalf("reason_code = %v, want %s", got, reasonJobNotFound)
	}
	if got := env.Payload["http_status"]; got != float64(404) {
		t.Fatalf("http_status = %v, want 404", got)
	}
	if got := env.Payload["actor"]; got != "user:alice" {
		t.Fatalf("actor = %v", got)
	}
	if got, ok := env.Payload["error"].(string); !ok || got == "" {
		t.Fatalf("error = %v", env.Payload["error"])
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestReasonCodeForStatus asserts the complete status-to-reason mapping.
func TestReasonCodeForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{404, reasonJobNotFound},
		{409, reasonJobNotBuildable},
		{400, reasonTriggerRejected},
		{403, reasonTriggerRejected},
		{500, reasonTriggerRejected},
		{0, reasonTriggerRejected},
	}
	for _, tc := range cases {
		if got := reasonCodeForStatus(tc.status); got != tc.want {
			t.Fatalf("reasonCodeForStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestSuccessfulTriggerPublishesNothing asserts a successful result writes no
// record.
func TestSuccessfulTriggerPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId: "cmd-1",
		Success:   true,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestFailedCancelPublishesNothing asserts the gateway keys on the command
// type, so a failed CANCEL_BUILD writes no record.
func TestFailedCancelPublishesNothing(t *testing.T) {
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
		Success:    false,
		Error:      "cancel build failed",
		HttpStatus: 404,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestUnresolvedTenantPublishesNothing asserts a cell that hosts no tenant for
// the job path writes no record, and acknowledges the pending message.
func TestUnresolvedTenantPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, stubJobTenantResolver{})
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    false,
		Error:      "trigger build team-a/build: HTTP 404: no such job",
		HttpStatus: 404,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestFailedPublishLeavesMessageUnacknowledged asserts the gateway acks the
// imperative only after a successful publish, so the stream redelivers it.
func TestFailedPublishLeavesMessageUnacknowledged(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("stream not found")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId:  "cmd-1",
		Success:    false,
		Error:      "trigger build team-a/build: HTTP 404: no such job",
		HttpStatus: 404,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	requireStaysUnacknowledged(t, conn, stream, consumer)
}

// TestFailedTriggerRedeliveryPublishesAfterATransientFailure asserts a
// transient publish failure never becomes a silent drop. The first result
// leaves the command unacknowledged, and the redelivered command publishes the
// record and acknowledges. The gateway must consult no time-based cache here:
// a cache armed by the first failure would acknowledge the redelivery and lose
// the record.
func TestFailedTriggerRedeliveryPublishesAfterATransientFailure(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("stream not found")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, msg := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedTriggerResult())
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	requireStaysUnacknowledged(t, conn, stream, consumer)

	// The stream redelivers the unacknowledged command, and the gateway files
	// it again.
	filePendingImperative(h, "cmd-1", msg)
	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedTriggerResult())

	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes after the redelivery, want 1", got)
	}
	if env := pub.envelopeAt(0); env.Kind != kindRunTriggerFailed {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunTriggerFailed)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestFailedTriggerWithNoRecordStorePublishesNothing asserts an install with no
// record store acknowledges the command and publishes nothing. Such an install
// has nothing to record.
func TestFailedTriggerWithNoRecordStorePublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	h.SetRecordStoreEnabled(false)
	stream, consumer, _ := seedPendingImperative(t, h, conn, triggerBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedTriggerResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}
