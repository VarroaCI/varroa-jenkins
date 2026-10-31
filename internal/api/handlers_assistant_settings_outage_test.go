package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// unreachableAssistantStore answers every settings read with ErrUnavailable,
// as the store does while the record store does not answer.
type unreachableAssistantStore struct{ store.Store }

func (unreachableAssistantStore) GetSettings(context.Context, string) (agent.Settings, error) {
	return agent.Settings{}, fmt.Errorf("%w: read settings", store.ErrUnavailable)
}

// TestMyAssistantAnswersStoreOutage asserts that GET /me/assistant answers 503
// while the store does not answer. It never answers 200 with the defaults of
// a read that failed.
func TestMyAssistantAnswersStoreOutage(t *testing.T) {
	f := newAssistantFixture(t)
	f.rt.Store = unreachableAssistantStore{f.rt.Store}
	rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil)
	assertErrorCode(t, rec, http.StatusServiceUnavailable, assistantErrDisabled)
}
