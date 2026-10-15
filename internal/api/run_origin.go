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

// runOrigin holds the repository facts and the trigger source of a run.
// SCMForge is empty when the forge is unknown or not one of the three
// forges the UI links to.
type runOrigin struct {
	SCMURL        string
	SCMForge      string
	TriggerSource string
}

// runOriginOf reads the repository URL, its forge, and the trigger source
// from a runs.payload column. The URL and the forge come from the same
// event: the run.source pair wins when its URL is set, else the run.started
// pair serves. A malformed payload answers the zero value.
func runOriginOf(payload json.RawMessage) runOrigin {
	if len(payload) == 0 {
		return runOrigin{}
	}
	var doc struct {
		Source struct {
			SCMURL   string `json:"scm_url"`
			SCMForge string `json:"scm_forge"`
		} `json:"run.source"`
		Started struct {
			Payload struct {
				SCMURL     string   `json:"scm_url"`
				SCMForge   string   `json:"scm_forge"`
				CauseTypes []string `json:"cause_types"`
			} `json:"payload"`
		} `json:"run.started"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return runOrigin{}
	}
	origin := runOrigin{
		SCMURL:        doc.Started.Payload.SCMURL,
		SCMForge:      doc.Started.Payload.SCMForge,
		TriggerSource: classifyTriggerSource(doc.Started.Payload.CauseTypes),
	}
	if doc.Source.SCMURL != "" {
		origin.SCMURL, origin.SCMForge = doc.Source.SCMURL, doc.Source.SCMForge
	}
	origin.SCMForge = knownForge(origin.SCMForge)
	return origin
}

// knownForge answers the forge when it is one the UI links to. It answers
// the empty string for any other value.
func knownForge(forge string) string {
	switch forge {
	case "github", "gitlab", "gitea":
		return forge
	default:
		return ""
	}
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
