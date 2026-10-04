package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// readRunSourceRow reads one run's stored source object and its source
// sequence. It returns the source object as its stored JSON text.
func readRunSourceRow(t *testing.T, db *sql.DB, runID uuid.UUID) (source, sourceSeq string) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT COALESCE(source::text, ''), COALESCE(payload->>'source_seq', '')
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&source, &sourceSeq); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	return source, sourceSeq
}

// storedSourceField decodes one key of a stored source object into want.
// It fails the test when the key is absent or does not decode.
func storedSourceField(t *testing.T, source, key string, want any) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &obj); err != nil {
		t.Fatalf("decode stored source %q: %v", source, err)
	}
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("stored source %q holds no key %q", source, key)
	}
	if err := json.Unmarshal(raw, want); err != nil {
		t.Fatalf("decode stored source key %q: %v", key, err)
	}
}

// readRunColumns reads the branch and the scm_revision of one run.
func readRunColumns(t *testing.T, db *sql.DB, runID uuid.UUID) (branch string, revision *string) {
	t.Helper()
	if err := db.QueryRow(`SELECT branch, scm_revision FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&branch, &revision); err != nil {
		t.Fatalf("read branch and scm_revision: %v", err)
	}
	return branch, revision
}

// TestApplyRunSourceSecondEventKeepsDefinitionPath asserts a second
// run.source event adds its own keys and drops none the first wrote.
func TestApplyRunSourceSecondEventKeepsDefinitionPath(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-second")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:source-second", KindRunSource, RunSourcePayload{
		Seq:            0,
		Branch:         strPtr("main"),
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.source: %v", err)
	}

	second := newTestEnvelope(t, runID, "manual:source-second", KindRunSource, RunSourcePayload{
		Seq:          1,
		PodTemplates: []SourcePodTemplate{{Name: "k8s", Image: "agent:1"}},
	})
	res, err := g.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply second run.source: %v", err)
	}
	if res.RefusedSource {
		t.Fatal("the second run.source write was refused, want applied")
	}

	source, sourceSeq := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
	var pods []SourcePodTemplate
	storedSourceField(t, source, "pod_templates", &pods)
	if len(pods) != 1 || pods[0].Name != "k8s" || pods[0].Image != "agent:1" {
		t.Fatalf("pod_templates = %+v, want one k8s entry", pods)
	}
	if sourceSeq != "1" {
		t.Fatalf("source_seq = %q, want 1", sourceSeq)
	}
	if branch, _ := readRunColumns(t, db, runID); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
}

// TestApplyRunSourceAllocationEventAddsPodTemplates asserts the allocation
// event adds its pod templates and keeps the keys the second event wrote.
func TestApplyRunSourceAllocationEventAddsPodTemplates(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-allocation")
	g := NewIngester(pool, nil, nil)

	// The second emission: no pod template yet.
	first := newTestEnvelope(t, runID, "manual:source-allocation", KindRunSource, RunSourcePayload{
		Seq:            2,
		Branch:         strPtr("main"),
		DefinitionPath: "Jenkinsfile",
		Libraries:      []SourceLibrary{{Name: "shared-lib", Version: "1.2.0"}},
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply the second emission: %v", err)
	}

	// The allocation emission: one pod template, and no definition path or libraries.
	second := newTestEnvelope(t, runID, "manual:source-allocation", KindRunSource, RunSourcePayload{
		Seq:          3,
		PodTemplates: []SourcePodTemplate{{Name: "probe-agent", Label: "probe-agent", Image: "agent:1"}},
	})
	res, err := g.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply the allocation emission: %v", err)
	}
	if res.RefusedSource {
		t.Fatal("the allocation emission was refused, want applied")
	}

	// The row keeps the earlier keys and gains the pod template.
	source, sourceSeq := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
	var libraries []SourceLibrary
	storedSourceField(t, source, "libraries", &libraries)
	if len(libraries) != 1 || libraries[0].Name != "shared-lib" {
		t.Fatalf("libraries = %+v, want one shared-lib entry", libraries)
	}
	var pods []SourcePodTemplate
	storedSourceField(t, source, "pod_templates", &pods)
	if len(pods) != 1 || pods[0].Name != "probe-agent" || pods[0].Label != "probe-agent" {
		t.Fatalf("pod_templates = %+v, want one probe-agent entry", pods)
	}
	if sourceSeq != "3" {
		t.Fatalf("source_seq = %q, want 3", sourceSeq)
	}
	if branch, _ := readRunColumns(t, db, runID); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}

	// A late second emission changes nothing.
	late := newTestEnvelope(t, runID, "manual:source-allocation", KindRunSource, RunSourcePayload{
		Seq:            2,
		DefinitionPath: "Jenkinsfile",
		Libraries:      []SourceLibrary{{Name: "other-lib", Version: "9.9.9"}},
	})
	lateRes, err := g.Apply(context.Background(), late)
	if err != nil {
		t.Fatalf("apply the late second emission: %v", err)
	}
	if !lateRes.RefusedSource {
		t.Fatal("the late second emission applied, want refused")
	}
	lateSource, lateSeq := readRunSourceRow(t, db, runID)
	var lateLibraries []SourceLibrary
	storedSourceField(t, lateSource, "libraries", &lateLibraries)
	if len(lateLibraries) != 1 || lateLibraries[0].Name != "shared-lib" {
		t.Fatalf("libraries after the late write = %+v, want one shared-lib entry", lateLibraries)
	}
	var latePods []SourcePodTemplate
	storedSourceField(t, lateSource, "pod_templates", &latePods)
	if len(latePods) != 1 || latePods[0].Name != "probe-agent" {
		t.Fatalf("pod_templates after the late write = %+v, want one probe-agent entry", latePods)
	}
	if lateSeq != "3" {
		t.Fatalf("source_seq after the late write = %q, want 3", lateSeq)
	}
}

// TestApplyRunSourceLowerSeqChangesNothing asserts a lower seq applies
// the accepted-key strip alone, and the stored values stay.
func TestApplyRunSourceLowerSeqChangesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-lower")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:source-lower", KindRunSource, RunSourcePayload{
		Seq:            5,
		Branch:         strPtr("main"),
		SCMRevision:    strPtr("rev-a"),
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.source: %v", err)
	}

	lower := newTestEnvelope(t, runID, "manual:source-lower", KindRunSource, RunSourcePayload{
		Seq:            4,
		Branch:         strPtr("other"),
		SCMRevision:    strPtr("rev-b"),
		DefinitionPath: "Other.groovy",
	})
	res, err := g.Apply(context.Background(), lower)
	if err != nil {
		t.Fatalf("apply lower run.source: %v", err)
	}
	if !res.RefusedSource {
		t.Fatal("the lower run.source write was applied, want refused")
	}

	source, sourceSeq := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
	if sourceSeq != "5" {
		t.Fatalf("source_seq = %q, want 5", sourceSeq)
	}
	var storedPayloadKey string
	if err := db.QueryRow(`SELECT payload->'run.source'->>'definition_path' FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&storedPayloadKey); err != nil {
		t.Fatalf("read stored payload key: %v", err)
	}
	if storedPayloadKey != "Jenkinsfile" {
		t.Fatalf("payload run.source definition_path = %q, want Jenkinsfile", storedPayloadKey)
	}
	branch, revision := readRunColumns(t, db, runID)
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if revision == nil || *revision != "rev-a" {
		t.Fatalf("scm_revision = %v, want rev-a", revision)
	}
}

// TestApplyRunSourceEqualSeqIsIdempotent asserts a redelivered envelope
// writes the same values again and leaves the row readable.
func TestApplyRunSourceEqualSeqIsIdempotent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-equal")
	g := NewIngester(pool, nil, nil)

	payload := RunSourcePayload{Seq: 3, Branch: strPtr("main"), DefinitionPath: "Jenkinsfile"}
	for i := 0; i < 2; i++ {
		env := newTestEnvelope(t, runID, "manual:source-equal", KindRunSource, payload)
		res, err := g.Apply(context.Background(), env)
		if err != nil {
			t.Fatalf("apply run.source %d: %v", i, err)
		}
		if res.RefusedSource {
			t.Fatalf("apply run.source %d was refused, want applied", i)
		}
	}

	source, sourceSeq := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
	if sourceSeq != "3" {
		t.Fatalf("source_seq = %q, want 3", sourceSeq)
	}
}

// TestApplyRunSourceZeroSeqApplies asserts a zero seq merges over a
// stored zero, because a resumed build's first event carries it.
func TestApplyRunSourceZeroSeqApplies(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-zero")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:source-zero", KindRunSource, RunSourcePayload{
		Seq:            0,
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.source: %v", err)
	}

	second := newTestEnvelope(t, runID, "manual:source-zero", KindRunSource, RunSourcePayload{
		Seq:       0,
		Libraries: []SourceLibrary{{Name: "shared", Version: "1.0", Revision: "abc"}},
	})
	res, err := g.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply second run.source: %v", err)
	}
	if res.RefusedSource {
		t.Fatal("the zero-seq write was refused, want applied")
	}

	source, sourceSeq := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
	var libraries []SourceLibrary
	storedSourceField(t, source, "libraries", &libraries)
	if len(libraries) != 1 || libraries[0].Name != "shared" {
		t.Fatalf("libraries = %+v, want one shared entry", libraries)
	}
	if sourceSeq != "0" {
		t.Fatalf("source_seq = %q, want 0", sourceSeq)
	}
}

// TestApplyRunSourceEmptyRevisionClearsNothing asserts an event that
// carries no branch and no revision leaves both stored values alone.
func TestApplyRunSourceEmptyRevisionClearsNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-empty")
	g := NewIngester(pool, nil, nil)

	first := newTestEnvelope(t, runID, "manual:source-empty", KindRunSource, RunSourcePayload{
		Seq:         1,
		Branch:      strPtr("main"),
		SCMRevision: strPtr("rev-a"),
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.source: %v", err)
	}

	second := newTestEnvelope(t, runID, "manual:source-empty", KindRunSource, RunSourcePayload{
		Seq:            2,
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), second); err != nil {
		t.Fatalf("apply second run.source: %v", err)
	}

	branch, revision := readRunColumns(t, db, runID)
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if revision == nil || *revision != "rev-a" {
		t.Fatalf("scm_revision = %v, want rev-a", revision)
	}
	_, sourceSeq := readRunSourceRow(t, db, runID)
	if sourceSeq != "2" {
		t.Fatalf("source_seq = %q, want 2", sourceSeq)
	}
}

// TestApplyRunSourceBeforeRunStartedInsertsQueuedRow asserts E1 reaching
// the recorder before run.started writes the placeholder row.
func TestApplyRunSourceBeforeRunStartedInsertsQueuedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:source-first")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:source-first", KindRunSource, RunSourcePayload{
		Seq:            0,
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.source: %v", err)
	}

	var status string
	var buildNumber *int64
	if err := db.QueryRow(`SELECT status, build_number FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &buildNumber); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != "QUEUED" {
		t.Fatalf("status = %q, want QUEUED", status)
	}
	if buildNumber != nil {
		t.Fatalf("build_number = %d, want nil", *buildNumber)
	}
	source, _ := readRunSourceRow(t, db, runID)
	var definitionPath string
	storedSourceField(t, source, "definition_path", &definitionPath)
	if definitionPath != "Jenkinsfile" {
		t.Fatalf("definition_path = %q, want Jenkinsfile", definitionPath)
	}
}
