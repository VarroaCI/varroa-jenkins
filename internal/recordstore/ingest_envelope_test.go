package recordstore

import (
	"encoding/json"
	"testing"
	"time"
)

// TestIngestEnvelopeFieldNames asserts the envelope's JSON field names
// and that every one of the eight kinds decodes.
func TestIngestEnvelopeFieldNames(t *testing.T) {
	raw := `{
		"schema_version": 1,
		"kind": "run.started",
		"run_id": "run-1",
		"intent_key": "manual:abc",
		"cluster": "core",
		"namespace": "team-a",
		"cell": "cell-1",
		"cell_instance": "pod-a",
		"tenant": "team-a",
		"job_path": "folder/job",
		"occurred_at": "2026-09-09T12:00:00Z",
		"payload": {"build_number": 5}
	}`

	var env IngestEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.SchemaVersion != 1 || env.Kind != KindRunStarted || env.RunID != "run-1" ||
		env.IntentKey != "manual:abc" || env.Cluster != "core" || env.Namespace != "team-a" ||
		env.Cell != "cell-1" || env.CellInstance != "pod-a" || env.Tenant != "team-a" ||
		env.JobPath != "folder/job" {
		t.Fatalf("field mismatch: %+v", env)
	}
	wantTime := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if !env.OccurredAt.Equal(wantTime) {
		t.Fatalf("occurred_at = %v, want %v", env.OccurredAt, wantTime)
	}

	var payload RunStartedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.BuildNumber == nil || *payload.BuildNumber != 5 {
		t.Fatalf("build_number = %v, want 5", payload.BuildNumber)
	}
}

// TestIngestEnvelopeEveryKindDecodes decodes a minimal envelope for each
// of the eight kinds and asserts IsKnownKind agrees.
func TestIngestEnvelopeEveryKindDecodes(t *testing.T) {
	kinds := []string{
		KindRunStarted, KindRunStage, KindRunCompleted, KindRunTests,
		KindRunArtifacts, KindRunChanges, KindRunLog, KindRunHeartbeat,
	}
	if len(kinds) != 8 {
		t.Fatalf("expected exactly eight kinds, got %d", len(kinds))
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			raw, err := json.Marshal(IngestEnvelope{
				SchemaVersion: 1,
				Kind:          kind,
				RunID:         "run-1",
				IntentKey:     "manual:abc",
				Cluster:       "core",
				Namespace:     "team-a",
				Cell:          "cell-1",
				CellInstance:  "pod-a",
				Tenant:        "team-a",
				JobPath:       "folder/job",
				OccurredAt:    time.Now().UTC(),
				Payload:       json.RawMessage(`{}`),
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var env IngestEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Kind != kind {
				t.Fatalf("kind = %q, want %q", env.Kind, kind)
			}
			if !IsKnownKind(env.Kind) {
				t.Fatalf("IsKnownKind(%q) = false, want true", env.Kind)
			}
		})
	}
}

// TestIngestEnvelopeUnknownKind asserts IsKnownKind rejects a kind
// outside the eight-kind set.
func TestIngestEnvelopeUnknownKind(t *testing.T) {
	if IsKnownKind("run.frobnicated") {
		t.Fatal("expected run.frobnicated to be unknown")
	}
}
