package api

import "encoding/json"

// Trigger source values for the triggerSource field of a run. A value
// names the kind of cause that started the build.
const (
	triggerSourceUser     = "user"
	triggerSourceWebhook  = "webhook"
	triggerSourceCron     = "cron"
	triggerSourceUpstream = "upstream"
	triggerSourceReplay   = "replay"
	triggerSourceRestart  = "restart"
	triggerSourceSCM      = "scm"
	triggerSourceOther    = "other"
)

// triggerCauseClasses maps Java cause class simple names to a trigger
// source. The order sets precedence: the first entry with a matching
// cause decides the result.
var triggerCauseClasses = []struct {
	source  string
	classes []string
}{
	{triggerSourceRestart, []string{"VarroaRestartCause", "RestartDeclarativePipelineCause"}},
	{triggerSourceReplay, []string{"VarroaReplayCause", "ReplayCause"}},
	{triggerSourceUpstream, []string{"UpstreamCause", "BuildUpstreamCause"}},
	{triggerSourceUser, []string{"UserIdCause", "UserCause", "VarroaUserCause"}},
	{triggerSourceCron, []string{"TimerTriggerCause", "ParameterizedTimerTriggerCause"}},
	{triggerSourceWebhook, []string{"GitHubPushCause", "GitHubPRCause", "GitLabWebHookCause", "BranchEventCause", "GenericCause", "RemoteCause"}},
	{triggerSourceSCM, []string{"SCMTriggerCause", "BranchIndexingCause"}},
}

// runOriginOf reads the repository URL and the trigger source from a
// runs.payload column. The run.source copy of scm_url wins over the
// run.started copy. A malformed payload answers two empty strings.
func runOriginOf(payload json.RawMessage) (scmURL, triggerSource string) {
	if len(payload) == 0 {
		return "", ""
	}
	var doc struct {
		Source struct {
			SCMURL string `json:"scm_url"`
		} `json:"run.source"`
		Started struct {
			Payload struct {
				SCMURL     string   `json:"scm_url"`
				CauseTypes []string `json:"cause_types"`
			} `json:"payload"`
		} `json:"run.started"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return "", ""
	}
	scmURL = doc.Source.SCMURL
	if scmURL == "" {
		scmURL = doc.Started.Payload.SCMURL
	}
	return scmURL, classifyTriggerSource(doc.Started.Payload.CauseTypes)
}

// runRejectedSignatureOf reads the script signature the sandbox refused in a
// failed build. It answers the empty string when the payload carries none or
// does not decode.
func runRejectedSignatureOf(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var doc struct {
		Completed struct {
			Payload struct {
				RejectedSignature string `json:"rejected_signature"`
			} `json:"payload"`
		} `json:"run.completed"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ""
	}
	return doc.Completed.Payload.RejectedSignature
}

// classifyTriggerSource maps cause class names to one trigger source.
// It answers the empty string for an empty list.
func classifyTriggerSource(causes []string) string {
	if len(causes) == 0 {
		return ""
	}
	for _, entry := range triggerCauseClasses {
		for _, name := range causes {
			for _, class := range entry.classes {
				if name == class {
					return entry.source
				}
			}
		}
	}
	return triggerSourceOther
}
