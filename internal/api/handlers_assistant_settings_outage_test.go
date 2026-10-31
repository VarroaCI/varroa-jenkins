package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

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
// with record_store_unreachable while the store does not answer. It never
// answers 200 with the defaults of a read that failed.
func TestMyAssistantAnswersStoreOutage(t *testing.T) {
	f := newAssistantFixture(t)
	f.rt.Store = unreachableAssistantStore{f.rt.Store}
	rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil)
	assertErrorCode(t, rec, http.StatusServiceUnavailable, codeRecordStoreUnreachable)
}

// countingAssistantStore counts the settings reads and answers each one with
// ErrUnavailable.
type countingAssistantStore struct {
	store.Store
	reads *int
}

func (s countingAssistantStore) GetSettings(context.Context, string) (agent.Settings, error) {
	*s.reads++
	return agent.Settings{}, fmt.Errorf("%w: read settings", store.ErrUnavailable)
}

// TestMyAssistantStopsAtTheFirstFailedRead asserts that the view makes one
// read when that read fails. The later reads do not run.
func TestMyAssistantStopsAtTheFirstFailedRead(t *testing.T) {
	f := newAssistantFixture(t)
	reads := 0
	f.rt.Store = countingAssistantStore{Store: f.rt.Store, reads: &reads}
	rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil)
	assertErrorCode(t, rec, http.StatusServiceUnavailable, codeRecordStoreUnreachable)
	if reads != 1 {
		t.Fatalf("settings reads = %d, want 1", reads)
	}
}

// hangingAssistantStore blocks each settings read until its context ends.
type hangingAssistantStore struct{ store.Store }

func (hangingAssistantStore) GetSettings(ctx context.Context, _ string) (agent.Settings, error) {
	<-ctx.Done()
	return agent.Settings{}, ctx.Err()
}

// TestMyAssistantAnswersWithinTheCallDeadline asserts that a store that never
// answers ends the request at the per-call deadline, with 503.
func TestMyAssistantAnswersWithinTheCallDeadline(t *testing.T) {
	f := newAssistantFixture(t)
	f.rt.Store = store.WithTimeout(hangingAssistantStore{f.rt.Store}, 100*time.Millisecond)
	start := time.Now()
	rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request took %v, want under 1s", elapsed)
	}
	assertErrorCode(t, rec, http.StatusServiceUnavailable, codeRecordStoreUnreachable)
}
