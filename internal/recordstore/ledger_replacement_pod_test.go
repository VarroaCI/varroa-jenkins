package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// markedControllerRun is a run that the claim granted to replacedPod
// under one placement generation and that replacedPod marked. No build
// record stands behind it, and its accepted row is QUEUED.
type markedControllerRun struct {
	ledger *Ledger
	db     *sql.DB
	req    ClaimRequest
	grant  ClaimResponse
	// next is job_numbers.next_number after the grant.
	next int64
	// markedAt is the start mark that replacedPod stored.
	markedAt time.Time
}

// seedMarkedControllerRun grants one run to replacedPod through the
// placement fence and the grant hold, stores its start mark, and seeds
// its accepted row. The placement row then names replacementPod, as the
// operator announces it after its pod read. The hold fails the test when
// it reads the ingest consumer.
func seedMarkedControllerRun(t *testing.T, generation int64) markedControllerRun {
	t.Helper()
	pool, db := setupLedgerPool(t)
	seedControllerPlacement(t, db, generation, replacedPod)
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))
	ledger.hold = newGrantHold(noConsumerRead(t), 0, 0)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), generation)
	req.CellInstance = replacedPod
	req.JenkinsNextBuildNumber = 1
	grant := mustGrant(t, ledger, req)
	mustMark(t, ledger, req)
	seedAcceptedRunRow(t, db, req)
	markedAt := readStartMarkedAt(t, db, req.IntentKey)

	announceControllerPod(t, db, replacementPod)
	return markedControllerRun{ledger: ledger, db: db, req: req, grant: grant, next: readNextNumber(t, db),
		markedAt: *markedAt}
}

// announceControllerPod names instance as the current pod of the test
// controller, as the operator's announce writes it.
func announceControllerPod(t *testing.T, db *sql.DB, instance string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET cell_instance = $3, rehydrated_instance = $3
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, instance); err != nil {
		t.Fatalf("announce %s: %v", instance, err)
	}
}

// fromReplacementPod returns the run's claim as the replacement pod sends
// it after it loads the queue item from the volume. The job's counter on
// the volume still holds the granted number.
func (m markedControllerRun) fromReplacementPod() ClaimRequest {
	repeat := m.req
	repeat.CellInstance = replacementPod
	repeat.JenkinsNextBuildNumber = m.grant.BuildNumber
	return repeat
}

// runStatusOf reads the status of the run's own runs row.
func runStatusOf(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return status
}

// TestReplacementPodClaimOfAMarkedRunIsTheOwnerRepeat proves the
// replacement pod rule on the claim path. The replaced pod marked the
// start and the build never started: the volume still holds the granted
// number as the job's next number. The replacement pod loads the queue
// item from the volume and claims the same run. The claim is the owner's
// own repeat: it answers the stored number, moves the run to the
// replacement pod, and reserves no number. The pod's start mark and its
// next repeat then answer granted and change nothing.
func TestReplacementPodClaimOfAMarkedRunIsTheOwnerRepeat(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	repeat := m.fromReplacementPod()

	resp := m.ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != m.grant.RunID || resp.BuildNumber != m.grant.BuildNumber {
		t.Fatalf("replacement pod claim = %+v, want granted for run %s with number %d",
			resp, m.grant.RunID, m.grant.BuildNumber)
	}
	cell, owner, generation, number := readIntentOwnerAndNumber(t, m.db, m.req.IntentKey)
	if cell != "cell-a" || owner != replacementPod || generation != 0 || number != m.grant.BuildNumber {
		t.Fatalf("intent = %s/%s generation %d number %d, want cell-a/%s generation 0 number %d",
			cell, owner, generation, number, replacementPod, m.grant.BuildNumber)
	}
	if next := readNextNumber(t, m.db); next != m.next {
		t.Fatalf("next_number = %d, want %d: the claim must reserve no number", next, m.next)
	}
	if at := readStartMarkedAt(t, m.db, m.req.IntentKey); at == nil || !at.Equal(m.markedAt) {
		t.Fatalf("start_marked_at = %v, want the stored mark %v", at, m.markedAt)
	}
	if status := runStatusOf(t, m.db, m.req.RunID); status != "QUEUED" {
		t.Fatalf("run status = %s, want QUEUED", status)
	}

	mark := m.ledger.Claim(context.Background(), startMarkOf(repeat))
	if mark.Decision != DecisionGranted || mark.BuildNumber != m.grant.BuildNumber {
		t.Fatalf("start mark of the replacement pod = %+v, want granted with number %d", mark, m.grant.BuildNumber)
	}
	if at := readStartMarkedAt(t, m.db, m.req.IntentKey); at == nil || !at.Equal(m.markedAt) {
		t.Fatalf("start_marked_at = %v after the mark, want the stored mark %v", at, m.markedAt)
	}
	again := m.ledger.Claim(context.Background(), repeat)
	if again.Decision != DecisionGranted || again.BuildNumber != m.grant.BuildNumber {
		t.Fatalf("second claim of the replacement pod = %+v, want granted with number %d", again, m.grant.BuildNumber)
	}
	if next := readNextNumber(t, m.db); next != m.next {
		t.Fatalf("next_number = %d after the repeats, want %d", next, m.next)
	}
	assertIntentRowCount(t, m.db, m.req.IntentKey, 1)
}

// TestReplacementPodClaimWaitsForTheAnnounce proves the fence order. The
// replacement pod can claim before the operator names it. The fence then
// answers the retryable NOT_READY before the decision table runs, and
// the run keeps the replaced pod. After the announce, the claim moves it.
func TestReplacementPodClaimWaitsForTheAnnounce(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	announceControllerPod(t, m.db, replacedPod)
	repeat := m.fromReplacementPod()

	early := m.ledger.Claim(context.Background(), repeat)
	if early.Decision != DecisionVetoed || early.ReasonCode != ReasonNotReady {
		t.Fatalf("claim before the announce = %+v, want vetoed NOT_READY", early)
	}
	if owner := readOwner(t, m.db, m.req.IntentKey); owner != replacedPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacedPod)
	}

	announceControllerPod(t, m.db, replacementPod)
	if resp := m.ledger.Claim(context.Background(), repeat); resp.Decision != DecisionGranted {
		t.Fatalf("claim after the announce = %+v, want granted", resp)
	}
	if owner := readOwner(t, m.db, m.req.IntentKey); owner != replacementPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacementPod)
	}
}

// TestReplacementPodClaimSignalsTheOwnerMove proves that the move sends
// one owner move signal after its commit, so every gateway replica drops
// the replaced pod's cached match. The next repeat of the same pod moves
// nothing and sends nothing.
func TestReplacementPodClaimSignalsTheOwnerMove(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	conn := newTestActionConn(t)
	m.ledger.SetOwnerMoves(conn, nil)
	received := collectLedgerMessages(t, conn, m.db)
	repeat := m.fromReplacementPod()

	if resp := m.ledger.Claim(context.Background(), repeat); resp.Decision != DecisionGranted {
		t.Fatalf("replacement pod claim = %+v, want granted", resp)
	}
	if got := expectOwnerMove(t, received, m.grant.RunID); !got.found || got.instance != replacementPod {
		t.Fatalf("ledger read on the signal = %+v, want the new owner %s", got, replacementPod)
	}
	if resp := m.ledger.Claim(context.Background(), repeat); resp.Decision != DecisionGranted {
		t.Fatalf("second claim = %+v, want granted", resp)
	}
	expectNoLedgerMessage(t, conn, received)
}

// TestReplacementPodClaimMovesTheFleetLock proves that the claim move
// takes the run's fleet lock with it, as the ingest takeover does. The
// release confirmation and the force release read the lock's cell
// instance.
func TestReplacementPodClaimMovesTheFleetLock(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	runID := uuid.MustParse(m.grant.RunID)
	seedReplacedPodLock(t, m.db, runID, time.Now().UTC())

	if resp := m.ledger.Claim(context.Background(), m.fromReplacementPod()); resp.Decision != DecisionGranted {
		t.Fatalf("replacement pod claim = %+v, want granted", resp)
	}
	if holder := lockHolderOf(t, m.db, runID); holder != replacementPod {
		t.Fatalf("lock cell_instance = %q, want %q", holder, replacementPod)
	}
}

// TestReplacementPodClaimKeepsTheCapSlot proves that a concurrency cap
// counts the run once. The run keeps its slot on the same cell, so the
// claim of the replacement pod passes the cap at once and writes no wait
// row. A new run of the tenant still waits for that slot.
func TestReplacementPodClaimKeepsTheCapSlot(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	policy, err := json.Marshal(capTenantLimitOne)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := m.db.Exec(`UPDATE varroa.tenant_placements SET policy = $3::jsonb WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant, string(policy)); err != nil {
		t.Fatalf("set the cap policy: %v", err)
	}

	if resp := m.ledger.Claim(context.Background(), m.fromReplacementPod()); resp.Decision != DecisionGranted {
		t.Fatalf("replacement pod claim = %+v, want granted", resp)
	}
	if n := waitRowCount(t, m.db, m.req.RunID); n != 0 {
		t.Fatalf("wait rows = %d, want 0", n)
	}
	other := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	other.CellInstance = replacementPod
	if resp := m.ledger.Claim(context.Background(), other); resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonCapped {
		t.Fatalf("new run = %+v, want vetoed CAPPED", resp)
	}
}

// TestMarkedRepeatThatTheReplacementPodRuleRefusesAnswersDuplicate proves
// each condition of the claim move. In each case the repeat answers
// duplicate, the intent keeps the replaced pod, the run's own row stays
// QUEUED, and the claim reserves no number. A cell run is the case the
// rule must never reach: a cell keeps its Jenkins home on ephemeral
// storage, so another pod of the cell cannot hold the build. A counter
// past the granted number means that the volume can hold the build, and
// Jenkins on the replacement pod then resumes it with no claim.
func TestMarkedRepeatThatTheReplacementPodRuleRefusesAnswersDuplicate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		generation int64
		// allowAll replaces the placement fence, so the case reaches the
		// decision table with a sender that the fence would hold.
		allowAll bool
		edit     func(t *testing.T, m markedControllerRun, repeat *ClaimRequest)
	}{
		{name: "a cell run", generation: cellRunGeneration},
		{name: "a counter past the granted number", edit: func(_ *testing.T, m markedControllerRun, repeat *ClaimRequest) {
			repeat.JenkinsNextBuildNumber = m.grant.BuildNumber + 1
		}},
		{name: "a zero counter", edit: func(_ *testing.T, _ markedControllerRun, repeat *ClaimRequest) {
			repeat.JenkinsNextBuildNumber = 0
		}},
		{name: "a taken number", edit: func(t *testing.T, m markedControllerRun, _ *ClaimRequest) {
			if _, err := m.db.Exec(`UPDATE varroa.run_intents SET build_number = NULL WHERE intent_key = $1`,
				m.req.IntentKey); err != nil {
				t.Fatalf("take the number: %v", err)
			}
		}},
		{name: "a pod the placement does not name", allowAll: true,
			edit: func(_ *testing.T, _ markedControllerRun, repeat *ClaimRequest) {
				repeat.CellInstance = "pod-other"
			}},
		{name: "another controller", allowAll: true, edit: func(_ *testing.T, _ markedControllerRun, repeat *ClaimRequest) {
			repeat.Cell = "cell-b"
		}},
		{name: "no cell instance", allowAll: true, edit: func(_ *testing.T, _ markedControllerRun, repeat *ClaimRequest) {
			repeat.CellInstance = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := seedMarkedControllerRun(t, tc.generation)
			ledger := m.ledger
			if tc.allowAll {
				ledger = NewLedger(ledger.pool, nil)
				ledger.hold = newGrantHold(noConsumerRead(t), 0, 0)
			}
			repeat := m.fromReplacementPod()
			if tc.edit != nil {
				tc.edit(t, m, &repeat)
			}

			resp := ledger.Claim(context.Background(), repeat)
			if resp.Decision != DecisionDuplicate || resp.RunID != m.grant.RunID {
				t.Fatalf("repeat = %+v, want duplicate for run %s", resp, m.grant.RunID)
			}
			if owner := readOwner(t, m.db, m.req.IntentKey); owner != replacedPod {
				t.Fatalf("intent owner = %q, want %q", owner, replacedPod)
			}
			if status := runStatusOf(t, m.db, m.req.RunID); status != "QUEUED" {
				t.Fatalf("run status = %s, want QUEUED", status)
			}
			if next := readNextNumber(t, m.db); next != m.next {
				t.Fatalf("next_number = %d, want %d", next, m.next)
			}
		})
	}
}

// TestReplacementPodClaimOfAStartedRunAnswersDuplicate proves that the
// started test runs before the move. The replaced pod's start event
// consumed the intent, so the build is on the volume, and Jenkins on the
// replacement pod resumes it with no claim.
func TestReplacementPodClaimOfAStartedRunAnswersDuplicate(t *testing.T) {
	t.Parallel()

	m := seedMarkedControllerRun(t, 0)
	started := newTestEnvelope(t, uuid.MustParse(m.grant.RunID), m.req.IntentKey, KindRunStarted,
		RunStartedPayload{BuildNumber: &m.grant.BuildNumber})
	started.CellInstance = replacedPod
	if _, err := NewIngester(m.ledger.pool, nil, nil).Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	resp := m.ledger.Claim(context.Background(), m.fromReplacementPod())
	if resp.Decision != DecisionDuplicate || resp.RunID != m.grant.RunID {
		t.Fatalf("repeat = %+v, want duplicate for run %s", resp, m.grant.RunID)
	}
	if owner := readOwner(t, m.db, m.req.IntentKey); owner != replacedPod {
		t.Fatalf("intent owner = %q, want %q", owner, replacedPod)
	}
}
