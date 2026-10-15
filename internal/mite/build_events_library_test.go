package mite

import (
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestLibraryEntriesCarryTheRepositoryFields(t *testing.T) {
	in := []*mitev1.BuildLibrary{{
		Name:            "shared-lib",
		Version:         "1.2.0",
		Revision:        "abc123",
		RepositoryUrl:   "https://github.com/example/shared-lib",
		RepositoryForge: "github",
	}}
	got := libraryEntries(in)
	if len(got) != 1 {
		t.Fatalf("libraryEntries answered %d entries, want 1", len(got))
	}
	if got[0].RepositoryURL != "https://github.com/example/shared-lib" {
		t.Errorf("RepositoryURL = %q", got[0].RepositoryURL)
	}
	if got[0].RepositoryForge != "github" {
		t.Errorf("RepositoryForge = %q", got[0].RepositoryForge)
	}
}
