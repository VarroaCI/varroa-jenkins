package rehydrate

import (
	"testing"

	"github.com/google/uuid"
)

func sampleBudgetResult() BudgetResult {
	return BudgetResult{
		Files: []CandidateFile{
			{Path: BuildXMLPath("team/api", 2), Content: []byte("<flow-build/>")},
			{Path: NextBuildNumberPath("team/api"), Content: []byte("3\n")},
			{Path: ChangelogFilePath("team/api", 2), Content: []byte("<varroa-changelog/>")},
		},
		JobPaths:    []string{"team/api"},
		RecordCount: 1,
	}
}

func TestBuildBundle_StableContentHash(t *testing.T) {
	b1, err := BuildBundle(sampleBudgetResult())
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	b2, err := BuildBundle(sampleBudgetResult())
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if b1.ContentHash == "" {
		t.Fatal("ContentHash is empty")
	}
	if b1.ContentHash != b2.ContentHash {
		t.Errorf("ContentHash is not stable for a fixed input: %q != %q", b1.ContentHash, b2.ContentHash)
	}

	// A different input produces a different hash.
	changed := sampleBudgetResult()
	changed.Files[0].Content = []byte("<flow-build><different/></flow-build>")
	b3, err := BuildBundle(changed)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if b3.ContentHash == b1.ContentHash {
		t.Error("ContentHash did not change for different content")
	}
}

func TestBuildBundle_BundleIDIsValidUUIDv7(t *testing.T) {
	b, err := BuildBundle(sampleBudgetResult())
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	id, err := uuid.Parse(b.BundleId)
	if err != nil {
		t.Fatalf("BundleId %q does not parse as a UUID: %v", b.BundleId, err)
	}
	if id.Version() != 7 {
		t.Errorf("BundleId version = %d, want 7", id.Version())
	}
}

func TestBuildBundle_FillsFieldsFromResult(t *testing.T) {
	result := sampleBudgetResult()
	result.Truncated = true
	b, err := BuildBundle(result)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if !b.Truncated {
		t.Error("Truncated not carried through")
	}
	if b.RecordCount != 1 {
		t.Errorf("RecordCount = %d, want 1", b.RecordCount)
	}
	if b.JobCount != 1 {
		t.Errorf("JobCount = %d, want 1", b.JobCount)
	}
	if len(b.ReloadItems) != 1 || b.ReloadItems[0] != "team/api" {
		t.Errorf("ReloadItems = %v, want [team/api]", b.ReloadItems)
	}
	if len(b.Files) != len(result.Files) {
		t.Fatalf("got %d files, want %d", len(b.Files), len(result.Files))
	}
	for _, f := range b.Files {
		if len(f.GzipContent) == 0 {
			t.Errorf("file %s has no gzip content", f.Path)
		}
		if f.RawSize == 0 {
			t.Errorf("file %s has raw size 0", f.Path)
		}
	}
}
