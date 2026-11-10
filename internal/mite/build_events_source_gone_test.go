package mite

import (
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestCompletedPayload_SourceGone checks that source_gone is written only when
// the event sets it.
func TestCompletedPayload_SourceGone(t *testing.T) {
	gone := completedPayload(&mitev1.BuildEvent{Phase: phaseCompleted, RunId: "run-1", SourceGone: true})
	if v, ok := gone["source_gone"]; !ok || v != true {
		t.Fatalf("source_gone = %v, %v; want true", v, ok)
	}
	live := completedPayload(&mitev1.BuildEvent{Phase: phaseCompleted, RunId: "run-1"})
	if _, ok := live["source_gone"]; ok {
		t.Fatal("source_gone present for a live branch job")
	}
}
