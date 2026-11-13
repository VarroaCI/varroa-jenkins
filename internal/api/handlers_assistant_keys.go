package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// assistantKeyMaxBody caps the body of a key write. A key is at most 4096 bytes,
// so the JSON wrapper fits well inside 8 KiB.
const assistantKeyMaxBody = 8 << 10

// assistantKeyMaxBytes is the longest provider key the BFF accepts.
const assistantKeyMaxBytes = 4096

// assistantProviderKeyBody is the body of PUT /assistant/providers/{name}/key. It is
// never logged and never echoed.
type assistantProviderKeyBody struct {
	APIKey string `json:"apiKey"`
}

// handlePutAssistantProviderKey answers PUT /assistant/providers/{name}/key. It
// sends the key to the operator, which writes it into the key Secret. The reply
// is 204 with no body. It needs assistant:admin.
func (s *Server) handlePutAssistantProviderKey(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantIsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, assistantKeyMaxBody)
	var in assistantProviderKeyBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge, "the key body is too large")
			return
		}
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	key := []byte(in.APIKey)
	if !validAssistantKey(key) {
		writeAssistantError(s, w, http.StatusBadRequest, "invalid_key", "the key must be 1 to 4096 bytes and not only whitespace")
		return
	}
	if !s.assistantKeyProviderOK(w, rt, name) {
		return
	}
	if err := rt.Operator.SetAssistantKey(r.Context(), rt.Cluster, name, key); err != nil {
		serr := assistantConfigFailure(err)
		writeAssistantConfigError(s, w, serr)
		return
	}
	s.notifyActivity(activity.Event{
		Type:    "assistant.key.set",
		Actor:   ActorFrom(claims),
		Cluster: rt.Cluster,
		Message: "Assistant key set for provider " + name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteAssistantProviderKey answers DELETE /assistant/providers/{name}/key.
// It removes the key of a listed provider from the key Secret. It needs
// assistant:admin.
func (s *Server) handleDeleteAssistantProviderKey(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantIsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	name := r.PathValue("name")
	if !s.assistantKeyProviderOK(w, rt, name) {
		return
	}
	if err := rt.Operator.ClearAssistantKey(r.Context(), rt.Cluster, name); err != nil {
		writeAssistantConfigError(s, w, assistantConfigFailure(err))
		return
	}
	s.notifyActivity(activity.Event{
		Type:    "assistant.key.cleared",
		Actor:   ActorFrom(claims),
		Cluster: rt.Cluster,
		Message: "Assistant key cleared for provider " + name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// assistantKeyProviderOK checks the provider against the snapshot of the
// AssistantConfig. It writes 400 and answers false when the provider is not
// listed or takes no key. A refused write never reaches the operator.
func (s *Server) assistantKeyProviderOK(w http.ResponseWriter, rt *AssistantRuntime, name string) bool {
	if rt.Operator == nil {
		writeAssistantConfigError(s, w, &AssistantConfigError{Status: http.StatusServiceUnavailable, Code: assistantCfgErrUnavailable, Message: assistantCfgErrNoOperator})
		return false
	}
	for _, p := range rt.snap().Config.Providers {
		if p.Name != name {
			continue
		}
		if !p.KeyRequired {
			writeAssistantError(s, w, http.StatusBadRequest, "provider_takes_no_key", "the provider takes no key")
			return false
		}
		return true
	}
	writeAssistantError(s, w, http.StatusBadRequest, "unknown_provider", "the provider is not listed in the assistant config")
	return false
}

// validAssistantKey reports whether a key has 1 to 4096 bytes and holds a byte
// that is not whitespace.
func validAssistantKey(key []byte) bool {
	if len(key) == 0 || len(key) > assistantKeyMaxBytes {
		return false
	}
	return len(bytes.TrimSpace(key)) > 0
}
