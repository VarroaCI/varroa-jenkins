package v1alpha1

import "testing"

// TestCellStatusDeepCopyIsolatesPendingSignatures mutates a deep copy of a
// populated cell status and asserts the original keeps its requesters.
func TestCellStatusDeepCopyIsolatesPendingSignatures(t *testing.T) {
	orig := &CellStatus{
		PendingSignatures: []CellPendingSignature{{
			Signature:  "method java.lang.System exit int",
			Dangerous:  true,
			Requesters: []CellSignatureRequester{{Tenant: "team-a", JobPath: "team-a/build", BuildNumber: 7, RunID: "r1"}},
		}},
		PendingSignaturesTotal: 1,
	}
	cp := orig.DeepCopy()
	cp.PendingSignatures[0].Signature = "changed"
	cp.PendingSignatures[0].Requesters[0].JobPath = "changed"
	cp.PendingSignatures = append(cp.PendingSignatures, CellPendingSignature{Signature: "extra"})

	if orig.PendingSignatures[0].Signature != "method java.lang.System exit int" {
		t.Fatalf("original signature changed: %q", orig.PendingSignatures[0].Signature)
	}
	if orig.PendingSignatures[0].Requesters[0].JobPath != "team-a/build" {
		t.Fatalf("original requester changed: %q", orig.PendingSignatures[0].Requesters[0].JobPath)
	}
	if len(orig.PendingSignatures) != 1 {
		t.Fatalf("original list grew to %d", len(orig.PendingSignatures))
	}
}
