package blob

import (
	"context"
	"errors"
)

// LogPartReader is the two Store methods UnindexedLogParts needs.
type LogPartReader interface {
	Parts(ctx context.Context, key string, from int64) ([]Part, error)
	PartWriter(ctx context.Context, key string, seq int64) (string, error)
}

// UnindexedLogParts answers the log parts of runID that a restarted
// gateway writes a recovered index entry for. from is one above the
// highest index entry. writer is the writer of the highest index entry
// that names one, or "" when no entry names one. The answer is the run
// of parts at from, from+1, and up. It stops at the first missing
// sequence, at the first part of size 0, and at the first part whose
// writer differs from the baseline. The baseline is writer, or the
// writer of the part at from when writer is "". It also returns the
// baseline, which every answered part carries.
//
// The gateway's restart recovery, the recorder's next log sequence, and
// both manifest readers use this rule, and they must stay equal.
//
// A part of size 0 must never move the acknowledged sequence. A moved
// build's blank pass writes empty parts above a hole. A recovered entry
// for such a part would mark the new writer's chunk at that sequence as
// stored and drop its bytes. Recovery therefore stops at an empty part,
// and the writer that still holds that sequence sends it again.
//
// A part of another writer must never move the acknowledged sequence
// either. A lost cell's write can land after a mesh takeover moved its
// build, above the new writer's highest index entry. A recovered entry
// for it would drop the new writer's chunk at that sequence. A part that
// the current writer stored is safe to skip: its writer still holds the
// chunk and sends it again.
func UnindexedLogParts(ctx context.Context, store LogPartReader, runID string, from int64, writer string) ([]Part, string, error) {
	key := LogKey(runID)
	parts, err := store.Parts(ctx, key, from)
	if err != nil {
		return nil, "", err
	}
	baseline := writer
	n := 0
	for n < len(parts) && parts[n].Seq == from+int64(n) && parts[n].Size > 0 {
		partWriter, err := store.PartWriter(ctx, key, parts[n].Seq)
		if errors.Is(err, ErrObjectNotFound) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if n == 0 && baseline == "" {
			baseline = partWriter
		}
		if partWriter != baseline {
			break
		}
		n++
	}
	return parts[:n], baseline, nil
}
