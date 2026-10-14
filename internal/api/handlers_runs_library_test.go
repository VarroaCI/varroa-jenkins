package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func TestBuildRunSourceMapsTheLibraryRepositoryFields(t *testing.T) {
	stored := recordstore.StoredRunSource{
		Libraries: []recordstore.SourceLibrary{{
			Name:            "shared-lib",
			Version:         "1.2.0",
			Revision:        "abc123",
			RepositoryURL:   "https://github.com/example/shared-lib",
			RepositoryForge: "github",
		}},
	}
	got := buildRunSource(stored)
	if got == nil || len(got.Libraries) != 1 {
		t.Fatalf("buildRunSource answered %+v, want one library", got)
	}
	lib := got.Libraries[0]
	if lib.RepositoryURL != "https://github.com/example/shared-lib" {
		t.Errorf("RepositoryURL = %q", lib.RepositoryURL)
	}
	if lib.RepositoryForge != "github" {
		t.Errorf("RepositoryForge = %q", lib.RepositoryForge)
	}
}
