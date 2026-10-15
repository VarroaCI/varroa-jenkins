package mite

import (
	"errors"
	"strings"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const commandRefusedRun = "0199d0b7-0000-7000-8000-000000000020"

func cancelBuildCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "cmd-1",
		Type:      mitev1.CommandTypeCancelBuild,
		CancelBuild: &mitev1.CancelBuildPayload{
			RunId: commandRefusedRun, JobPath: triggerFailedJob, BuildNumber: 4, Actor: "alice",
		},
	}
}

func refusedRunCommandResult(cmdType mitev1.CommandType) *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId:   "cmd-1",
		Success:     false,
		Error:       "cancel build team-a/build: HTTP 404: " + strings.Repeat("x", 900),
		HttpStatus:  404,
		CommandType: cmdType,
		RunId:       commandRefusedRun,
		JobPath:     triggerFailedJob,
		Actor:       "alice",
	}
}

// requireCommandRefused asserts one run.command_refused envelope for the
// test run, naming command and actor, with the error capped at the gateway
// limit.
func requireCommandRefused(t *testing.T, pub *fakePublisher, command, actor string) {
	t.Helper()
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunCommandRefused {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunCommandRefused)
	}
	if env.RunID != commandRefusedRun || env.JobPath != triggerFailedJob || env.Tenant != "team-a" {
		t.Fatalf("identity = run %q job %q tenant %q", env.RunID, env.JobPath, env.Tenant)
	}
	if env.Namespace != triggerFailedNS || env.Cell != triggerFailedCell {
		t.Fatalf("namespace/cell = %q/%q", env.Namespace, env.Cell)
	}
	if env.OccurredAt.IsZero() {
		t.Fatal("occurred_at is zero")
	}
	if got := env.Payload["command"]; got != command {
		t.Fatalf("command = %v, want %s", got, command)
	}
	if got := env.Payload["actor"]; got != actor {
		t.Fatalf("actor = %v, want %s", got, actor)
	}
	if got, _ := env.Payload["http_status"].(float64); got != 404 {
		t.Fatalf("http_status = %v, want 404", env.Payload["http_status"])
	}
	errText, _ := env.Payload["error"].(string)
	if n := len([]rune(errText)); n == 0 || n > errorTruncateChars {
		t.Fatalf("error has %d runes, want 1..%d", n, errorTruncateChars)
	}
}

// TestRecordCommandOutcome_RefusedCancelPublishesCommandRefused asserts a
// refused cancel publishes one run.command_refused envelope and acknowledges.
func TestRecordCommandOutcome_RefusedCancelPublishesCommandRefused(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, cancelBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeCancelBuild))

	requireCommandRefused(t, pub, refusedCommandCancel, "alice")
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcome_RefusedInputAnswerPublishesCommandRefused asserts
// a refused proceed and a refused abort each publish one run.command_refused
// envelope that names the acting user.
func TestRecordCommandOutcome_RefusedInputAnswerPublishesCommandRefused(t *testing.T) {
	cases := []struct {
		command string
		cmd     *mitev1.ImperativeCommand
	}{
		{refusedCommandProceed, &mitev1.ImperativeCommand{CommandId: "cmd-1", Type: mitev1.CommandTypeProceedInput,
			ProceedInput: &mitev1.ProceedInputPayload{RunId: commandRefusedRun, JobPath: triggerFailedJob, InputId: "deploy",
				ActingUser: &mitev1.ActingUser{Name: "bob"}}}},
		{refusedCommandAbort, &mitev1.ImperativeCommand{CommandId: "cmd-1", Type: mitev1.CommandTypeAbortInput,
			AbortInput: &mitev1.AbortInputPayload{RunId: commandRefusedRun, JobPath: triggerFailedJob, InputId: "deploy",
				ActingUser: &mitev1.ActingUser{Name: "bob"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			pub := &fakePublisher{}
			h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
			stream, consumer, _ := seedPendingImperative(t, h, conn, tc.cmd)

			h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(tc.cmd.Type))

			requireCommandRefused(t, pub, tc.command, "bob")
			waitAckAll(t, conn, stream, consumer)
		})
	}
}

// TestRecordCommandOutcome_RefusedReplayPublishesTriggerFailedForTheNewRun
// asserts a refused replay publishes one run.trigger_failed envelope for
// the new run the replay named, so that run's page shows the refusal.
func TestRecordCommandOutcome_RefusedReplayPublishesTriggerFailedForTheNewRun(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	cmd := &mitev1.ImperativeCommand{
		CommandId: "cmd-1",
		Type:      mitev1.CommandTypeReplayBuild,
		ReplayBuild: &mitev1.ReplayBuildPayload{
			RunId:       commandRefusedRun,
			IntentKey:   "replay:source:" + commandRefusedRun,
			JobPath:     triggerFailedJob,
			SourceRunId: "0199d0b7-0000-7000-8000-000000000001",
			Script:      "echo hi",
			ActingUser:  &mitev1.ActingUser{Name: "carol"},
		},
	}
	stream, consumer, _ := seedPendingImperative(t, h, conn, cmd)

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeReplayBuild))

	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunTriggerFailed {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunTriggerFailed)
	}
	if env.RunID != commandRefusedRun || env.IntentKey != "replay:source:"+commandRefusedRun {
		t.Fatalf("run %q intent %q", env.RunID, env.IntentKey)
	}
	if got := env.Payload["actor"]; got != "carol" {
		t.Fatalf("actor = %v, want carol", got)
	}
	if got := env.Payload["reason_code"]; got != reasonJobNotFound {
		t.Fatalf("reason_code = %v, want %s", got, reasonJobNotFound)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcome_SuccessfulCancelPublishesNothing asserts a cancel
// the plugin applied writes no record.
func TestRecordCommandOutcome_SuccessfulCancelPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, cancelBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId: "cmd-1", Success: true, HttpStatus: 202,
		CommandType: mitev1.CommandTypeCancelBuild, RunId: commandRefusedRun,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcome_RefusedCancelWithoutRecordStorePublishesNothing
// asserts an install with no record store publishes nothing and
// acknowledges.
func TestRecordCommandOutcome_RefusedCancelWithoutRecordStorePublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	h.SetRecordStoreEnabled(false)
	stream, consumer, _ := seedPendingImperative(t, h, conn, cancelBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeCancelBuild))

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcome_RefusedCancelUnresolvedTenantPublishesNothing
// asserts a job path no tenant owns publishes nothing: a cell must not write
// a record under a tenant it does not host.
func TestRecordCommandOutcome_RefusedCancelUnresolvedTenantPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})
	stream, consumer, _ := seedPendingImperative(t, h, conn, cancelBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeCancelBuild))

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcome_RefusedCancelPublishFailureStillAcks asserts a
// failed publish still acknowledges: a redelivered cancel runs the refused
// call again, and the record is only a notice.
func TestRecordCommandOutcome_RefusedCancelPublishFailureStillAcks(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("no responders")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, cancelBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeCancelBuild))

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestFailedRestartRefusalPublishLeavesMessageUnacknowledged asserts a refused
// restart whose run.trigger_failed publish fails keeps the command pending, so
// the stream redelivers it and the refusal record is not lost.
func TestFailedRestartRefusalPublishLeavesMessageUnacknowledged(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("stream not found")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	cmd := &mitev1.ImperativeCommand{
		CommandId: "cmd-1",
		Type:      mitev1.CommandTypeRestartBuild,
		RestartBuild: &mitev1.RestartBuildPayload{
			RunId:             commandRefusedRun,
			IntentKey:         "restart:source:" + commandRefusedRun,
			JobPath:           triggerFailedJob,
			SourceRunId:       "0199d0b7-0000-7000-8000-000000000001",
			SourceBuildNumber: 7,
			Stage:             "deploy",
			ActingUser:        &mitev1.ActingUser{Name: "carol"},
		},
	}
	stream, consumer, _ := seedPendingImperative(t, h, conn, cmd)

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, refusedRunCommandResult(mitev1.CommandTypeRestartBuild))

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0 after a failed publish", got)
	}
	requireStaysUnacknowledged(t, conn, stream, consumer)
}
