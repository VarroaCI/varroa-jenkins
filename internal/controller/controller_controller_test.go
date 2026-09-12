package controller

import (
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestComputeDesiredStateHash_IgnoresRehydration asserts that
// computeDesiredStateHash ignores DesiredStateCommand field 17
// (rehydration): two commands that differ only in that field hash the
// same. The rehydration bundle changes after every build, so hashing it
// would force a desired-state push after every build.
func TestComputeDesiredStateHash_IgnoresRehydration(t *testing.T) {
	base := &mitev1.DesiredStateCommand{
		JcascYaml: "jcasc: 1",
		RbacYaml:  "rbac: 1",
		ItemsYaml: "items: 1",
	}
	withRehydration := &mitev1.DesiredStateCommand{
		JcascYaml: "jcasc: 1",
		RbacYaml:  "rbac: 1",
		ItemsYaml: "items: 1",
		Rehydration: &mitev1.RehydrationBundle{
			BundleId:    "bundle-1",
			ContentHash: "abc123",
			RecordCount: 5,
		},
	}

	baseHash := computeDesiredStateHash(base)
	rehydrationHash := computeDesiredStateHash(withRehydration)

	if baseHash != rehydrationHash {
		t.Errorf("computeDesiredStateHash changed when only field 17 (rehydration) differs: %q != %q", baseHash, rehydrationHash)
	}
}
