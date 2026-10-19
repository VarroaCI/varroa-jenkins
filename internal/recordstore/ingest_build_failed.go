package recordstore

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// buildNotFailed lists the terminal results that are not a failure. A
// successful build and an interrupted build write no build.failed row.
var buildNotFailed = map[string]bool{"SUCCESS": true, "INTERRUPTED": true}

// isBuildFailure reports whether a terminal result counts as a failed build.
func isBuildFailure(result string) bool {
	return !buildNotFailed[result]
}

// buildFailedEvent builds the build.failed activity event of one owner
// run.completed. The event time is the run end when the payload names one,
// and the envelope time otherwise. The caller checks the result first.
func buildFailedEvent(intent *intentRow, payload RunCompletedPayload, env IngestEnvelope) activity.Event {
	at := env.OccurredAt
	if payload.EndedAt != nil {
		at = *payload.EndedAt
	}
	result := *payload.Result
	ev := activity.Event{
		Timestamp: at,
		Type:      "build.failed",
		Source:    "recorder",
		Severity:  "error",
		Cluster:   intent.cluster,
		Namespace: intent.namespace,
		ItemPath:  intent.jobPath,
		Result:    result,
		Message:   fmt.Sprintf("%s failed (%s)", intent.jobPath, result),
	}
	if intent.cell != nil {
		ev.Controller = *intent.cell
	}
	if intent.buildNumber != nil {
		ev.BuildNumber = *intent.buildNumber
		ev.Message = fmt.Sprintf("%s #%d failed (%s)", intent.jobPath, *intent.buildNumber, result)
	}
	return ev
}
