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
// Jenkins home. pusherEpoch names the Jenkins process that pushed it.
//
// The epoch is part of the key because two gateway replicas hold no
// shared order. Without it a retired process's write can land on a live
// process's object, and no rule inside one replica can stop that. Two
// epochs name two keys, so no write reaches another epoch's object.
func RunStateKey(runID string, pusherEpoch int64, relpath string) string {
	return fmt.Sprintf("runstate/%s/%d/%s", runID, pusherEpoch, relpath)
}

// RunStatePrefix returns the key prefix that holds every replicated
// state file of one run. A cleanup deletes the whole prefix. The epoch
// sits inside this prefix, so one delete covers every epoch of a run.
func RunStatePrefix(runID string) string {
	return fmt.Sprintf("runstate/%s/", runID)
}
