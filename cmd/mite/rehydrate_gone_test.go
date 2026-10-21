package main

import (
	"reflect"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestOrphanDeferredJobsGoneFolder(t *testing.T) {
	const failure = "FAILURE"
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	deferred := []string{"mb/main"}
	tests := []struct {
		name       string
		gone       map[string]bool
		set        *mitev1.JobBranchSet
		wantKeep   []string
		wantOrphan []string
	}{
		{
			name:       "gone folder with a failed scan orphans the job",
			gone:       map[string]bool{"mb": true},
			set:        branchSet(branchRow("mb", failure)),
			wantOrphan: []string{"mb/main"},
		},
		{
			name:     "gone folder whose scan succeeds again keeps the job",
			gone:     map[string]bool{"mb": true},
			set:      branchSet(branchRow("mb", branchIndexSucceeded, "mb/main")),
			wantKeep: []string{"mb/main"},
		},
		{
			name:     "folder not in the gone set keeps the job",
			gone:     map[string]bool{"other": true},
			set:      branchSet(branchRow("mb", failure)),
			wantKeep: []string{"mb/main"},
		},
		{
			name:     "nil inventory keeps the job",
			gone:     map[string]bool{"mb": true},
			set:      nil,
			wantKeep: []string{"mb/main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keep, orphans := orphanDeferredJobs(home, deferred, tt.gone, func() *mitev1.JobBranchSet { return tt.set })
			if !reflect.DeepEqual(keep, tt.wantKeep) || !reflect.DeepEqual(orphans, tt.wantOrphan) {
				t.Errorf("keep=%v orphans=%v, want keep=%v orphans=%v", keep, orphans, tt.wantKeep, tt.wantOrphan)
			}
		})
	}
}

func TestFailedBranchScansCarriesRepoURL(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	row := branchRow("mb", "FAILURE")
	row.SourceUrl = "https://git.example.test/owner/repo.git"
	got := failedBranchScans(home, []string{"mb/main"}, branchSet(row))
	want := []*mitev1.RehydrationFailedScan{{
		Folder: "mb", IndexResult: "FAILURE", DeferredJobs: 1, RepoUrl: "https://git.example.test/owner/repo.git",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("failedBranchScans = %v, want %v", got, want)
	}
}
