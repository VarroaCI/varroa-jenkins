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

// cellRolledRow answers the operator's cell.rolled event for cell at ts.
func cellRolledRow(cell string, ts time.Time, reason string) activity.Event {
	return activity.Event{
		Timestamp:  ts,
		Type:       "cell.rolled",
		Source:     "operator",
		Cluster:    testCluster,
		Namespace:  testNamespace,
		Controller: cell,
		Message:    rollCauseMessage,
		Reason:     reason,
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

// TestCellRolledMarkStoresTheRollCause asserts that a cell-rolled mark
// stores the reason word of the oldest cell.rolled row of the run's cell,
// from after the run's liveness baseline to the mark time. Each other row
// stores no detail. The free-text message never reaches the run.
func TestCellRolledMarkStoresTheRollCause(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	cases := []struct {
		name string
		// rows answers the activity rows for the run's cell, the run's
		// liveness baseline, and the mark time.
		rows       func(cell string, baseline, mark time.Time) []activity.Event
		wantDetail string
	}{
		{
			name: "the oldest row in the window",
			rows: func(cell string, baseline, mark time.Time) []activity.Event {
				return []activity.Event{
					cellRolledRow(cell, mark.Add(-time.Second), "MiteUnreachable"),
					cellRolledRow(cell, baseline.Add(10*time.Second), "NotProvisioned"),
				}
			},
			wantDetail: "NotProvisioned",
		},
		{
			name: "a row at the mark time",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, mark, "NotProvisioned")}
			},
			wantDetail: "NotProvisioned",
		},
		{
			name: "a row before the baseline",
			rows: func(cell string, baseline, _ time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, baseline.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row after the mark",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, mark.Add(time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of another cell",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell+"-other", mark.Add(-time.Second), "MiteUnreachable")}
			},
		},
		{
			name: "a row of another cluster",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, mark.Add(-time.Second), "MiteUnreachable")
				e.Cluster = "member-b"
				return []activity.Event{e}
			},
		},
		{
			name: "a row of another namespace",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, mark.Add(-time.Second), "MiteUnreachable")
				e.Namespace = "other-ns"
				return []activity.Event{e}
			},
		},
		{
			name: "a row of another type",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				e := cellRolledRow(cell, mark.Add(-time.Second), "MiteUnreachable")
				e.Type = "pod.replaced"
				return []activity.Event{e}
			},
		},
		{
			name: "a reason that is not one word",
			rows: func(cell string, _, mark time.Time) []activity.Event {
				return []activity.Event{cellRolledRow(cell, mark.Add(-time.Second), "rolled the cell pod")}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := "roll-" + string(rune('a'+i))
			runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), cell)
			c := fetchRunningCandidate(t, pool, runID)
			baseline := livenessBaseline(c)
			mark := baseline.Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
			for _, e := range tc.rows(cell, baseline, mark) {
				ingestActivityEvent(t, pool, e)
			}

			if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
				t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
			}

			detail, found := readInterruptedDetail(t, db, runID)
			if tc.wantDetail == "" && found {
				t.Fatalf("interrupted_detail = %q, want no key", detail)
			}
			if tc.wantDetail != "" && detail != tc.wantDetail {
				t.Fatalf("interrupted_detail = %q (found %v), want %q", detail, found, tc.wantDetail)
			}
			assertNoRollMessage(t, db, runID)
		})
	}
}

// TestCellRolledMarkStoresTheFirstRollOfTheWindow asserts the two-roll
// order: the run's pod rolls for MiteUnreachable, and its replacement pod
// rolls for NotProvisioned before the mark. The first roll removed the pod
// of the run, so the mark stores MiteUnreachable.
func TestCellRolledMarkStoresTheFirstRollOfTheWindow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "twice-a")
	c := fetchRunningCandidate(t, pool, runID)
	baseline := livenessBaseline(c)
	mark := baseline.Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	ingestActivityEvent(t, pool, cellRolledRow("twice-a", baseline.Add(time.Minute), "MiteUnreachable"))
	ingestActivityEvent(t, pool, cellRolledRow("twice-a", baseline.Add(8*time.Minute), "NotProvisioned"))

	if changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, mark, ""); err != nil || !changed {
		t.Fatalf("mark cell-rolled: changed=%v err=%v, want a mark", changed, err)
	}
	if detail, _ := readInterruptedDetail(t, db, runID); detail != "MiteUnreachable" {
		t.Fatalf("interrupted_detail = %q, want MiteUnreachable, the cause of the first roll", detail)
	}
}

// TestOtherMarksStoreNoRollCause asserts that only a cell-rolled mark reads
// the roll. A mark with another reason stores no detail, even when a
// cell.rolled row of the run's cell sits in the window.
func TestOtherMarksStoreNoRollCause(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	for i, reason := range []string{ReasonCellLost, ReasonTenantMoved, ReasonNoHeartbeat, ReasonResumeTimeout} {
		t.Run(reason, func(t *testing.T) {
			cell := "other-" + string(rune('a'+i))
			runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), cell)
			c := fetchRunningCandidate(t, pool, runID)
			mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
			ingestActivityEvent(t, pool, cellRolledRow(cell, mark.Add(-time.Second), "MiteUnreachable"))

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
	ingestActivityEvent(t, pool, cellRolledRow("late-a", mark.Add(-time.Second), "MiteUnreachable"))

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
	ingestActivityEvent(t, pool, cellRolledRow(triggerAcceptedCell, mark.Add(-time.Second), "MiteUnreachable"))

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
