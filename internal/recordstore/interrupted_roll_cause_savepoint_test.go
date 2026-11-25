package recordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestReadCellRollCauseLogsATimeoutAndKeepsTheTransaction asserts the
// savepoint rule of the roll cause read. A read that a held lock stops past
// its timeout answers no detail and no error, and it logs one Warn line with
// the run id and the error. The caller's transaction stays usable, and its
// statement timeout is the one it had before the read. A second read after
// the lock goes answers the cause and logs nothing.
func TestReadCellRollCauseLogsATimeoutAndKeepsTheTransaction(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	runID := startRunOnCell(t, pool, "manual:"+newTestUUIDv7String(), "savepoint-a")
	c := fetchRunningCandidate(t, pool, runID)
	mark := livenessBaseline(c).Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	ingestActivityEvent(t, pool, cellRolledRow("savepoint-a", c.cellInstance, mark.Add(-time.Second), "MiteUnreachable"))

	locker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	t.Cleanup(func() { _ = locker.Rollback() })
	if _, err := locker.Exec(`LOCK TABLE varroa.activity IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock varroa.activity: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before string
	if err := tx.QueryRow(ctx, `SHOW statement_timeout`).Scan(&before); err != nil {
		t.Fatalf("show statement_timeout: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cause, err := readCellRollCause(ctx, tx, c, mark, 100*time.Millisecond, logger)
	if err != nil || cause != "" {
		t.Fatalf("readCellRollCause under a held lock = %q, %v, want no detail and no error", cause, err)
	}

	var after string
	if err := tx.QueryRow(ctx, `SHOW statement_timeout`).Scan(&after); err != nil {
		t.Fatalf("the transaction is not usable after the failed read: %v", err)
	}
	if after != before {
		t.Fatalf("statement_timeout = %q after the read, want %q", after, before)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d (%q), want 1", len(lines), logs.String())
	}
	var record struct {
		Level string `json:"level"`
		RunID string `json:"run_id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if record.Level != "WARN" || record.RunID != runID.String() || !strings.Contains(record.Error, "statement timeout") {
		t.Fatalf("log line = %+v, want WARN with the run id and a statement timeout error", record)
	}

	if err := locker.Rollback(); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	cause, err = readCellRollCause(ctx, tx, c, mark, 100*time.Millisecond, logger)
	if err != nil || cause != "MiteUnreachable" {
		t.Fatalf("readCellRollCause after the lock = %q, %v, want MiteUnreachable", cause, err)
	}
	if got := strings.Count(strings.TrimSpace(logs.String()), "\n") + 1; got != 1 {
		t.Fatalf("log lines = %d after a good read, want 1", got)
	}
}
