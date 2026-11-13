package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// endRecorder is a store that records each EndSession and fails every other call.
type endRecorder struct {
	agentstore.Store
	ends []agentstore.EndInput
}

func (r *endRecorder) EndSession(_ context.Context, _ uuid.UUID, in agentstore.EndInput) ([]agent.Event, error) {
	r.ends = append(r.ends, in)
	return nil, nil
}

// errorCodeOf returns the code of the first error event of one end.
func errorCodeOf(t *testing.T, in agentstore.EndInput) agent.ErrorCode {
	t.Helper()
	for _, ev := range in.Events {
		if ev.Type != agent.EventError {
			continue
		}
		var p agent.ErrorPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Code
	}
	t.Fatal("the end holds no error event")
	return ""
}

// TestGateUnreadableSettingsEndsWithUncheckedCode proves that the runner gate
// ends the session with data_level_unchecked when the settings cannot be read,
// and reports that the provider call must not run.
func TestGateUnreadableSettingsEndsWithUncheckedCode(t *testing.T) {
	t.Parallel()
	rec := &endRecorder{}
	sess := agent.Session{ID: uuid.New(), Tenant: "payments", DataSharing: agent.DataLogs}
	ok := GateResumeLevel(context.Background(), rec, fakeSettings{err: errors.New("store down")},
		agent.DataLogs, agent.DataLogs, agent.TrustExternal, sess)
	if ok {
		t.Fatal("the gate allowed a provider call with unreadable settings")
	}
	if len(rec.ends) != 1 {
		t.Fatalf("ends = %d, want 1", len(rec.ends))
	}
	if code := errorCodeOf(t, rec.ends[0]); code != agent.CodeDataLevelUnchecked {
		t.Fatalf("code = %q, want %q", code, agent.CodeDataLevelUnchecked)
	}
}

// TestGateLoweredLevelEndsWithLoweredCode proves that a lowered level ends the
// session with data_level_lowered and refuses the provider call.
func TestGateLoweredLevelEndsWithLoweredCode(t *testing.T) {
	t.Parallel()
	rec := &endRecorder{}
	sess := agent.Session{ID: uuid.New(), Tenant: "payments", DataSharing: agent.DataLogs}
	ok := GateResumeLevel(context.Background(), rec, fakeSettings{},
		agent.DataNone, agent.DataLogs, agent.TrustExternal, sess)
	if ok || len(rec.ends) != 1 {
		t.Fatalf("ok = %v, ends = %d; want a refusal", ok, len(rec.ends))
	}
	if code := errorCodeOf(t, rec.ends[0]); code != agent.CodeDataLevelLowered {
		t.Fatalf("code = %q, want %q", code, agent.CodeDataLevelLowered)
	}
}

// TestGateUnchangedLevelContinues proves that an unchanged level passes the gate
// without ending the session.
func TestGateUnchangedLevelContinues(t *testing.T) {
	t.Parallel()
	rec := &endRecorder{}
	sess := agent.Session{ID: uuid.New(), Tenant: "payments", DataSharing: agent.DataLogs}
	if !GateResumeLevel(context.Background(), rec, fakeSettings{},
		agent.DataLogs, agent.DataLogs, agent.TrustExternal, sess) {
		t.Fatal("the gate refused an unchanged level")
	}
	if len(rec.ends) != 0 {
		t.Fatalf("ends = %d, want none", len(rec.ends))
	}
}
