package blob

import "context"

// PartLister is the one Store method UnindexedLogParts needs.
type PartLister interface {
	Parts(ctx context.Context, key string, from int64) ([]Part, error)
}

// UnindexedLogParts answers the log parts of runID that a restarted
// gateway writes a recovered index entry for. from is one above the
// highest index entry. The answer is the run of parts at from, from+1,
// and up, and it stops at the first missing sequence or the first part
// of size 0.
//
// The gateway's restart recovery and the recorder's next log sequence
// both use this rule, and the two must stay equal.
//
// A part of size 0 must never move the acknowledged sequence. A moved
// build's blank pass writes empty parts above a hole. A recovered entry
// for such a part would mark the new writer's chunk at that sequence as
// stored and drop its bytes. Recovery therefore stops at an empty part,
// and the writer that still holds that sequence sends it again.
func UnindexedLogParts(ctx context.Context, store PartLister, runID string, from int64) ([]Part, error) {
	parts, err := store.Parts(ctx, LogKey(runID), from)
	if err != nil {
		return nil, err
	}
	n := 0
	for n < len(parts) && parts[n].Seq == from+int64(n) && parts[n].Size > 0 {
		n++
	}
	return parts[:n], nil
}
