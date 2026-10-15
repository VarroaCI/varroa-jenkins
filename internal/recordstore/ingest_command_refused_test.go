package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newCommandRefusedEnvelope returns a run.command_refused envelope for
// runID with the test coordinates and the given refusal time.
func newCommandRefusedEnvelope(t *testing.T, runID uuid.UUID, command, actor string, at time.Time) IngestEnvelope {
	t.Helper()
	env := newTestEnvelope(t, runID, "", KindRunCommandRefused, RunCommandRefusedPayload{
		Command:    command,
		Actor:      actor,
		HTTPStatus: 404,
		Error:      "cancel build " + testJobPath + ": HTTP 404: Not Found",
	})
	env.OccurredAt = at.UTC().Truncate(time.Microsecond)
	return env
}

// storedCommandRefusal is one run's command_refusal payload key.
type storedCommandRefusal struct {
	Command    string    `json:"command"`
	Actor      string    `json:"actor"`
	HTTPStatus int       `json:"http_status"`
	Error      string    `json:"error"`
	RefusedAt  time.Time `json:"refused_at"`
}

// readCommandRefusal reads one run's command_refusal key, answering nil
// for an absent key.
func readCommandRefusal(t *testing.T, db *sql.DB, runID uuid.UUID) *storedCommandRefusal {
	t.Helper()
	var raw sql.NullString
	if err := db.QueryRow(`SELECT payload->'command_refusal' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read command refusal: %v", err)
	}
	if !raw.Valid {
		return nil
	}
	var out storedCommandRefusal
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		t.Fatalf("decode command refusal %s: %v", raw.String, err)
	}
	return &out
}

// TestApplyCommandRefusedStoresTheRefusal asserts the write merges one
// notice into the run's payload and keeps every other payload key.
func TestApplyCommandRefusedStoresTheRefusal(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	at := time.Now().Add(-time.Minute)
	res, err := g.Apply(context.Background(), newCommandRefusedEnvelope(t, runID, RefusedCommandCancel, "alice", at))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.CommandRefusedStored || res.CommandRefused != RefusedCommandCancel {
		t.Fatalf("result = %+v, want a stored cancel notice", res)
	}
	got := readCommandRefusal(t, db, runID)
	if got == nil {
		t.Fatal("command_refusal is absent")
	}
	if got.Command != RefusedCommandCancel || got.Actor != "alice" || got.HTTPStatus != 404 || got.Error == "" {
		t.Fatalf("notice = %+v", got)
	}
	if !got.RefusedAt.Equal(at.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("refused_at = %v, want %v", got.RefusedAt, at)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING: a notice changes no run state", status)
	}
}

// TestApplyCommandRefusedIgnoresAnotherTenantsRun asserts an envelope that
// names another tenant or another job writes nothing onto the run.
func TestApplyCommandRefusedIgnoresAnotherTenantsRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	for name, mutate := range map[string]func(*IngestEnvelope){
		"tenant":  func(e *IngestEnvelope) { e.Tenant = "other-team" },
		"job":     func(e *IngestEnvelope) { e.JobPath = testTenant + "/other-job" },
		"cluster": func(e *IngestEnvelope) { e.Cluster = "other-cluster" },
	} {
		env := newCommandRefusedEnvelope(t, runID, RefusedCommandCancel, "mallory", time.Now())
		mutate(&env)
		res, err := g.Apply(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: Apply: %v", name, err)
		}
		if res.CommandRefusedStored {
			t.Fatalf("%s: stored a notice on a run the envelope does not own", name)
		}
	}
	if got := readCommandRefusal(t, db, runID); got != nil {
		t.Fatalf("notice = %+v, want none", got)
	}
}

// TestApplyCommandRefusedKeepsTheNewerRefusal asserts an older notice that
// arrives late leaves the newer stored notice in place, and a newer
// notice replaces an older one.
func TestApplyCommandRefusedKeepsTheNewerRefusal(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	newer := time.Now().Add(-time.Minute)
	older := newer.Add(-time.Minute)
	if res, err := g.Apply(context.Background(), newCommandRefusedEnvelope(t, runID, RefusedCommandProceed, "bob", newer)); err != nil || !res.CommandRefusedStored {
		t.Fatalf("newer: res=%+v err=%v", res, err)
	}
	res, err := g.Apply(context.Background(), newCommandRefusedEnvelope(t, runID, RefusedCommandCancel, "alice", older))
	if err != nil {
		t.Fatalf("older: Apply: %v", err)
	}
	if res.CommandRefusedStored {
		t.Fatal("older notice replaced a newer one")
	}
	if got := readCommandRefusal(t, db, runID); got == nil || got.Command != RefusedCommandProceed || got.Actor != "bob" {
		t.Fatalf("notice = %+v, want the newer proceed notice", got)
	}

	latest := time.Now()
	if res, err := g.Apply(context.Background(), newCommandRefusedEnvelope(t, runID, RefusedCommandAbort, "carol", latest)); err != nil || !res.CommandRefusedStored {
		t.Fatalf("latest: res=%+v err=%v", res, err)
	}
	if got := readCommandRefusal(t, db, runID); got == nil || got.Command != RefusedCommandAbort || got.Actor != "carol" {
		t.Fatalf("notice = %+v, want the latest abort notice", got)
	}
}

// TestApplyCommandRefusedWithNoRunIsANoOp asserts a notice for a run with no
// row writes nothing and answers no error, so the consumer acknowledges it.
func TestApplyCommandRefusedWithNoRunIsANoOp(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := uuid.MustParse(newTestUUIDv7String())

	res, err := g.Apply(context.Background(), newCommandRefusedEnvelope(t, runID, RefusedCommandCancel, "alice", time.Now()))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.CommandRefusedStored {
		t.Fatal("stored a notice with no run row")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
}

// TestApplyCommandRefusedRejectsAMalformedRunID asserts a run id that is not
// a UUIDv7, and a command name outside the set, answer
// ErrMalformedEnvelope, so the consumer drops the message and never
// retries it.
func TestApplyCommandRefusedRejectsAMalformedRunID(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	bad := newCommandRefusedEnvelope(t, uuid.MustParse(newTestUUIDv7String()), RefusedCommandCancel, "alice", time.Now())
	bad.RunID = "not-a-uuid"
	if _, err := g.Apply(context.Background(), bad); !errors.Is(err, ErrMalformedEnvelope) {
		t.Fatalf("bad run id: err = %v, want ErrMalformedEnvelope", err)
	}

	v4 := newCommandRefusedEnvelope(t, uuid.MustParse(newTestUUIDv7String()), RefusedCommandCancel, "alice", time.Now())
	v4.RunID = uuid.New().String()
	if _, err := g.Apply(context.Background(), v4); !errors.Is(err, ErrMalformedEnvelope) {
		t.Fatalf("v4 run id: err = %v, want ErrMalformedEnvelope", err)
	}

	unknown := newCommandRefusedEnvelope(t, uuid.MustParse(newTestUUIDv7String()), "replay", "alice", time.Now())
	if _, err := g.Apply(context.Background(), unknown); !errors.Is(err, ErrMalformedEnvelope) {
		t.Fatalf("unknown command: err = %v, want ErrMalformedEnvelope", err)
	}
}
