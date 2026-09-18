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
// Jenkins home. pusherEpoch names the Jenkins process that pushed it,
// and stateGen names that process's own push generation.
//
// Both numbers are part of the key because two gateway replicas hold no
// shared order. Without them a superseded write can land on the live
// object, and no rule inside one replica can stop that. A gateway
// rollout leaves two replicas that forward chunks of one process, so the
// epoch alone does not order two generations. Every push names its own
// key, so no write reaches another push's object.
func RunStateKey(runID string, pusherEpoch, stateGen int64, relpath string) string {
	return fmt.Sprintf("runstate/%s/%d/%d/%s", runID, pusherEpoch, stateGen, relpath)
}

// RunStatePrefix returns the key prefix that holds every replicated
// state file of one run. A cleanup deletes the whole prefix. The epoch
// and the generation sit inside this prefix, so one delete covers every
// push of a run.
func RunStatePrefix(runID string) string {
	return fmt.Sprintf("runstate/%s/", runID)
}
