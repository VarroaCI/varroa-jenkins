package recordstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// notifyClockStart is the fixed instant every notifier test starts its
// injected clock at, so one case reads the same times as another.
var notifyClockStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newCaptureNotifier returns a notifier with no connection, a replaced
// send that records every payload, a captured log, and a clock a test
// moves by hand.
func newCaptureNotifier(t *testing.T) (*RunNotifier, *captureSend, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	n := NewRunNotifier(nil, slog.New(slog.NewTextHandler(&logs, nil)))
	n.now = func() time.Time { return notifyClockStart }
	sent := &captureSend{}
	n.send = sent.publish
	return n, sent, &logs
}

// captureSend records the subject and the payload of every publish. It
// guards its own state, because a timer goroutine can publish while the
// test reads.
type captureSend struct {
	mu       sync.Mutex
	subjects []string
	payloads []RunNotification
}

// publish records one send, and fails a decode the test would otherwise
// read as a missing payload.
func (c *captureSend) publish(subject string, data []byte) error {
	var got RunNotification
	if err := json.Unmarshal(data, &got); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subjects = append(c.subjects, subject)
	c.payloads = append(c.payloads, got)
	return nil
}

// count returns how many payloads reached send.
func (c *captureSend) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

// payload returns the i'th published notification.
func (c *captureSend) payload(i int) RunNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.payloads[i]
}

// subject returns the i'th published subject.
func (c *captureSend) subject(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subjects[i]
}

// entryCount returns how many runs the notifier still holds.
func entryCount(n *RunNotifier) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.state)
}

// notifyOne builds a non-terminal notification for one run.
func notifyOne(runID, kind, status string) RunNotification {
	return RunNotification{
		Kind:    kind,
		Cluster: testCluster,
		Tenant:  testTenant,
		JobPath: testJobPath,
		RunID:   runID,
		Status:  status,
	}
}

// TestRunNotifierCoalescesInsideInterval asserts two notifications that
// arrive inside the interval publish once, and the newest state wins.
func TestRunNotifierCoalescesInsideInterval(t *testing.T) {
	n, sent, _ := newCaptureNotifier(t)
	defer n.Close()

	n.Notify(notifyOne("run-1", "run", "RUNNING"))
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the first write, want 1", got)
	}

	n.now = func() time.Time { return notifyClockStart.Add(100 * time.Millisecond) }
	n.Notify(notifyOne("run-1", "stage", ""))
	n.Notify(notifyOne("run-1", "run", "QUEUED"))
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications inside the interval, want 1", got)
	}

	n.now = func() time.Time { return notifyClockStart.Add(500 * time.Millisecond) }
	n.flush("run-1")

	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications in total, want 2", got)
	}
	held := sent.payload(1)
	if held.Status != "QUEUED" {
		t.Fatalf("published status = %q, want QUEUED (the newest state)", held.Status)
	}
	if held.Seq != 2 {
		t.Fatalf("published seq = %d, want 2", held.Seq)
	}
	if want := notifyClockStart.Add(500 * time.Millisecond); !held.At.Equal(want) {
		t.Fatalf("published at = %s, want %s", held.At, want)
	}
	if want := bus.RunNotifySubject(testCluster, testTenant); sent.subject(1) != want {
		t.Fatalf("published subject = %q, want %q", sent.subject(1), want)
	}

	n.flush("run-1")
	if got := sent.count(); got != 2 {
		t.Fatalf("a second flush published %d notifications in total, want 2", got)
	}
}

// TestRunNotifierTerminalPublishesAtOnce asserts a terminal notification
// publishes at once, and drops the state held for its run.
func TestRunNotifierTerminalPublishesAtOnce(t *testing.T) {
	n, sent, _ := newCaptureNotifier(t)
	defer n.Close()

	n.Notify(notifyOne("run-1", "run", "RUNNING"))
	n.now = func() time.Time { return notifyClockStart.Add(100 * time.Millisecond) }
	n.Notify(notifyOne("run-1", "stage", ""))
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications before the terminal write, want 1", got)
	}

	n.Notify(RunNotification{
		Kind:    "run",
		Cluster: testCluster,
		Tenant:  testTenant,
		JobPath: testJobPath,
		RunID:   "run-1",
		Status:  "COMPLETED",
		Result:  "SUCCESS",
	})

	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications after the terminal write, want 2", got)
	}
	if got := sent.payload(1); got.Status != "COMPLETED" || got.Result != "SUCCESS" {
		t.Fatalf("published %+v, want a COMPLETED/SUCCESS notification", got)
	}
	if got := entryCount(n); got != 0 {
		t.Fatalf("held %d entries after the terminal write, want 0", got)
	}

	n.flush("run-1")
	if got := sent.count(); got != 2 {
		t.Fatalf("flush after the terminal write published %d notifications in total, want 2", got)
	}
}

// TestRunNotifierEntryLife asserts the sweep deletes an idle entry, and
// Close publishes a held notification.
func TestRunNotifierEntryLife(t *testing.T) {
	t.Run("sweep deletes an idle entry", func(t *testing.T) {
		n, sent, _ := newCaptureNotifier(t)
		defer n.Close()

		n.Notify(notifyOne("run-1", "run", "RUNNING"))
		if got := entryCount(n); got != 1 {
			t.Fatalf("held %d entries after one write, want 1", got)
		}

		n.sweep(notifyClockStart.Add(runNotifyIdle + time.Second))
		if got := entryCount(n); got != 0 {
			t.Fatalf("held %d entries after an idle sweep, want 0", got)
		}

		// The forgotten entry leaves no stale send time behind: the next
		// write publishes at once.
		n.Notify(notifyOne("run-1", "run", "QUEUED"))
		if got := sent.count(); got != 2 {
			t.Fatalf("published %d notifications in total, want 2", got)
		}
	})

	t.Run("close publishes a held notification", func(t *testing.T) {
		n, sent, _ := newCaptureNotifier(t)

		n.Notify(notifyOne("run-1", "run", "RUNNING"))
		n.now = func() time.Time { return notifyClockStart.Add(100 * time.Millisecond) }
		n.Notify(notifyOne("run-1", "stage", ""))
		if got := sent.count(); got != 1 {
			t.Fatalf("published %d notifications before Close, want 1", got)
		}

		n.Close()
		if got := sent.count(); got != 2 {
			t.Fatalf("published %d notifications after Close, want 2", got)
		}
		if got := sent.payload(1); got.Kind != "stage" {
			t.Fatalf("published kind = %q, want stage", got.Kind)
		}
		if got := entryCount(n); got != 0 {
			t.Fatalf("held %d entries after Close, want 0", got)
		}

		n.flush("run-1")
		if got := sent.count(); got != 2 {
			t.Fatalf("flush after Close published %d notifications in total, want 2", got)
		}
	})
}

// TestNotificationForMapsEveryKind asserts the notification builder maps
// every ingest kind, and answers false for the three kinds that publish
// nothing.
func TestNotificationForMapsEveryKind(t *testing.T) {
	buildNumber := int64(42)
	success := "SUCCESS"
	cases := []struct {
		kind       string
		payload    any
		wantKind   string
		wantStatus string
		wantResult string
		wantBuild  int64
		wantOK     bool
	}{
		{KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber}, "run", "RUNNING", "", 42, true},
		{KindRunCompleted, RunCompletedPayload{Result: &success}, "run", "COMPLETED", "SUCCESS", 0, true},
		{KindRunStage, RunStagePayload{StageID: "node-1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS"}, "stage", "", "", 0, true},
		{KindRunHeartbeat, map[string]any{"build_number": 7}, "run", "", "", 0, true},
		{KindRunLog, map[string]any{"build_number": 7}, "run", "", "", 0, true},
		{KindRunChanges, map[string]any{"build_number": 7}, "run", "", "", 0, true},
		{KindRunTests, map[string]any{"build_number": 7}, "run", "", "", 0, true},
		{KindRunState, map[string]any{"build_number": 7}, "", "", "", 0, false},
		{KindRunResumeFailed, map[string]any{"build_number": 7}, "run", "", "", 0, true},
		// A zero ApplyResult is a skipped refusal notice, which publishes
		// nothing. The stored case follows the table.
		{KindRunCommandRefused, RunCommandRefusedPayload{Command: RefusedCommandCancel}, "", "", "", 0, false},
		{KindRunSource, RunSourcePayload{Seq: 0}, "run", "", "", 0, true},
		{KindRunInput, RunInputPayload{Seq: inputSeqPtr(3), Inputs: runInputSet("input-1")}, "input", "", "", 0, true},
		{KindRunTriggerAccepted, map[string]any{}, "run", "QUEUED", "", 0, true},
		{KindRunTriggerFailed, map[string]any{}, "run", "COMPLETED", "TRIGGER_FAILED", 0, true},
		{KindRunArtifacts, map[string]any{}, "", "", "", 0, false},
		{KindJobSchedules, map[string]any{}, "", "", "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			runID := uuid.MustParse(newTestUUIDv7String())
			env := newTestEnvelope(t, runID, "manual:notify-"+tc.kind, tc.kind, tc.payload)

			got, ok := notificationFor(env, ApplyResult{})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if got != (RunNotification{}) {
					t.Fatalf("notification = %+v, want the zero value", got)
				}
				return
			}
			if got.Kind != tc.wantKind || got.Status != tc.wantStatus || got.Result != tc.wantResult {
				t.Fatalf("got %s/%q/%q, want %s/%q/%q",
					got.Kind, got.Status, got.Result, tc.wantKind, tc.wantStatus, tc.wantResult)
			}
			if got.BuildNumber != tc.wantBuild {
				t.Fatalf("build number = %d, want %d", got.BuildNumber, tc.wantBuild)
			}
			if got.Cluster != testCluster || got.Tenant != testTenant || got.JobPath != testJobPath {
				t.Fatalf("coordinates = %s/%s/%s, want %s/%s/%s",
					got.Cluster, got.Tenant, got.JobPath, testCluster, testTenant, testJobPath)
			}
			if got.RunID != env.RunID {
				t.Fatalf("run id = %q, want %q", got.RunID, env.RunID)
			}
		})
	}

	// A superseded start wrote no row, so it publishes nothing.
	superseded := newTestEnvelope(t, uuid.MustParse(newTestUUIDv7String()),
		"manual:notify-superseded", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, ok := notificationFor(superseded, ApplyResult{SupersededStart: true}); ok {
		t.Fatal("notificationFor reported a superseded run.started, want no notification")
	}

	// A refused run.input write changed no stored set, so it publishes
	// nothing.
	refused := newTestEnvelope(t, uuid.MustParse(newTestUUIDv7String()),
		"manual:notify-input-refused", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-1")})
	if _, ok := notificationFor(refused, ApplyResult{RefusedInput: true}); ok {
		t.Fatal("notificationFor reported a refused run.input, want no notification")
	}

	// A stored refusal notice changes the run detail, so an open build
	// page refetches it.
	notice := newTestEnvelope(t, uuid.MustParse(newTestUUIDv7String()),
		"manual:notify-command-refused", KindRunCommandRefused,
		RunCommandRefusedPayload{Command: RefusedCommandCancel})
	got, ok := notificationFor(notice, ApplyResult{CommandRefusedStored: true})
	if !ok || got.Kind != "run" || got.Status != "" || got.RunID != notice.RunID {
		t.Fatalf("stored run.command_refused = %+v, %v; want one run notification", got, ok)
	}
}

// TestRunNotifierDropsUnusableSubjectToken asserts a notification whose
// cluster or tenant holds a character NATS refuses publishes nothing and
// logs the drop.
func TestRunNotifierDropsUnusableSubjectToken(t *testing.T) {
	n, sent, logs := newCaptureNotifier(t)
	defer n.Close()

	bad := []struct {
		runID, cluster, tenant string
	}{
		{"run-1", testCluster, testTenant + ">"},
		{"run-2", testCluster, testTenant + " tenant"},
		{"run-3", testCluster, "*"},
		{"run-4", testCluster + ">", testTenant},
		{"run-5", "core tenant", testTenant},
	}
	for _, tc := range bad {
		notify := notifyOne(tc.runID, "run", "RUNNING")
		notify.Cluster = tc.cluster
		notify.Tenant = tc.tenant
		n.Notify(notify)
	}
	if got := sent.count(); got != 0 {
		t.Fatalf("published %d notifications with an unusable subject token, want 0", got)
	}
	if got := strings.Count(logs.String(), "dropping run notification with an unusable subject token"); got != len(bad) {
		t.Fatalf("logged %d drops, want %d\n%s", got, len(bad), logs.String())
	}

	// A held notification meets the same drop when its run's interval ends.
	held := notifyOne("run-1", "run", "QUEUED")
	held.Tenant = testTenant + ">"
	n.Notify(held)
	n.flush("run-1")
	if got := sent.count(); got != 0 {
		t.Fatalf("flushing a held notification published %d payloads, want 0", got)
	}
	if got := strings.Count(logs.String(), "dropping run notification with an unusable subject token"); got != len(bad)+1 {
		t.Fatalf("logged %d drops after the flush, want %d\n%s", got, len(bad)+1, logs.String())
	}
}

// TestRunNotifierDropsEmptySubjectToken asserts a notification whose
// tenant is empty publishes nothing and logs one drop.
func TestRunNotifierDropsEmptySubjectToken(t *testing.T) {
	n, sent, logs := newCaptureNotifier(t)
	defer n.Close()

	notify := notifyOne("run-1", "run", "RUNNING")
	notify.Tenant = ""
	n.Notify(notify)

	if got := sent.count(); got != 0 {
		t.Fatalf("published %d notifications with an empty tenant, want 0", got)
	}
	if got := strings.Count(logs.String(), "dropping run notification with an unusable subject token"); got != 1 {
		t.Fatalf("logged %d drops, want 1\n%s", got, logs.String())
	}
}

// TestRunNotifierLogsFailedSendAndKeepsWorking asserts a failed publish
// logs one line and leaves the notifier able to publish the next
// notification.
func TestRunNotifierLogsFailedSendAndKeepsWorking(t *testing.T) {
	var logs bytes.Buffer
	n := NewRunNotifier(nil, slog.New(slog.NewTextHandler(&logs, nil)))
	n.now = func() time.Time { return notifyClockStart }
	defer n.Close()

	calls := 0
	n.send = func(string, []byte) error {
		calls++
		if calls == 1 {
			return errors.New("bus down")
		}
		return nil
	}

	n.Notify(notifyOne("run-1", "run", "RUNNING"))
	if got := strings.Count(logs.String(), "publish run notification failed"); got != 1 {
		t.Fatalf("logged %d publish failures, want 1\n%s", got, logs.String())
	}

	n.Notify(notifyOne("run-2", "run", "RUNNING"))
	if calls != 2 {
		t.Fatalf("send calls = %d, want 2 (the notifier must keep working)", calls)
	}
	if got := strings.Count(logs.String(), "publish run notification failed"); got != 1 {
		t.Fatalf("logged %d publish failures after the second write, want 1\n%s", got, logs.String())
	}
}

// TestLockNotificationJSONShape asserts the lock fields reach the wire
// for a lock notification alone, and that a lock notification still
// carries an empty status and result.
func TestLockNotificationJSONShape(t *testing.T) {
	t.Run("a lock notification carries lock_name and state", func(t *testing.T) {
		lockName := testTenant + "/smoke-lock"
		n := lockNotification(testCluster, testTenant, testJobPath, lockName, "run-1", FleetLockStateHeld)
		body := marshalNotification(t, n)

		if got := body["kind"]; got != "lock" {
			t.Errorf("kind = %v, want lock", got)
		}
		if got := body["lock_name"]; got != lockName {
			t.Errorf("lock_name = %v, want %s", got, lockName)
		}
		if got := body["state"]; got != FleetLockStateHeld {
			t.Errorf("state = %v, want %s", got, FleetLockStateHeld)
		}
		if got := body["cluster"]; got != testCluster {
			t.Errorf("cluster = %v, want %s", got, testCluster)
		}
		if got := body["tenant"]; got != testTenant {
			t.Errorf("tenant = %v, want %s", got, testTenant)
		}
		if got := body["job_path"]; got != testJobPath {
			t.Errorf("job_path = %v, want %s", got, testJobPath)
		}
		if got := body["run_id"]; got != "run-1" {
			t.Errorf("run_id = %v, want run-1", got)
		}

		// A lock write changes no run state, so both fields are present
		// as the empty string, never omitted.
		for _, key := range []string{"status", "result"} {
			value, present := body[key]
			if !present {
				t.Errorf("%s is absent, want the empty string", key)
				continue
			}
			if value != "" {
				t.Errorf("%s = %v, want the empty string", key, value)
			}
		}
		if value, present := body["build_number"]; present {
			t.Errorf("build_number = %v, want the field absent", value)
		}
	})

	t.Run("a run or stage notification carries neither key", func(t *testing.T) {
		for _, kind := range []string{"run", "stage"} {
			body := marshalNotification(t, notifyOne("run-1", kind, "RUNNING"))
			for _, key := range []string{"lock_name", "state"} {
				if value, present := body[key]; present {
					t.Errorf("%s notification carries %s = %v, want the field absent", kind, key, value)
				}
			}
		}
	})
}

// TestRunNotifierNilReceiverPublishesNothing asserts a nil notifier
// accepts a notification and publishes nothing, so no caller guards
// against a nil notifier of its own.
func TestRunNotifierNilReceiverPublishesNothing(t *testing.T) {
	var n *RunNotifier
	n.Notify(lockNotification(testCluster, testTenant, testJobPath, testTenant+"/smoke-lock", "run-1", "released"))
}

// marshalNotification decodes one notification's own JSON body, so a
// test reads the keys the wire carries rather than the struct's fields.
func marshalNotification(t *testing.T, n RunNotification) map[string]any {
	t.Helper()
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	return body
}
