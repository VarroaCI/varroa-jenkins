package controller

import (
	"reflect"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// The gone folders travel only with a rehydration bundle.
func TestAttachRepoGoneFoldersOnlyWithRehydration(t *testing.T) {
	const cell = "ns/cell"
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	prober := &countingProber{goneURLs: map[string]bool{"https://git.example.org/o/gone": true}}
	r := inlineProbeReconciler(prober)
	r.noteRepoScans(cell, successResult(failedScan("t/a", "https://git.example.org/o/gone")),
		map[string]bool{"t": true}, []string{"t"}, now)

	without := &mitev1.DesiredStateCommand{}
	r.attachRepoGoneFolders(without, cell)
	if without.RehydrationGoneFolders != nil {
		t.Fatalf("gone folders without a bundle = %v, want nil", without.RehydrationGoneFolders)
	}

	with := &mitev1.DesiredStateCommand{Rehydration: &mitev1.RehydrationBundle{}}
	r.attachRepoGoneFolders(with, cell)
	if want := []string{"t/a"}; !reflect.DeepEqual(with.RehydrationGoneFolders, want) {
		t.Fatalf("gone folders with a bundle = %v, want %v", with.RehydrationGoneFolders, want)
	}
}
