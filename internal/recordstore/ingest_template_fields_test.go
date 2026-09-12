package recordstore

import (
	"context"
	"testing"
)

// TestApplyRunStartedKeepsTemplateFields proves the four template-resolution
// fields the gateway adds to a run.started payload (design.md D14) survive
// into runs.payload with no typed column and no migration: the whole
// envelope round-trips under the "run.started" key.
func TestApplyRunStartedKeepsTemplateFields(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:template-started-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:template-started-1", KindRunStarted, map[string]any{
		"build_number":     buildNumber,
		"template_name":    "go-service",
		"template_version": "2.1.4",
		"marker_path":      ".varroa/pipeline.yaml",
		"marker_revision":  "abc123",
	})

	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var name, version, markerPath, revision string
	if err := db.QueryRow(`
		SELECT payload->'run.started'->'payload'->>'template_name',
		       payload->'run.started'->'payload'->>'template_version',
		       payload->'run.started'->'payload'->>'marker_path',
		       payload->'run.started'->'payload'->>'marker_revision'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&name, &version, &markerPath, &revision); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if name != "go-service" {
		t.Errorf("template_name = %q, want go-service", name)
	}
	if version != "2.1.4" {
		t.Errorf("template_version = %q, want 2.1.4", version)
	}
	if markerPath != ".varroa/pipeline.yaml" {
		t.Errorf("marker_path = %q, want .varroa/pipeline.yaml", markerPath)
	}
	if revision != "abc123" {
		t.Errorf("marker_revision = %q, want abc123", revision)
	}
}

// TestApplyRunCompletedKeepsTemplateFields is the run.completed sibling of
// TestApplyRunStartedKeepsTemplateFields.
func TestApplyRunCompletedKeepsTemplateFields(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:template-completed-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:template-completed-1", KindRunCompleted, map[string]any{
		"result":           "SUCCESS",
		"template_name":    "go-service",
		"template_version": "2.1.4",
		"marker_path":      ".varroa/pipeline.yaml",
		"marker_revision":  "abc123",
	})

	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var name, version string
	if err := db.QueryRow(`
		SELECT payload->'run.completed'->'payload'->>'template_name',
		       payload->'run.completed'->'payload'->>'template_version'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&name, &version); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if name != "go-service" {
		t.Errorf("template_name = %q, want go-service", name)
	}
	if version != "2.1.4" {
		t.Errorf("template_version = %q, want 2.1.4", version)
	}
}

// TestApplyRunStartedNoTemplateFields proves a run with no template writes
// no template keys at all, matching design.md D14's "all four are empty
// for a job with no template" for the stored payload.
func TestApplyRunStartedNoTemplateFields(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:template-none-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:template-none-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var hasKey bool
	if err := db.QueryRow(`
		SELECT payload->'run.started'->'payload' ? 'template_name'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&hasKey); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if hasKey {
		t.Error("template_name key present for a run with no template")
	}
}
