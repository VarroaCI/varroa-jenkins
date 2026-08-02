package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// broodScheduleCreateRequest is the JSON body for POST /brood-schedules.
type broodScheduleCreateRequest struct {
	Namespace string                     `json:"namespace,omitempty"`
	Name      string                     `json:"name"`
	Cluster   string                     `json:"cluster,omitempty"`
	Spec      v1alpha1.BroodScheduleSpec `json:"spec"`
}

// broodScheduleSuspendRequest is the JSON body for POST /brood-schedules/{ns}/{name}/suspend.
type broodScheduleSuspendRequest struct {
	Suspend bool `json:"suspend"`
}

// handleBroodSchedules routes /brood-schedules and /brood-schedules/ paths.
func (s *Server) handleBroodSchedules(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/brood-schedules") {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(path, "/brood-schedules")

	switch suffix {
	case "", "/":
		switch r.Method {
		case http.MethodGet:
			s.handleBroodScheduleList(w, r)
		case http.MethodPost:
			s.handleBroodScheduleCreate(w, r)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		segments := strings.Split(strings.Trim(suffix, "/"), "/")
		switch {
		case len(segments) == 2 && segments[0] != "" && segments[1] != "":
			switch r.Method {
			case http.MethodGet:
				s.handleBroodScheduleGet(w, r, segments[0], segments[1])
			case http.MethodDelete:
				s.handleBroodScheduleDelete(w, r, segments[0], segments[1])
			default:
				s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case len(segments) == 3 && segments[0] != "" && segments[1] != "" && segments[2] == "suspend":
			if r.Method != http.MethodPost {
				s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			s.handleBroodScheduleSuspend(w, r, segments[0], segments[1])
		default:
			http.NotFound(w, r)
		}
	}
}

// handleBroodScheduleCreate handles POST /brood-schedules.
func (s *Server) handleBroodScheduleCreate(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req broodScheduleCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.deps.OperatorNamespace
	}
	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "name required")
		return
	}

	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Tenancy pre-check.
	if err := controller.ValidateBroodTenancy(
		v1alpha1.BroodOperationSpec{Targets: req.Spec.Template.Targets},
		ns, s.deps.OperatorNamespace,
	); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Schedule-specific: at most one cluster in team-namespace mode.
	if ns != s.deps.OperatorNamespace && len(req.Spec.Template.Clusters) > 1 {
		s.writeJSONError(w, http.StatusBadRequest, "at most one cluster allowed in team namespace")
		return
	}

	scheds := s.deps.BroodSchedules
	if scheds == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "brood schedules not available")
		return
	}

	resp, err := scheds.Create(r.Context(), req.Cluster, ns, req.Name, req.Spec)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusCreated, resp)
}

// handleBroodScheduleList handles GET /brood-schedules[?namespace=].
func (s *Server) handleBroodScheduleList(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	ns := r.URL.Query().Get("namespace")

	if !s.broodOpAccess(claims, ns, false) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	scheds := s.deps.BroodSchedules
	if scheds == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "brood schedules not available")
		return
	}

	items, err := scheds.List(r.Context(), ns)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": items,
	})
}

// handleBroodScheduleGet handles GET /brood-schedules/{ns}/{name}.
func (s *Server) handleBroodScheduleGet(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if !s.broodOpAccess(claims, ns, false) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	scheds := s.deps.BroodSchedules
	if scheds == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "brood schedules not available")
		return
	}

	resp, err := scheds.Get(r.Context(), "", ns, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			s.writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleBroodScheduleDelete handles DELETE /brood-schedules/{ns}/{name}.
func (s *Server) handleBroodScheduleDelete(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	scheds := s.deps.BroodSchedules
	if scheds == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "brood schedules not available")
		return
	}

	if err := scheds.Delete(r.Context(), "", ns, name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			s.writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleBroodScheduleSuspend handles POST /brood-schedules/{ns}/{name}/suspend.
func (s *Server) handleBroodScheduleSuspend(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req broodScheduleSuspendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	scheds := s.deps.BroodSchedules
	if scheds == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "brood schedules not available")
		return
	}

	if err := scheds.Suspend(r.Context(), "", ns, name, req.Suspend); err != nil {
		if strings.Contains(err.Error(), "not found") {
			s.writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"suspend": req.Suspend,
	})
}
