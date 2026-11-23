package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// runningOwnerInstance and runningStaleInstance are the two senders the
// tests in this file compare. cellRunGeneration is a placement
// generation a cell tenant can hold. A controller with no cell role
// always claims under zero.
const (
	runningOwnerInstance = "pod-owner"
	runningStaleInstance = "pod-stale"
	cellRunGeneration    = int64(1)
)

// runningFactsStageID is the one stage every running-fact round writes.
const runningFactsStageID = "stage-7"

// runningFacts is what one run holds of the four running-build kinds:
// the name of its one stage row, its branch, its change list, and its
// pending set. Each field is empty when no write stored it.
type runningFacts struct {
	stageName string
	branch    string
	changes   string
	inputs    string
}

// readRunningFacts reads the four running-build facts of runID. A missing
// runs row answers empty strings for the three runs columns.
func readRunningFacts(t *testing.T, db *sql.DB, runID uuid.UUID) runningFacts {
	t.Helper()
	var f runningFacts
	if err := db.QueryRow(`
		SELECT COALESCE((SELECT name FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2), ''),
		       COALESCE((SELECT branch FROM varroa.runs WHERE run_id = $1), ''),
		       COALESCE((SELECT changes::text FROM varroa.runs WHERE run_id = $1), ''),
		       COALESCE((SELECT inputs::text FROM varroa.runs WHERE run_id = $1), '')
	`, runID, runningFactsStageID).Scan(&f.stageName, &f.branch, &f.changes, &f.inputs); err != nil {
		t.Fatalf("read running facts: %v", err)
	}
	return f
}

// assertRunningFacts fails the test unless every fact of runID carries tag.
func assertRunningFacts(t *testing.T, db *sql.DB, runID uuid.UUID, tag string) {
	t.Helper()
	f := readRunningFacts(t, db, runID)
	if f.stageName != tag {
		t.Errorf("run_stages.name = %q, want %q", f.stageName, tag)
	}
	if f.branch != tag {
		t.Errorf("runs.branch = %q, want %q", f.branch, tag)
	}
	if !strings.Contains(f.changes, tag) {
		t.Errorf("runs.changes = %q, want a change named %q", f.changes, tag)
	}
	if !strings.Contains(f.inputs, tag) {
		t.Errorf("runs.inputs = %q, want an input named %q", f.inputs, tag)
	}
}

// runningFactEnvelopes builds one envelope of each running-build kind for
// runID, sent by sender. Every value carries tag, and both sequenced
// kinds carry seq.
func runningFactEnvelopes(t *testing.T, runID uuid.UUID, intentKey, sender, tag string, seq int64) []IngestEnvelope {
	t.Helper()
	branch := tag
	changes, err := json.Marshal([]map[string]string{{"commit_id": tag}})
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	envs := []IngestEnvelope{
		newTestEnvelope(t, runID, intentKey, KindRunStage, RunStagePayload{
			StageID: runningFactsStageID, Name: tag, NodeType: "stage", Status: "SUCCESS",
		}),
		newTestEnvelope(t, runID, intentKey, KindRunSource, RunSourcePayload{Seq: seq, Branch: &branch}),
		newTestEnvelope(t, runID, intentKey, KindRunChanges, RunChangesPayload{Changes: changes}),
		newTestEnvelope(t, runID, intentKey, KindRunInput, RunInputPayload{
			Seq: inputSeqPtr(seq), Inputs: []StoredRunInput{{ID: tag, Message: "Deploy?"}},
		}),
	}
	for i := range envs {
		envs[i].CellInstance = sender
	}
	return envs
}

// applyRunningFacts applies one round of runningFactEnvelopes and answers
// the result of each write. Every write must answer no error, so the
// consumer acknowledges each message.
func applyRunningFacts(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, intentKey, sender, tag string, seq int64) []ApplyResult {
	t.Helper()
	g := NewIngester(pool, nil, nil)
	envs := runningFactEnvelopes(t, runID, intentKey, sender, tag, seq)
	results := make([]ApplyResult, 0, len(envs))
	for _, env := range envs {
		res, err := g.Apply(context.Background(), env)
		if err != nil {
			t.Fatalf("apply %s from %q: %v", env.Kind, sender, err)
		}
		results = append(results, res)
	}
	return results
}

// assertForeignSender fails the test unless every result answers want.
func assertForeignSender(t *testing.T, results []ApplyResult, want bool) {
	t.Helper()
	kinds := []string{KindRunStage, KindRunSource, KindRunChanges, KindRunInput}
	for i, res := range results {
		if res.ForeignSender != want {
			t.Errorf("%s: ForeignSender = %v, want %v", kinds[i], res.ForeignSender, want)
		}
	}
}

// seedOwnedRun claims one run under generation for owner and applies its
// run.started from owner. It answers the run and its intent key.
func seedOwnedRun(t *testing.T, pool *pgxpool.Pool, owner string, generation int64) (uuid.UUID, string) {
	t.Helper()
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), generation)
	req.CellInstance = owner
	resp := mustGrant(t, NewLedger(pool, nil), req)
	runID := uuid.MustParse(resp.RunID)
	started := newTestEnvelope(t, runID, req.IntentKey, KindRunStarted, RunStartedPayload{})
	started.CellInstance = owner
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	return runID, req.IntentKey
}

// seedMovedCellRun claims one cell run for meshDeadInstance, gives it a
// usable mesh record, and runs a real takeover round. The round moves the
// run's owner to meshResumeTarget, so the test starts from the state a
// resume dispatch itself writes.
func seedMovedCellRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB) (uuid.UUID, string) {
	t.Helper()
	runID, intentKey := seedOwnedRun(t, pool, meshDeadInstance, cellRunGeneration)
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh', jsonb_build_object(
		         'state_at', now(),
		         'files', jsonb_build_array(
		             jsonb_build_object('relpath', $2::text, 'bytes', 100, 'seq', 1, 'state_gen', 1,
		                                'epoch', $3::bigint))))
		 WHERE run_id = $1
	`, runID, meshBuildRecordPath, meshPusherEpoch); err != nil {
		t.Fatalf("seed mesh state: %v", err)
	}
	seedMeshPlacement(t, db, meshResumeTarget, meshResumeTarget)
	conn := newTestActionConn(t)
	collectActions(t, conn)
	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("published = %d, want 1 dispatched resume", got)
	}
	var owner string
	if err := db.QueryRow(`SELECT cell_instance FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&owner); err != nil {
		t.Fatalf("read the intent owner: %v", err)
	}
	if owner != meshResumeTarget {
		t.Fatalf("intent owner = %q, want %q", owner, meshResumeTarget)
	}
	return runID, intentKey
}

// TestRunningFactsFromTheOwnerMerge asserts the owner of a cell run
// writes its stage, source, change list, and pending set.
func TestRunningFactsFromTheOwnerMerge(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, runningOwnerInstance, cellRunGeneration)

	results := applyRunningFacts(t, pool, runID, intentKey, runningOwnerInstance, "owner", 1)

	assertForeignSender(t, results, false)
	assertRunningFacts(t, db, runID, "owner")
}

// TestRunningFactsFromAStaleCellAfterAMoveAreDropped is the negative
// proof. A cell that the mesh moved a run away from can connect again and
// replay its buffered events. Each of them must write nothing, also with
// a higher seq than the new owner's and with an occurred_at from before
// the move. Each must answer no error, so the consumer acknowledges it.
// The new owner's own facts still merge.
func TestRunningFactsFromAStaleCellAfterAMoveAreDropped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedMovedCellRun(t, pool, db)

	live := applyRunningFacts(t, pool, runID, intentKey, meshResumeTarget, "live", 5)
	assertForeignSender(t, live, false)
	assertRunningFacts(t, db, runID, "live")

	g := NewIngester(pool, nil, nil)
	stale := runningFactEnvelopes(t, runID, intentKey, meshDeadInstance, "stale", 9)
	results := make([]ApplyResult, 0, len(stale))
	for _, env := range stale {
		env.OccurredAt = time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
		res, err := g.Apply(context.Background(), env)
		if err != nil {
			t.Fatalf("apply %s from the stale cell: %v, want no error", env.Kind, err)
		}
		if _, ok := notificationFor(env, res); ok {
			t.Errorf("%s from the stale cell earns a notification, want none", env.Kind)
		}
		results = append(results, res)
	}

	assertForeignSender(t, results, true)
	assertRunningFacts(t, db, runID, "live")
}

// TestRunningFactsFromANonOwnerBeforeAnyRunRowAreDropped asserts the
// owner test holds on a cell run that has no runs row yet. The join then
// reads no payload, and the test must answer false, never NULL.
func TestRunningFactsFromANonOwnerBeforeAnyRunRowAreDropped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), cellRunGeneration)
	req.CellInstance = runningOwnerInstance
	runID := uuid.MustParse(mustGrant(t, NewLedger(pool, nil), req).RunID)

	results := applyRunningFacts(t, pool, runID, req.IntentKey, runningStaleInstance, "stale", 1)

	assertForeignSender(t, results, true)
	var rows int
	if err := db.QueryRow(`
		SELECT (SELECT count(*) FROM varroa.runs WHERE run_id = $1)
		     + (SELECT count(*) FROM varroa.run_stages WHERE run_id = $1)
	`, runID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want no runs row and no stage row", rows)
	}
}

// TestRunningFactsAfterAJenkinsRestartInTheSamePodMerge asserts a Jenkins
// restart inside one pod keeps the pod's instance, so the restarted
// process still writes every fact of its run.
func TestRunningFactsAfterAJenkinsRestartInTheSamePodMerge(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, runningOwnerInstance, cellRunGeneration)

	applyRunningFacts(t, pool, runID, intentKey, runningOwnerInstance, "before-restart", 1)
	results := applyRunningFacts(t, pool, runID, intentKey, runningOwnerInstance, "after-restart", 2)

	assertForeignSender(t, results, false)
	assertRunningFacts(t, db, runID, "after-restart")
}

// TestRunningFactsOfAControllerWithNoCellRoleMergeFromAReplacementPod
// asserts a run claimed under generation zero takes every sender. A
// controller with no cell role keeps its Jenkins home on a volume, so a
// replacement pod resumes the build with no new claim, and the intent
// still names the old pod.
func TestRunningFactsOfAControllerWithNoCellRoleMergeFromAReplacementPod(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, runningOwnerInstance, 0)

	results := applyRunningFacts(t, pool, runID, intentKey, runningStaleInstance, "replacement", 1)

	assertForeignSender(t, results, false)
	assertRunningFacts(t, db, runID, "replacement")
}

// TestRunningFactsOfAnIntentWithNoInstanceMerge asserts a cell run whose
// intent names no instance takes every sender, because the test has no
// owner to compare.
func TestRunningFactsOfAnIntentWithNoInstanceMerge(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, "", cellRunGeneration)

	results := applyRunningFacts(t, pool, runID, intentKey, runningStaleInstance, "unowned", 1)

	assertForeignSender(t, results, false)
	assertRunningFacts(t, db, runID, "unowned")
}

// TestRunningFactsWithNoSenderMerge asserts an envelope that names no
// sender instance still writes, because the test has no sender to
// compare.
func TestRunningFactsWithNoSenderMerge(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, runningOwnerInstance, cellRunGeneration)

	results := applyRunningFacts(t, pool, runID, intentKey, "", "anonymous", 1)

	assertForeignSender(t, results, false)
	assertRunningFacts(t, db, runID, "anonymous")
}

// TestRunningFactsWithNoIntentStayRetryable asserts the owner test never
// runs for a run with no run_intents row. Each kind still answers
// ErrIntentMissing, which the consumer retries, exactly as before the
// test existed.
func TestRunningFactsWithNoIntentStayRetryable(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := uuid.Must(uuid.NewV7())
	for _, env := range runningFactEnvelopes(t, runID, "manual:no-intent", runningStaleInstance, "orphan", 1) {
		res, err := g.Apply(context.Background(), env)
		if !errors.Is(err, ErrIntentMissing) {
			t.Fatalf("apply %s: err = %v, want ErrIntentMissing", env.Kind, err)
		}
		if res.ForeignSender {
			t.Fatalf("apply %s: ForeignSender = true, want false", env.Kind)
		}
	}
}

// TestIngestConsumerAcknowledgesAForeignRunningFact asserts the consumer
// acknowledges a running-build fact that the owner test refuses. The
// message leaves the stream once, with no redelivery, and writes no row.
func TestIngestConsumerAcknowledgesAForeignRunningFact(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID, intentKey := seedOwnedRun(t, pool, runningOwnerInstance, cellRunGeneration)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, NewIngester(pool, conn, nil), nil, 24*time.Hour, nil)
	}()

	stale := runningFactEnvelopes(t, runID, intentKey, runningStaleInstance, "stale", 1)[0]
	publishBuildEvent(t, conn, stale)

	waitForRow(t, func() bool {
		info, err := conn.JetStream().ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
	info, err := conn.JetStream().ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumRedelivered != 0 {
		t.Fatalf("redelivered = %d, want 0", info.NumRedelivered)
	}
	if f := readRunningFacts(t, db, runID); f.stageName != "" {
		t.Fatalf("run_stages.name = %q, want no stage row", f.stageName)
	}
}
