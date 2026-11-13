package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// assistantConfigMaxBody caps the body of a config write. It holds the largest
// stored object: the spec the schema admits (about 80 KiB) plus metadata
// annotations, which the apiserver caps at 256 KiB.
const assistantConfigMaxBody = 512 << 10

// handleGetAssistantConfig answers GET /assistant/config. It needs assistant:admin.
func (s *Server) handleGetAssistantConfig(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	if !s.assistantIsAdmin(auth.ClaimsFromContext(r.Context())) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	obj, serr := rt.GetConfig(r.Context())
	if serr != nil {
		writeAssistantConfigError(s, w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, obj)
}

// handlePutAssistantConfig answers PUT /assistant/config. The body is the whole
// object. A resourceVersion in it fences the write. Without one the write only
// creates the object. It needs assistant:admin and publishes one activity event.
func (s *Server) handlePutAssistantConfig(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantIsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, assistantConfigMaxBody)
	var cr v1alpha1.AssistantConfig
	if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge, "the assistant config body is too large")
			return
		}
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cr.Name = v1alpha1.AssistantConfigName
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = v1alpha1.AssistantConfigKind
	out, serr := rt.PutConfig(r.Context(), &cr)
	if serr != nil {
		writeAssistantConfigError(s, w, serr)
		return
	}
	s.notifyActivity(activity.Event{
		Type:    "assistantconfig.updated",
		Actor:   ActorFrom(claims),
		Cluster: rt.Cluster,
		Message: "AssistantConfig " + v1alpha1.AssistantConfigName + " updated",
	})
	s.writeJSON(w, http.StatusOK, out)
}

// writeAssistantConfigError writes a refusal with its code field.
func writeAssistantConfigError(s *Server, w http.ResponseWriter, e *AssistantConfigError) {
	writeAssistantError(s, w, e.Status, e.Code, e.Message)
}
