package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// settingsFailingStore reads every settings row with an error and passes the
// other calls through.
type settingsFailingStore struct{ store.Store }

func (settingsFailingStore) GetSettings(context.Context, string) (agent.Settings, error) {
	return agent.Settings{}, errors.New("settings unreadable")
}

// externalSnapshot returns a snapshot whose only provider is external, with the
// given ceiling. The provider adapter is a scripted one that is never used by
// the refusals these tests check.
func externalSnapshot(t *testing.T, ceiling agent.DataSharing) *assistantconfig.Snapshot {
	t.Helper()
	cfg := provider.Config{
		Providers: []provider.Endpoint{{
			Name: "smoke", Kind: agent.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
			Models: []string{"m1"}, Trust: agent.TrustExternal, Default: true,
		}},
		ExternalDataSharing: ceiling,
		Budgets:             provider.Budgets{Session: 100000, UserDay: 1000000, TenantDay: 1000000, Steps: 32},
	}
	factories := map[agent.ProviderKind]provider.Factory{
		agent.ProviderOpenAICompatible: func(provider.Endpoint) (provider.Provider, error) {
			return &scriptedAssistantProvider{}, nil
		},
	}
	reg, err := provider.NewRegistry(cfg, factories, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &assistantconfig.Snapshot{Config: cfg, Registry: reg}
}

// startCompletedSession starts a session that completes one turn, and returns
// its id and the state the store holds after the run.
func startCompletedSession(t *testing.T, f *assistantFixture) assistantSessionJSON {
	t.Helper()
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	return sess
}

// TestAssistantMessageFollowsCurrentCeiling proves that a message to a session
// that started at logs reads the ceiling of the current snapshot. With the
// ceiling lowered to none the message is refused, and the session stays as it
// was.
func TestAssistantMessageFollowsCurrentCeiling(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("first"))
	sess := startCompletedSession(t, f)
	f.rt.Live.Store(externalSnapshot(t, agent.DataNone))

	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions/"+sess.ID+"/messages", map[string]any{"message": "again"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("message = %d body = %s, want 409", rec.Code, rec.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != assistantErrDataLevel {
		t.Fatalf("code = %q (err %v), want %s", body.Code, err, assistantErrDataLevel)
	}
	got, err := f.mem.GetSession(context.Background(), mustUUID(t, sess.ID))
	if err != nil || got.State != agent.StateCompleted {
		t.Fatalf("session state = %v (err %v), want completed", got.State, err)
	}
}

// TestAssistantMessageFailsClosedOnUnreadableSettings proves that a message
// whose settings row cannot be read makes no run and leaves the session as it
// was.
func TestAssistantMessageFailsClosedOnUnreadableSettings(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("first"))
	sess := startCompletedSession(t, f)
	f.rt.Store = settingsFailingStore{Store: f.rt.Store}

	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions/"+sess.ID+"/messages", map[string]any{"message": "again"})
	if rec.Code == http.StatusAccepted {
		t.Fatalf("message = 202 with unreadable settings, want a refusal")
	}
	f.rt.Wait()
	got, err := f.mem.GetSession(context.Background(), mustUUID(t, sess.ID))
	if err != nil || got.State != agent.StateCompleted {
		t.Fatalf("session state = %v (err %v), want completed", got.State, err)
	}
}
