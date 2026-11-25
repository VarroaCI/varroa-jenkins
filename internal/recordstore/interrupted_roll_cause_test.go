package recordstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// rollCauseMessage is a cell.rolled message with a raw presence read error.
// No test may find any part of it in a run row.
const rollCauseMessage = "rolled the cell pod: MiteUnreachable for 5m0s; mite jenkins/cell-a command stream lost: " +
	"presence record unreadable: dial tcp 10.0.0.5:4222: connect: connection refused"

// otherPodInstance is the metadata.uid of a cell pod that does not own the
// run under test: an earlier pod or a replacement pod of the same cell.
const otherPodInstance = "pod-other"

// startRunOnCell seeds one granted intent and one RUNNING run of the test
// tenant on cell. Each test gives its run its own cell, so the activity rows
// of one run never match another run.
func startRunOnCell(t *testing.T, pool *pgxpool.Pool, intentKey, cell string) uuid.UUID {
	t.Helper()
	runID := seedGrantedIntentOnCell(t, pool, intentKey, cell)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	started.Cell = cell
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	return runID
}

// cellRolledRow answers the operator's cell.rolled event for the pod
// instance of cell at ts.
func cellRolledRow(cell, instance string, ts time.Time, reason string) activity.Event {
	return activity.Event{
		Timestamp:  ts,
		Type:       "cell.rolled",
		Source:     "operator",
		Cluster:    testCluster,
		Namespace:  testNamespace,
		Controller: cell,
		Message:    rollCauseMessage,
		Reason:     reason,
		Instance:   instance,
	}
}

// podReplacedRow answers the operator's pod.replaced event for the
// boot-failed pod instance of cell at ts.
func podReplacedRow(cell, instance string, ts time.Time) activity.Event {
	return activity.Event{
		Timestamp:  ts,
		Type:       "pod.replaced",
		Source:     "operator",
		Cluster:    testCluster,
		Namespace:  testNamespace,
		Controller: cell,
		Message:    "deleted the boot-failed pod so the StatefulSet can roll the new revision",
		Reason:     "JenkinsBootFailed",
		Instance:   instance,
	}
}

// ingestActivityEvent writes e through the activity ingest, as the
// activity-recorder consumer writes an operator event.
func ingestActivityEvent(t *testing.T, pool *pgxpool.Pool, e activity.Event) {
	t.Helper()
	e.Timestamp = e.Timestamp.UTC().Truncate(time.Microsecond)
	e.Normalize()
	if _, err := NewActivityIngester(pool).Insert(context.Background(), e, activity.EventKey(e)); err != nil {
		t.Fatalf("insert %s activity row: %v", e.Type, err)
	}
}

// readInterruptedDetail answers the stored interrupted_detail of runID, and
// whether the payload holds the key.
func readInterruptedDetail(t *testing.T, db *sql.DB, runID uuid.UUID) (string, bool) {
	t.Helper()
	var detail sql.NullString
	if err := db.QueryRow(`SELECT payload->>'interrupted_detail' FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&detail); err != nil {
		t.Fatalf("read interrupted_detail: %v", err)
	}
	return detail.String, detail.Valid
}

// assertNoRollMessage fails the test when the run row holds any part of
// the free-text message of the cell.rolled row.
func assertNoRollMessage(t *testing.T, db *sql.DB, runID uuid.UUID) {
	t.Helper()
	var payload string
	if err := db.QueryRow(`SELECT payload::text FROM varroa.runs WHERE run_id = $1`, runID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if strings.Contains(payload, "10.0.0.5") || strings.Contains(payload, "rolled the cell pod") {
		t.Fatalf("the run payload holds the cell.rolled message: %s", payload)
	}
}

// assertDetail fails the test unless the run's interrupted_detail is want.
// An empty want asserts that the payload holds no such key.
func assertDetail(t *testing.T, db *sql.DB, runID uuid.UUID, want string) {
	t.Helper()
	detail, found := readInterruptedDetail(t, db, runID)
	if want == "" && found {
		t.Fatalf("interrupted_detail = %q, want no key", detail)
	}
	if want != "" && detail != want {
		t.Fatalf("interrupted_detail = %q (found %v), want %q", detail, found, want)
	}
}

// TestCellRolledMarkStoresTheRollCause asserts that a cell-rolled mark
// stores the reason word of the operator pod delete that removed the run's
// pod: a cell.rolled or pod.replaced row of the run's cell whose instance is
// the run's owning instance, from after the run's creation to the mark
// time. Each other row stores no detail. The free-text message never
// reaches the run.
func TestCellRolledMarkStoresTheRollCause(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	cases := []struct {
		name string
		// rows answers the activity rows for the run's cell, the run's
		// owning instance, the run's creation time, and the mark time.
		rows       func(cell, instance string, created, mark time.Time) []activity.Event
		wantDetail string
	}{
		{
			name: "a cell.rolled row of the run's pod",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, instance, mark.Add(-time.Second), "MiteUnreachable")}
			},
			wantDetail: "MiteUnreachable",
		},
		{
			name: "a pod.replaced row of the run's pod",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{podReplacedRow(cell, instance, mark.Add(-time.Second))}
			},
			wantDetail: "JenkinsBootFailed",
		},
		{
			name: "a row of the run's pod at the mark time",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, instance, mark, "NotProvisioned")}
			},
			wantDetail: "NotProvisioned",
		},
		{
			name: "a row of the run's pod and a row of another pod",
			rows: func(cell, instance string, created, mark time.Time) []activity.Event {
				return []activity.Event{
					cellRolledRow(cell, otherPodInstance, created.Add(10*time.Second), "NotProvisioned"),
					cellRolledRow(cell, instance, mark.Add(-time.Second), "MiteUnreachable"),
				}
			},
			wantDetail: "MiteUnreachable",
		},
		{
			name: "a row of another pod of the cell",
			rows: func(cell, _ string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, otherPodInstance, mark.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row that names no pod",
			rows: func(cell, _ string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, "", mark.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of the run's pod before the run's creation",
			rows: func(cell, instance string, created, _ time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, instance, created.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of the run's pod after the mark",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, instance, mark.Add(time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of another cell",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell+"-other", instance, mark.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of another cluster",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, instance, mark.Add(-time.Second), "MiteUnreachable")
				e.Cluster = "member-b"
				return []activity.Event{e}
			},
		},
		{
			name: "a row of another namespace",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, instance, mark.Add(-time.Second), "MiteUnreachable")
				e.Namespace = "other-ns"
				return []activity.Event{e}
			},
		},
		{
			name: "a row of another type",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, instance, mark.Add(-time.Second), "MiteUnreachable")
				e.Type = "restart.approved"
				return []activity.Event{e}
			},
		},
		{
			name: "a reason that is not one word",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, instance, mark.Add(-time.Second), "rolled the cell pod")}
			},
		},
		{
			name: "a pod.replaced row with no reason",
			rows: func(cell, instance string, _, mark time.Time) []activity.Event {
				e := podReplacedRow(cell, instance, mark.Add(-time.Second))
				e.Reason = ""
				return []activity.Event{e}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := "roll-" + string(rune('a'+i))
			runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), cell)
			c := fetchRunningCandidate(t, pool, runID)
			if c.cellInstance == "" {
				t.Fatal("the candidate names no owning instance")
			}
			mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
			for _, e := range tc.rows(cell, c.cellInstance, c.createdAt, mark) {
				ingestActivityEvent(t, pool, e)
			}

			if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
				t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
			}

			assertDetail(t, db, runID, tc.wantDetail)
			assertNoRollMessage(t, db, runID)
		})
	}
}

// TestCellRolledMarkStoresTheRollOfTheRunsPod asserts that the instance,
// not the time order, picks the row. Each case holds two rolls of the cell
// in the window.
func TestCellRolledMarkStoresTheRollOfTheRunsPod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	cases := []struct {
		name string
		// rows answers the two rolls for the run's cell, the run's owning
		// instance, and the run's creation time. The mark comes one hour
		// after the creation.
		rows       func(cell, instance string, created time.Time) []activity.Event
		wantDetail string
	}{
		{
			// The run's pod rolls first. Its replacement rolls before the mark.
			name: "the replacement pod rolls later",
			rows: func(cell, instance string, created time.Time) []activity.Event {
				return []activity.Event{
					cellRolledRow(cell, instance, created.Add(time.Minute), "MiteUnreachable"),
					cellRolledRow(cell, otherPodInstance, created.Add(8*time.Minute), "NotProvisioned"),
				}
			},
			wantDetail: "MiteUnreachable",
		},
		{
			// An earlier pod rolls before the run's pod takes the run. The
			// run's pod rolls much later.
			name: "an earlier pod rolls first",
			rows: func(cell, instance string, created time.Time) []activity.Event {
				return []activity.Event{
					cellRolledRow(cell, otherPodInstance, created.Add(time.Minute), "NotProvisioned"),
					cellRolledRow(cell, instance, created.Add(55*time.Minute), "MiteUnreachable"),
				}
			},
			wantDetail: "MiteUnreachable",
		},
		{
			// Two deletes name the run's pod. The first one removed it.
			name: "the run's pod is named twice",
			rows: func(cell, instance string, created time.Time) []activity.Event {
				return []activity.Event{
					podReplacedRow(cell, instance, created.Add(2*time.Minute)),
					cellRolledRow(cell, instance, created.Add(5*time.Minute), "NotProvisioned"),
				}
			},
			wantDetail: "JenkinsBootFailed",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := "twice-" + string(rune('a'+i))
			runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), cell)
			c := fetchRunningCandidate(t, pool, runID)
			created := c.createdAt.UTC().Truncate(time.Microsecond)
			mark := created.Add(time.Hour)
			for _, e := range tc.rows(cell, c.cellInstance, created) {
				ingestActivityEvent(t, pool, e)
			}

			if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
				t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
			}
			assertDetail(t, db, runID, tc.wantDetail)
		})
	}
}

// TestCellRolledMarkStoresADeleteBeforeTheLastHeartbeat asserts that a
// delete of the run's pod names the cause also when the run sent a
// heartbeat after it. A deleted pod runs on through its termination grace.
func TestCellRolledMarkStoresADeleteBeforeTheLastHeartbeat(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "drain-a")
	c := fetchRunningCandidate(t, pool, runID)
	created := c.createdAt.UTC().Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`,
		created.Add(5*time.Minute), runID); err != nil {
		t.Fatalf("set heartbeat_received_at: %v", err)
	}
	c = fetchRunningCandidate(t, pool, runID)
	ingestActivityEvent(t, pool, cellRolledRow("drain-a", c.cellInstance, created.Add(time.Minute), "MiteUnreachable"))

	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
	}
	assertDetail(t, db, runID, "MiteUnreachable")
}

// TestMarkedLostIntentEndStoresTheRollOfItsPod asserts the cell-rolled end
// of a marked intent whose start event was lost. The run has no heartbeat,
// so the window starts at the intent's creation. A roll of an earlier pod
// sits in that window. The run stores the roll of the pod that marked the
// start.
func TestMarkedLostIntentEndStoresTheRollOfItsPod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	created := now.Add(-time.Hour)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, created, markedLostOwner, "", true)
	ingestActivityEvent(t, pool, cellRolledRow("cell-a", otherPodInstance, created.Add(time.Minute), "NotProvisioned"))
	ingestActivityEvent(t, pool, cellRolledRow("cell-a", markedLostOwner, created.Add(55*time.Minute), "MiteUnreachable"))

	intents, err := selectStaleMarkedIntents(context.Background(), pool, now, 10)
	if err != nil || len(intents) != 1 {
		t.Fatalf("select the marked intent: %d intents, err=%v, want 1", len(intents), err)
	}
	changed, err := endMarkedLostIntent(context.Background(), pool, intents[0], ReasonCellRolled, time.Time{}, now, "")
	if err != nil || !changed {
		t.Fatalf("end the marked intent: changed=%v err=%v, want a mark", changed, err)
	}
	assertDetail(t, db, runID, "MiteUnreachable")
}

// TestOtherMarksStoreNoRollCause asserts that only a cell-rolled mark reads
// the roll. A mark with another reason stores no detail, even when a
// cell.rolled row of the run's pod sits in the window.
func TestOtherMarksStoreNoRollCause(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	for i, reason := range []string{ReasonCellLost, ReasonTenantMoved, ReasonNoHeartbeat, ReasonResumeTimeout} {
		t.Run(reason, func(t *testing.T) {
			cell := "other-" + string(rune('a'+i))
			runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), cell)
			c := fetchRunningCandidate(t, pool, runID)
			mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
			ingestActivityEvent(t, pool, cellRolledRow(cell, c.cellInstance, mark.Add(-time.Second), "MiteUnreachable"))

			if changed, err := markInterrupted(context.Background(), pool, c, reason, mark, ""); err != nil || !changed {
				t.Fatalf("mark %s: changed=%v err=%v, want a mark", reason, changed, err)
			}
			if detail, found := readInterruptedDetail(t, db, runID); found {
				t.Fatalf("interrupted_detail = %q after a %s mark, want no key", detail, reason)
			}
		})
	}
}

// TestLateCellRolledRowFillsNothing asserts that a cell.rolled row that
// the activity ingest writes after the mark leaves the run with no detail.
func TestLateCellRolledRowFillsNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "late-a")
	c := fetchRunningCandidate(t, pool, runID)
	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)

	if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
	}
	ingestActivityEvent(t, pool, cellRolledRow("late-a", c.cellInstance, mark.Add(-time.Second), "MiteUnreachable"))

	if detail, found := readInterruptedDetail(t, db, runID); found {
		t.Fatalf("interrupted_detail = %q after a late row, want no key", detail)
	}
}

// TestRunStartedRemovesTheRollCauseOfAMarkedAcceptedRow asserts the queued
// order: a cell-rolled mark of an accepted QUEUED row stores the roll cause,
// and a later run.started supersedes the row and removes the cause with the
// reason.
func TestRunStartedRemovesTheRollCauseOfAMarkedAcceptedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), intentKey)); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	c := fetchRunningCandidate(t, pool, runID)
	if c.status != StatusQueued {
		t.Fatalf("candidate status = %q, want QUEUED", c.status)
	}
	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	ingestActivityEvent(t, pool, cellRolledRow(triggerAcceptedCell, c.cellInstance, mark.Add(-time.Second), "MiteUnreachable"))

	if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
	}
	if detail, _ := readInterruptedDetail(t, db, runID); detail != "MiteUnreachable" {
		t.Fatalf("interrupted_detail = %q after the mark, want MiteUnreachable", detail)
	}

	buildNumber := int64(3)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	payload := readRunPayload(t, db, runID.String())
	for _, key := range []string{"interrupted_reason", "interrupted_detail"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after a supersede", key)
		}
	}
}
