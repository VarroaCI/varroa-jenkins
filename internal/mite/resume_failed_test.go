package mite

import (
	"errors"
	"fmt"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// resumeFailedInstance is the target instance every resume command in this
// file names, and the value the recorder's own write is guarded by.
const resumeFailedInstance = "pod-live"

// resumeBuildCommand returns one RESUME_BUILD imperative for the job the
// trigger tests already resolve, so both suites share one resolver.
func resumeBuildCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "cmd-1",
		Type:      mitev1.CommandTypeResumeBuild,
		ResumeBuild: &mitev1.ResumeBuildPayload{
			RunId:       "0199d0b7-0000-7000-8000-000000000011",
			JobPath:     triggerFailedJob,
			BuildNumber: 7,
			Files:       []string{"jobs/team-a/jobs/build/builds/7/build.xml"},
			Instance:    resumeFailedInstance,
		},
	}
}

// failedResumeResult is the result a refused RESUME_BUILD command reports.
// The error names no token, so it maps onto the default reason code.
func failedResumeResult() *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId: "cmd-1",
		Success:   false,
		Error:     "resume_build: fetch jobs/team-a/jobs/build/builds/7/build.xml: stream closed",
	}
}

// TestRecordCommandOutcomeResumeFailedPublishes asserts a failed
// RESUME_BUILD result publishes one run.resume_failed envelope naming the
// run, the job, the target instance, and one reason code.
func TestRecordCommandOutcomeResumeFailedPublishes(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, resumeBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedResumeResult())

	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunResumeFailed {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunResumeFailed)
	}
	if env.RunID != "0199d0b7-0000-7000-8000-000000000011" {
		t.Fatalf("run_id = %q", env.RunID)
	}
	if env.JobPath != triggerFailedJob {
		t.Fatalf("job_path = %q", env.JobPath)
	}
	if env.Tenant != "team-a" {
		t.Fatalf("tenant = %q, want team-a", env.Tenant)
	}
	if env.IntentKey != "" {
		t.Fatalf("intent_key = %q, want empty: a resume command carries none", env.IntentKey)
	}
	if env.Cluster != h.cluster || env.Namespace != triggerFailedNS || env.Cell != triggerFailedCell {
		t.Fatalf("identity = %q/%q/%q", env.Cluster, env.Namespace, env.Cell)
	}
	if env.OccurredAt.IsZero() {
		t.Fatal("occurred_at is zero")
	}
	if got := env.Payload["instance"]; got != resumeFailedInstance {
		t.Fatalf("instance = %v, want %s", got, resumeFailedInstance)
	}
	if got := env.Payload["reason_code"]; got != reasonResumeFailed {
		t.Fatalf("reason_code = %v, want %s", got, reasonResumeFailed)
	}
	if got, ok := env.Payload["error"].(string); !ok || got == "" {
		t.Fatalf("error = %v", env.Payload["error"])
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcomeResumeSuccessPublishesNothing asserts a resume
// the target applied writes no record. Such a build proves itself by its own
// heartbeat, which the recorder already reads.
func TestRecordCommandOutcomeResumeSuccessPublishesNothing(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, resumeBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, &mitev1.CommandResult{
		CommandId: "cmd-1",
		Success:   true,
	})

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcomeResumeAlwaysAcks asserts a failed publish still
// acknowledges the command. A redelivered resume can succeed while the
// recorder holds no dispatched resume, and only the named target's own
// heartbeat clears a resume record. The run keeps its resume deadline
// instead, which is the path it takes with no record at all.
func TestRecordCommandOutcomeResumeAlwaysAcks(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("no responders")}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	stream, consumer, _ := seedPendingImperative(t, h, conn, resumeBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedResumeResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0: the publish failed", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcomeResumeNoRecordStore asserts an install with no
// record store publishes nothing and acknowledges.
func TestRecordCommandOutcomeResumeNoRecordStore(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	h.SetRecordStoreEnabled(false)
	stream, consumer, _ := seedPendingImperative(t, h, conn, resumeBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedResumeResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcomeResumeUnresolvedTenant asserts a cell writes no
// record under a tenant it does not host.
func TestRecordCommandOutcomeResumeUnresolvedTenant(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, stubJobTenantResolver{})
	stream, consumer, _ := seedPendingImperative(t, h, conn, resumeBuildCommand())

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedResumeResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestRecordCommandOutcomeResumeNamesNoInstance asserts a command that names
// no instance publishes nothing. The recorder's write is guarded by that
// instance, so it could apply no such record.
func TestRecordCommandOutcomeResumeNamesNoInstance(t *testing.T) {
	pub := &fakePublisher{}
	h, conn := newTriggerFailedHandler(t, pub, triggerFailedResolver())
	cmd := resumeBuildCommand()
	cmd.ResumeBuild.Instance = ""
	stream, consumer, _ := seedPendingImperative(t, h, conn, cmd)

	h.OnCommandResult(triggerFailedCell, triggerFailedNS, failedResumeResult())

	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want 0", got)
	}
	waitAckAll(t, conn, stream, consumer)
}

// TestReasonCodeForResumeError asserts the six codes, the order that
// makes the answer fail-safe, and the bounded token test that keeps a
// job path from reading as a failure reason.
func TestReasonCodeForResumeError(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a load failure",
			text: fmt.Sprintf("%s: load build 7 of t/j: HTTP 500", reasonResumeLoadFailed),
			want: reasonResumeLoadFailed,
		},
		{
			name: "a build that does not run",
			text: fmt.Sprintf("%s: build 7 of t/j does not run after the reload", reasonResumeNotBuilding),
			want: reasonResumeNotBuilding,
		},
		{
			name: "an expired command",
			text: fmt.Sprintf("%s: the resume deadline passed at 1700000000", reasonResumeExpired),
			want: reasonResumeExpired,
		},
		{
			name: "an instance mismatch",
			text: fmt.Sprintf("%s: the command names instance pod-a and this mite is pod-b", miteInstanceMismatchToken),
			want: reasonResumeInstanceMismatch,
		},
		{
			name: "a probe that cannot answer",
			text: fmt.Sprintf("%s: get build 7 of t/j: connection refused", reasonResumeProbeFailed),
			want: reasonResumeProbeFailed,
		},
		{
			name: "a fetch failure names no token",
			text: "resume_build: fetch jobs/t/jobs/j/builds/7/program.dat: stream closed",
			want: reasonResumeFailed,
		},
		{
			name: "a reload failure names no token",
			text: "resume_build: reload t/j: HTTP 503",
			want: reasonResumeFailed,
		},
		{
			// The deadline guard wraps the inner error, so the text holds
			// both tokens. The loaded build must win, or a second target
			// would adopt a build this one already holds.
			name: "a deadline that wraps a load failure answers the load failure",
			text: fmt.Sprintf("%s: the resume deadline passed at 1700000000: %s: HTTP 500",
				reasonResumeExpired, reasonResumeLoadFailed),
			want: reasonResumeLoadFailed,
		},
		{
			name: "a deadline that wraps a fetch failure answers the expiry",
			text: fmt.Sprintf("%s: the resume deadline passed at 1700000000: resume_build: fetch: closed",
				reasonResumeExpired),
			want: reasonResumeExpired,
		},
		{
			// A folder may legally carry a token as part of its name. A
			// plain substring test would read this as a loaded build and
			// strand the run.
			name: "a job path that holds a token as text answers the default",
			text: "resume_build: fetch jobs/t/jobs/RESUME_LOAD_FAILED_backup/builds/7/build.xml: closed",
			want: reasonResumeFailed,
		},
		{
			name: "an empty error answers the default",
			text: "",
			want: reasonResumeFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonCodeForResumeError(tc.text); got != tc.want {
				t.Fatalf("reasonCodeForResumeError(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
