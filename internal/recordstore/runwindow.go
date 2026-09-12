package recordstore

import "time"

// runWindowBefore and runWindowAfter bound the reader's fallback window
// around a run identifier's own UUIDv7 time. The window spans 8 days, so it
// holds at most two monthly partitions.
const (
	runWindowBefore = 24 * time.Hour
	runWindowAfter  = 7 * 24 * time.Hour
)

// runWindow returns the time window the reader's bounded fallback query
// uses for runID. It reports false when runID is not a UUIDv7, and the
// caller then uses the unbounded query alone.
//
// The window is a fast path. It never decides whether a row is readable,
// because nothing bounds the time from a command's delivery to its result.
func runWindow(runID string) (from, to time.Time, ok bool) {
	_, minted, err := parseRunUUIDv7(runID)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return minted.Add(-runWindowBefore), minted.Add(runWindowAfter), true
}
