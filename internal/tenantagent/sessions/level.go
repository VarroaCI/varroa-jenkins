package sessions

import (
	"context"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// SettingsReader reads the global settings row. The runner store satisfies it.
type SettingsReader interface {
	GetSettings(ctx context.Context, scope string) (agent.Settings, error)
}

// CheckResumeLevel decides whether a resumed Tenant agent session may call its
// provider. It returns an empty code when the session may continue. It returns
// CodeDataLevelLowered when the level the settings allow now is below the level
// the session started with. It returns CodeDataLevelUnchecked, with the read
// error, when the settings cannot be read. A caller that gets a code ends the
// session and makes no provider call.
func CheckResumeLevel(ctx context.Context, st SettingsReader, ceiling, userLevel agent.DataSharing,
	trust agent.ProviderTrust, sess agent.Session) (agent.ErrorCode, error) {
	settings, err := st.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		return agent.CodeDataLevelUnchecked, err
	}
	now := datashare.Effective(datashare.Inputs{
		User:            userLevel,
		Tenant:          sess.Tenant,
		TenantLevels:    settings.TenantDataSharing,
		Trust:           trust,
		ExternalCeiling: ceiling,
	})
	if datashare.Rank(now) < datashare.Rank(sess.DataSharing) {
		return agent.CodeDataLevelLowered, nil
	}
	return "", nil
}

// GateResumeLevel runs the level check before a resumed session calls its
// provider. It returns true when the session may continue. A refusal ends the
// running session with the matching error code, so no provider call runs.
func GateResumeLevel(ctx context.Context, writer agentstore.Store, st SettingsReader,
	ceiling, userLevel agent.DataSharing, trust agent.ProviderTrust, sess agent.Session) bool {
	code, err := CheckResumeLevel(ctx, st, ceiling, userLevel, trust, sess)
	if err == nil && code == "" {
		return true
	}
	message := "the data level of this session is lower now"
	if err != nil || code == agent.CodeDataLevelUnchecked {
		code = agent.CodeDataLevelUnchecked
		message = "the data level could not be checked"
	}
	errEv, _ := agent.NewEvent(agent.EventError, agent.ErrorPayload{Code: code, Message: message})
	done, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateFailed, Stop: "error"})
	_, _ = writer.EndSession(ctx, sess.ID, agentstore.EndInput{
		From:       []agent.SessionState{agent.StateRunning},
		LeaseOwner: sess.LeaseOwner,
		State:      agent.StateFailed,
		Events:     []agent.EventInput{errEv, done},
	})
	return false
}
