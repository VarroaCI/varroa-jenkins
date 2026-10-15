package api

import (
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestScanHeadNamesVisibleToFiltersByStoredPath asserts a scan list names
// only the heads whose stored job path the caller may read. A head the scan
// deleted keeps its path, so it is still tested.
func TestScanHeadNamesVisibleToFiltersByStoredPath(t *testing.T) {
	readable := map[string]bool{"team/app/main": true, "team/app/gone": true}
	canRead := func(path string) bool { return readable[path] }

	heads := []recordstore.BranchScanHead{
		{Name: "main", JobPath: "team/app/main"},
		{Name: "secret", JobPath: "team/app/secret"},
		{Name: "gone", JobPath: "team/app/gone"},
	}
	got := scanHeadNamesVisibleTo(heads, canRead)
	want := []string{"main", "gone"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visible names = %v, want %v", got, want)
	}

	if empty := scanHeadNamesVisibleTo([]recordstore.BranchScanHead{{Name: "secret", JobPath: "team/app/secret"}}, canRead); empty == nil || len(empty) != 0 {
		t.Fatalf("visible names = %v, want an empty non-nil list", empty)
	}
	if none := scanHeadNamesVisibleTo(nil, canRead); none == nil || len(none) != 0 {
		t.Fatalf("visible names of no heads = %v, want an empty non-nil list", none)
	}
}

// TestScanHeadNamesVisibleToTestsEachHeadOnItsOwnPath asserts a name shared
// by a readable branch and an unreadable tag shows the branch only.
func TestScanHeadNamesVisibleToTestsEachHeadOnItsOwnPath(t *testing.T) {
	readable := map[string]bool{"team/app/release": true}
	canRead := func(path string) bool { return readable[path] }

	heads := []recordstore.BranchScanHead{
		{Name: "release", JobPath: "team/app/release"},
		{Name: "release", JobPath: "team/app/tags/release"},
	}
	got := scanHeadNamesVisibleTo(heads, canRead)
	if want := []string{"release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visible names = %v, want %v", got, want)
	}
}
