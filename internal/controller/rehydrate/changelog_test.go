package rehydrate

import (
	"testing"
	"time"
)

// TestRenderChangelog_DocumentShape covers task 5.2a's first
// acceptance: the rendered document's shape.
func TestRenderChangelog_DocumentShape(t *testing.T) {
	changes := []Change{
		{CommitID: "abc123", AuthorID: "ada", Message: "fix the queue lock", Timestamp: time.UnixMilli(1_700_000_000_000)},
		{CommitID: "def456", AuthorID: "grace", Message: "add a test", Timestamp: time.UnixMilli(1_700_000_100_000)},
	}

	out, err := RenderChangelog(changes)
	if err != nil {
		t.Fatalf("RenderChangelog: %v", err)
	}
	if out == nil {
		t.Fatal("expected a document for a non-empty changeset")
	}

	var doc changelogDocXML
	mustUnmarshal(t, out, &doc)

	if len(doc.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(doc.Entries))
	}
	first := doc.Entries[0]
	if first.CommitID != "abc123" {
		t.Errorf("commitId = %q, want abc123", first.CommitID)
	}
	if first.Author != "ada" {
		t.Errorf("author = %q, want ada", first.Author)
	}
	if first.Timestamp != 1_700_000_000_000 {
		t.Errorf("timestamp = %d, want 1700000000000", first.Timestamp)
	}
	if first.Msg != "fix the queue lock" {
		t.Errorf("msg = %q, want %q", first.Msg, "fix the queue lock")
	}
}

// TestRenderChangelog_CapsAtOneHundredEntries covers the entry cap.
func TestRenderChangelog_CapsAtOneHundredEntries(t *testing.T) {
	changes := make([]Change, 150)
	for i := range changes {
		changes[i] = Change{CommitID: "c", AuthorID: "a", Message: "m", Timestamp: time.Now()}
	}
	out, err := RenderChangelog(changes)
	if err != nil {
		t.Fatalf("RenderChangelog: %v", err)
	}
	var doc changelogDocXML
	mustUnmarshal(t, out, &doc)
	if len(doc.Entries) != maxChangelogEntries {
		t.Errorf("got %d entries, want %d", len(doc.Entries), maxChangelogEntries)
	}
}

// TestRenderChangelog_NoChangeWritesNoFile covers task 5.2a's second
// acceptance: a run with no change writes neither a changelog file
// nor a <checkouts> element.
func TestRenderChangelog_NoChangeWritesNoFile(t *testing.T) {
	out, err := RenderChangelog(nil)
	if err != nil {
		t.Fatalf("RenderChangelog: %v", err)
	}
	if out != nil {
		t.Errorf("expected no file for an empty changeset, got %d bytes", len(out))
	}

	rec := sampleRecord()
	rec.Changes = nil
	buildOut, err := RenderBuildXML(rec, Options{}, "")
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}
	var doc flowBuildXML
	mustUnmarshal(t, buildOut, &doc)
	if doc.Checkouts != nil {
		t.Error("build.xml carries a <checkouts> element for a run with no recorded change")
	}
}

// TestChangelogAbsolutePath_UsesContainerJenkinsHome covers task
// 5.2a's third acceptance: the path uses the container's JENKINS_HOME,
// not a hardcoded value.
func TestChangelogAbsolutePath_UsesContainerJenkinsHome(t *testing.T) {
	got := ChangelogAbsolutePath("/custom/jenkins/home", "team/api/main", 7)
	want := "/custom/jenkins/home/jobs/team/jobs/api/jobs/main/builds/7/changelog0.xml"
	if got != want {
		t.Errorf("ChangelogAbsolutePath = %q, want %q", got, want)
	}

	// A different JENKINS_HOME produces a different path: the value is
	// not hardcoded anywhere in the renderer.
	other := ChangelogAbsolutePath("/var/jenkins_home", "team/api/main", 7)
	if other == got {
		t.Error("changed JENKINS_HOME did not change the rendered path")
	}
}
