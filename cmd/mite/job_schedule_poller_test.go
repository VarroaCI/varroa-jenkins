package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func newJobScheduleTestAgent(t *testing.T, url string) *Agent {
	t.Helper()
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     url,
	})
	agent.Logger = slog.Default()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	return agent
}

// --- 9.3: hash computation ---

// TestJobScheduleHashStableUnderReorder proves the hash is unchanged when
// the same rows arrive in a different order.
func TestJobScheduleHashStableUnderReorder(t *testing.T) {
	a := jobScheduleRow{JobPath: "team-a/build", TriggerType: "cron", Spec: "H/15 * * * *"}
	b := jobScheduleRow{JobPath: "team-a/deploy", TriggerType: "none"}
	c := jobScheduleRow{JobPath: "team-b/build", TriggerType: "scm-poll", Spec: "H/5 * * * *", Timezone: "UTC"}

	h1 := computeJobScheduleHash([]jobScheduleRow{a, b, c})
	h2 := computeJobScheduleHash([]jobScheduleRow{c, a, b})
	h3 := computeJobScheduleHash([]jobScheduleRow{b, c, a})

	if h1 != h2 || h1 != h3 {
		t.Fatalf("hash not stable under reorder: %q, %q, %q", h1, h2, h3)
	}
}

// TestJobScheduleHashUnchangedWhenOccurrencesMove proves the hash covers
// only the job path, the trigger type, the specification, the time zone,
// and the disabled state — never the occurrences.
func TestJobScheduleHashUnchangedWhenOccurrencesMove(t *testing.T) {
	base := jobScheduleRow{
		JobPath:     "team-a/build",
		TriggerType: "cron",
		Spec:        "H/15 * * * *",
		Timezone:    "America/New_York",
		Disabled:    false,
	}
	early := base
	early.NextFireTimes = []string{"2026-09-10T12:00:00Z", "2026-09-10T12:15:00Z"}
	later := base
	later.NextFireTimes = []string{"2026-09-10T12:15:00Z", "2026-09-10T12:30:00Z"}

	h1 := computeJobScheduleHash([]jobScheduleRow{early})
	h2 := computeJobScheduleHash([]jobScheduleRow{later})
	if h1 != h2 {
		t.Fatalf("hash changed when only occurrences moved: %q != %q", h1, h2)
	}
}

// TestJobScheduleHashChangesOnFieldDiff is a control: the hash does
// change when a covered field differs, so the two tests above are not
// vacuously true.
func TestJobScheduleHashChangesOnFieldDiff(t *testing.T) {
	base := jobScheduleRow{JobPath: "team-a/build", TriggerType: "cron", Spec: "H/15 * * * *"}
	changed := base
	changed.Disabled = true

	if computeJobScheduleHash([]jobScheduleRow{base}) == computeJobScheduleHash([]jobScheduleRow{changed}) {
		t.Fatal("hash did not change when disabled state differed")
	}
	upstream := base
	upstream.UpstreamResult = "FAILURE"
	if computeJobScheduleHash([]jobScheduleRow{base}) == computeJobScheduleHash([]jobScheduleRow{upstream}) {
		t.Fatal("hash did not change when an upstream field differed")
	}
}

// --- 9.4: three-condition send gate ---

func fixedJobScheduleSet(hash string) *mitev1.JobScheduleSet {
	return &mitev1.JobScheduleSet{
		Hash: hash,
		Items: []*mitev1.JobSchedule{
			{JobPath: "team-a/build", TriggerType: "cron", Spec: "H/15 * * * *"},
		},
	}
}

// TestMaybeAttachJobSchedulesUnchangedSetNotCarried proves an unchanged
// set stays off the snapshot on ordinary (non-reconnect, non-timer)
// attach calls, matching the 15-second heartbeat cadence.
func TestMaybeAttachJobSchedulesUnchangedSetNotCarried(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	set := fixedJobScheduleSet("hash-1")
	a.jobScheduleSet = set
	a.jobScheduleLastSentHash = "hash-1"
	a.jobScheduleLastSendTime = time.Now()
	a.jobScheduleResendAfter = time.Hour

	for i := 0; i < 5; i++ {
		snap := &mitev1.StateSnapshot{}
		a.maybeAttachJobSchedules(snap)
		if snap.JobSchedules != nil {
			t.Fatalf("tick %d: expected no job-schedule set carried, got one", i)
		}
	}
}

// TestMaybeAttachJobSchedulesReconnectForcesSend proves a reconnect
// carries the last known set on the next snapshot even though its hash
// is unchanged.
func TestMaybeAttachJobSchedulesReconnectForcesSend(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	set := fixedJobScheduleSet("hash-1")
	a.jobScheduleSet = set
	a.jobScheduleLastSentHash = "hash-1"
	a.jobScheduleLastSendTime = time.Now()
	a.jobScheduleResendAfter = time.Hour

	// Not yet reconnected: unchanged set stays off.
	snap := &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != nil {
		t.Fatal("expected no set carried before reconnect")
	}

	a.markStreamReconnected()

	snap = &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != set {
		t.Fatal("expected the set carried on the first snapshot after reconnect")
	}

	// The gate closes again immediately after: the next tick with an
	// unchanged hash and no further reconnect carries nothing.
	snap = &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != nil {
		t.Fatal("expected the gate to close again after the reconnect send")
	}
}

// TestMaybeAttachJobSchedulesTimerForcesSend proves the resend interval
// carries an unchanged set again once it elapses.
func TestMaybeAttachJobSchedulesTimerForcesSend(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	set := fixedJobScheduleSet("hash-1")
	a.jobScheduleSet = set
	a.jobScheduleLastSentHash = "hash-1"
	a.jobScheduleResendAfter = 10 * time.Millisecond
	a.jobScheduleLastSendTime = time.Now()

	snap := &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != nil {
		t.Fatal("expected no set carried before the resend interval elapses")
	}

	time.Sleep(15 * time.Millisecond)

	snap = &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != set {
		t.Fatal("expected the resend timer to force a send once the interval elapsed")
	}
}

// TestMaybeAttachJobSchedulesChangedHashForcesSend is a control proving
// condition 1 (a changed hash) independently opens the gate.
func TestMaybeAttachJobSchedulesChangedHashForcesSend(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	set := fixedJobScheduleSet("hash-2")
	a.jobScheduleSet = set
	a.jobScheduleLastSentHash = "hash-1"
	a.jobScheduleLastSendTime = time.Now()
	a.jobScheduleResendAfter = time.Hour

	snap := &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != set {
		t.Fatal("expected a changed hash to force a send")
	}
}

// TestMaybeAttachJobSchedulesNeverPolled proves the field stays unset
// when the mite has never completed a successful poll.
func TestMaybeAttachJobSchedulesNeverPolled(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")
	a.markStreamReconnected()

	snap := &mitev1.StateSnapshot{}
	a.maybeAttachJobSchedules(snap)
	if snap.JobSchedules != nil {
		t.Fatal("expected no set carried before any poll succeeded")
	}
}

// --- 9.2b: failed poll retains the last known set ---

// TestPollJobSchedulesOnceFailureKeepsLastKnownSet proves a failed poll
// leaves the cached set and hash in place and logs the failure once.
func TestPollJobSchedulesOnceFailureKeepsLastKnownSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	a := newJobScheduleTestAgent(t, srv.URL)
	original := fixedJobScheduleSet("hash-1")
	a.jobScheduleSet = original

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	a.pollJobSchedulesOnce(context.Background(), logger)

	if a.jobScheduleSet != original {
		t.Fatal("expected the cached set to survive a failed poll unchanged")
	}
	logged := logBuf.String()
	if strings.Count(logged, "job-schedule poll failed") != 1 {
		t.Fatalf("expected exactly one failure log line, got: %q", logged)
	}
}

// TestPollJobSchedulesOnceSuccessReplacesCachedSet is a control proving a
// successful poll does update the cached set.
func TestPollJobSchedulesOnceSuccessReplacesCachedSet(t *testing.T) {
	resp := jobScheduleResponse{
		Schedules: []jobScheduleRow{
			{JobPath: "team-a/build", TriggerType: "cron", Spec: "H/15 * * * *"},
			{JobPath: "team-a/deploy", TriggerType: "varroa-upstream",
				UpstreamTenant: "payments", UpstreamJob: "release", UpstreamResult: "SUCCESS"},
		},
		Total: 2,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobSchedulesPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	a := newJobScheduleTestAgent(t, srv.URL)
	a.pollJobSchedulesOnce(context.Background(), slog.Default())

	if a.jobScheduleSet == nil {
		t.Fatal("expected a cached set after a successful poll")
	}
	if len(a.jobScheduleSet.Items) != 2 || a.jobScheduleSet.Items[0].JobPath != "team-a/build" {
		t.Fatalf("unexpected cached set: %+v", a.jobScheduleSet)
	}
	if a.jobScheduleSet.Total != 2 {
		t.Errorf("Total = %d, want 2", a.jobScheduleSet.Total)
	}
	up := a.jobScheduleSet.Items[1]
	if up.UpstreamTenant != "payments" || up.UpstreamJob != "release" || up.UpstreamResult != "SUCCESS" {
		t.Errorf("upstream fields = %+v, want payments/release/SUCCESS", up)
	}
}

// --- 9.2: poll cadence ---

// TestStartJobSchedulePollerCadence proves the poller polls once
// immediately, then again on every tick of the configured interval.
func TestStartJobSchedulePollerCadence(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobScheduleResponse{})
	}))
	t.Cleanup(srv.Close)

	a := newJobScheduleTestAgent(t, srv.URL)
	a.jobScheduleInterval = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 95*time.Millisecond)
	defer cancel()
	a.startJobSchedulePoller(ctx)

	got := atomic.LoadInt32(&count)
	// One immediate poll plus at least three ticks in the 95ms window at
	// a 20ms cadence.
	if got < 3 {
		t.Fatalf("expected at least 3 polls in the window, got %d", got)
	}
}

// --- 9.5: the set stays out of every state hash ---

// TestJobSchedulesExcludedFromStateHash proves attaching a job-schedule
// set never changes actual_state_hash.
func TestJobSchedulesExcludedFromStateHash(t *testing.T) {
	a := newJobScheduleTestAgent(t, "http://localhost:1")

	withoutSet := &mitev1.StateSnapshot{
		ConfigHash:  "cfg",
		PluginsHash: "plg",
		RbacHash:    "rbac",
		ItemsHash:   "items",
	}
	withoutSet.ActualStateHash = computeStateHash(withoutSet.ConfigHash, withoutSet.PluginsHash, withoutSet.RbacHash, withoutSet.ItemsHash)

	a.jobScheduleSet = fixedJobScheduleSet("hash-1")
	a.markStreamReconnected()

	withSet := &mitev1.StateSnapshot{
		ConfigHash:  "cfg",
		PluginsHash: "plg",
		RbacHash:    "rbac",
		ItemsHash:   "items",
	}
	withSet.ActualStateHash = computeStateHash(withSet.ConfigHash, withSet.PluginsHash, withSet.RbacHash, withSet.ItemsHash)
	a.maybeAttachJobSchedules(withSet)

	if withSet.JobSchedules == nil {
		t.Fatal("expected the set to be attached in this scenario")
	}
	if withSet.ActualStateHash != withoutSet.ActualStateHash {
		t.Fatalf("ActualStateHash changed when the job-schedule set was attached: %q != %q",
			withSet.ActualStateHash, withoutSet.ActualStateHash)
	}
}
