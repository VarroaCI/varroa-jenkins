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

// RunStateKey returns the object key that holds one replicated state
// file of a running build. relpath is that file's own path under the
// Jenkins home.
func RunStateKey(runID, relpath string) string {
	return fmt.Sprintf("runstate/%s/%s", runID, relpath)
}

// RunStatePrefix returns the key prefix that holds every replicated
// state file of one run. A cleanup deletes the whole prefix.
func RunStatePrefix(runID string) string {
	return fmt.Sprintf("runstate/%s/", runID)
}
