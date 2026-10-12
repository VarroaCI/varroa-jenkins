package api

import (
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestScanNamesVisibleToKeepsOnlyReadableHeads asserts a scan result names
// only the inventory heads the caller may read. A name with no inventory head
// is dropped, because its job path cannot be rebuilt from the name.
func TestScanNamesVisibleToKeepsOnlyReadableHeads(t *testing.T) {
	heads := []recordstore.JobBranchHead{
		{Name: "main", JobPath: "team/app/main"},
		{Name: "secret", JobPath: "team/app/secret"},
	}
	readable := map[string]bool{"team/app/main": true}
	canRead := func(path string) bool { return readable[path] }

	got := scanNamesVisibleTo([]string{"main", "secret", "gone"}, heads, canRead)
	want := []string{"main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visible names = %v, want %v", got, want)
	}
	if empty := scanNamesVisibleTo([]string{"secret"}, heads, canRead); empty == nil || len(empty) != 0 {
		t.Fatalf("visible names = %v, want an empty non-nil list", empty)
	}

	// A name shared by a readable branch and an unreadable tag is hidden.
	shared := []recordstore.JobBranchHead{
		{Name: "release", JobPath: "team/app/release"},
		{Name: "release", JobPath: "team/app/tags/release"},
	}
	if got := scanNamesVisibleTo([]string{"release"}, shared, canRead); len(got) != 0 {
		t.Fatalf("shared name visible = %v, want hidden", got)
	}
}
