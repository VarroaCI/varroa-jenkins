package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestAssistantStoreErrorStatus asserts that a store that does not answer, or
// a store with an old schema, answers 503 assistant_disabled. Any other store
// failure stays a 500.
func TestAssistantStoreErrorStatus(t *testing.T) {
	t.Parallel()
	s := NewServer(&Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"agent store unreachable", fmt.Errorf("%w: read schema version: x", store.ErrUnavailable), 503, assistantErrDisabled},
		{"record store unreachable", fmt.Errorf("%w: read", recordstore.ErrUnavailable), 503, assistantErrDisabled},
		{"wrapped network error", fmt.Errorf("query: %w", netErr), 503, assistantErrDisabled},
		{"agent store schema too old", store.ErrSchemaTooOld, 503, assistantErrDisabled},
		{"record store schema too old", recordstore.ErrSchemaTooOld, 503, assistantErrDisabled},
		{"unclassified failure", errors.New("boom"), 500, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.assistantStoreError(rec, tt.err)
			assertErrorCode(t, rec, tt.status, tt.code)
		})
	}
}
