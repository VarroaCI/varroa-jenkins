package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// readRunInputsRow reads one run's stored pending set and its inputs
// sequence. It answers the set as its stored JSON text, and the empty
// string for a NULL column.
func readRunInputsRow(t *testing.T, db *sql.DB, runID uuid.UUID) (inputs, inputsSeq string) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT COALESCE(inputs::text, ''), COALESCE(payload->>'inputs_seq', '')
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&inputs, &inputsSeq); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	return inputs, inputsSeq
}

// storedRunInputs decodes one stored pending set. It fails the test when
// the text does not decode.
func storedRunInputs(t *testing.T, inputs string) []StoredRunInput {
	t.Helper()
	var entries []StoredRunInput
	if err := json.Unmarshal([]byte(inputs), &entries); err != nil {
		t.Fatalf("decode stored inputs %q: %v", inputs, err)
	}
	return entries
}

// runInputSet builds a pending set from one id per entry.
func runInputSet(ids ...string) []StoredRunInput {
	out := make([]StoredRunInput, 0, len(ids))
	for _, id := range ids {
		out = append(out, StoredRunInput{ID: id, Message: "Deploy?"})
	}
	return out
}

// inputSeqPtr answers a pointer to seq, which the payload carries.
func inputSeqPtr(seq int64) *int64 { return &seq }

// TestApplyRunInputLaterSetReplacesTheStoredSet asserts the stored set is
// the payload's set, so a settled input leaves the record.
func TestApplyRunInputLaterSetReplacesTheStoredSet(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-replace")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:input-replace", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-1", "input-2")})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.input: %v", err)
	}

	second := newTestEnvelope(t, runID, "manual:input-replace", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(2), Inputs: runInputSet("input-1")})
	res, err := g.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply second run.input: %v", err)
	}
	if res.RefusedInput {
		t.Fatal("the write was refused, want applied")
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	entries := storedRunInputs(t, inputs)
	if len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want the payload's set alone", entries)
	}
	if inputsSeq != "2" {
		t.Fatalf("inputs_seq = %q, want 2", inputsSeq)
	}
}

// TestApplyRunInputEqualSeqApplies asserts an equal seq applies, so a
// redelivered envelope stays safe.
func TestApplyRunInputEqualSeqApplies(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-equal")
	g := NewIngester(pool, nil, nil)

	payload := RunInputPayload{Seq: inputSeqPtr(3), Inputs: runInputSet("input-1")}
	for i := 0; i < 2; i++ {
		env := newTestEnvelope(t, runID, "manual:input-equal", KindRunInput, payload)
		res, err := g.Apply(context.Background(), env)
		if err != nil {
			t.Fatalf("apply run.input %d: %v", i, err)
		}
		if res.RefusedInput {
			t.Fatalf("apply run.input %d was refused, want applied", i)
		}
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want one input-1 entry", entries)
	}
	if inputsSeq != "3" {
		t.Fatalf("inputs_seq = %q, want 3", inputsSeq)
	}
}

// TestApplyRunInputLowerSeqChangesNothing asserts the guard refuses a
// write below the stored watermark and leaves the stored set in place.
func TestApplyRunInputLowerSeqChangesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-lower")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:input-lower", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(2), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.input: %v", err)
	}

	lower := newTestEnvelope(t, runID, "manual:input-lower", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-9")})
	res, err := g.Apply(context.Background(), lower)
	if err != nil {
		t.Fatalf("apply lower run.input: %v", err)
	}
	if !res.RefusedInput {
		t.Fatal("the lower write was applied, want refused")
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want the newer set", entries)
	}
	if inputsSeq != "2" {
		t.Fatalf("inputs_seq = %q, want 2", inputsSeq)
	}
}

// TestApplyRunInputEmptySetWritesNull asserts an empty set clears the
// column and raises the watermark.
func TestApplyRunInputEmptySetWritesNull(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-empty")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:input-empty", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.input: %v", err)
	}

	empty := newTestEnvelope(t, runID, "manual:input-empty", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(2)})
	res, err := g.Apply(context.Background(), empty)
	if err != nil {
		t.Fatalf("apply empty run.input: %v", err)
	}
	if res.RefusedInput {
		t.Fatal("the empty write was refused, want applied")
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if inputs != "" {
		t.Fatalf("stored inputs = %q, want NULL", inputs)
	}
	if inputsSeq != "2" {
		t.Fatalf("inputs_seq = %q, want 2", inputsSeq)
	}
}

// TestApplyRunInputNullPayloadTakesTheWrite asserts a stored row whose
// payload is still NULL takes the write, because the accepted-key strip
// reads a coalesced payload.
func TestApplyRunInputNullPayloadTakesTheWrite(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-null-payload")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:input-null-payload", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.input: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.runs SET payload = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("clear the payload: %v", err)
	}

	second := newTestEnvelope(t, runID, "manual:input-null-payload", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(2), Inputs: runInputSet("input-2")})
	res, err := g.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply second run.input: %v", err)
	}
	if res.RefusedInput {
		t.Fatal("the write over a NULL payload was refused, want applied")
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-2" {
		t.Fatalf("stored inputs = %+v, want the payload's set", entries)
	}
	if inputsSeq != "2" {
		t.Fatalf("inputs_seq = %q, want 2", inputsSeq)
	}
}

// TestApplyRunInputBeforeRunStartedInsertsQueuedRow asserts a run.input
// envelope that arrives first writes the placeholder row.
func TestApplyRunInputBeforeRunStartedInsertsQueuedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-first")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:input-first", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(0), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.input: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != "QUEUED" {
		t.Fatalf("status = %q, want QUEUED", status)
	}
	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want one input-1 entry", entries)
	}
	if inputsSeq != "0" {
		t.Fatalf("inputs_seq = %q, want 0", inputsSeq)
	}
}

// TestApplyRunInputCompletedRowRefusesTheWrite asserts a terminal run
// takes no pending set.
func TestApplyRunInputCompletedRowRefusesTheWrite(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:input-completed")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:input-completed", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(1), Inputs: runInputSet("input-1")})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.input: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("complete the run: %v", err)
	}

	late := newTestEnvelope(t, runID, "manual:input-completed", KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(9), Inputs: runInputSet("input-9")})
	res, err := g.Apply(context.Background(), late)
	if err != nil {
		t.Fatalf("apply late run.input: %v", err)
	}
	if !res.RefusedInput {
		t.Fatal("the terminal row took the write, want refused")
	}

	inputs, inputsSeq := readRunInputsRow(t, db, runID)
	if entries := storedRunInputs(t, inputs); len(entries) != 1 || entries[0].ID != "input-1" {
		t.Fatalf("stored inputs = %+v, want the stored set", entries)
	}
	if inputsSeq != "1" {
		t.Fatalf("inputs_seq = %q, want 1", inputsSeq)
	}
}
