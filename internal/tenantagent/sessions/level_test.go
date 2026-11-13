package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// fakeSettings returns one settings row, or an error.
type fakeSettings struct {
	row agent.Settings
	err error
}

func (f fakeSettings) GetSettings(context.Context, string) (agent.Settings, error) {
	return f.row, f.err
}

// TestResumeLevelUnreadableSettingsFailsClosed proves that a settings row that
// cannot be read returns the unchecked code with the read error, so the caller
// makes no provider call.
func TestResumeLevelUnreadableSettingsFailsClosed(t *testing.T) {
	t.Parallel()
	sess := agent.Session{Tenant: "payments", DataSharing: agent.DataLogs}
	code, err := CheckResumeLevel(context.Background(),
		fakeSettings{err: errors.New("store down")}, agent.DataLogs, agent.DataLogs, agent.TrustExternal, sess)
	if code != agent.CodeDataLevelUnchecked || err == nil {
		t.Fatalf("code = %q, err = %v; want the unchecked code with the read error", code, err)
	}
}

// TestResumeLevelFollowsCurrentCeiling proves that a resumed session takes the
// ceiling of the current snapshot. A session that started at logs under a logs
// ceiling is lowered when the ceiling is now none.
func TestResumeLevelFollowsCurrentCeiling(t *testing.T) {
	t.Parallel()
	sess := agent.Session{Tenant: "payments", DataSharing: agent.DataLogs}
	code, err := CheckResumeLevel(context.Background(),
		fakeSettings{}, agent.DataNone, agent.DataLogs, agent.TrustExternal, sess)
	if err != nil || code != agent.CodeDataLevelLowered {
		t.Fatalf("code = %q, err = %v; want the lowered code", code, err)
	}
}

// TestResumeLevelUnchangedContinues proves that an unchanged level lets the
// session continue, with no code and no error.
func TestResumeLevelUnchangedContinues(t *testing.T) {
	t.Parallel()
	sess := agent.Session{Tenant: "payments", DataSharing: agent.DataLogs}
	code, err := CheckResumeLevel(context.Background(),
		fakeSettings{}, agent.DataLogs, agent.DataLogs, agent.TrustExternal, sess)
	if err != nil || code != "" {
		t.Fatalf("code = %q, err = %v; want no code", code, err)
	}
}
