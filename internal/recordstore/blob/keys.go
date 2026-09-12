package blob

import "fmt"

// LogKey returns the object key that holds a run's console log. The
// gateway writes it, one chunk per Append call.
func LogKey(runID string) string {
	return fmt.Sprintf("logs/%s", runID)
}

// LogIndexKey returns the object key that holds a run's console log
// index. The gateway writes it alongside the log.
func LogIndexKey(runID string) string {
	return fmt.Sprintf("logindex/%s", runID)
}
