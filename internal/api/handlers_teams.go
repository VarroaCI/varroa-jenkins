package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// teamEntry is the JSON shape for GET /teams entries.
type teamEntry struct {
	Name                string                        `json:"name"`
	DisplayName         string                        `json:"displayName,omitempty"`
	Members             []string                      `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef         `json:"subjects,omitempty"`
	Namespaces          []string                      `json:"namespaces"`
	RoleRef             string                        `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                          `json:"provisionNamespaces,omitempty"`
	ObservedGeneration  int64                         `json:"observedGeneration,omitempty"`
	GroupRef            string                        `json:"groupRef,omitempty"`
	BindingRef          string                        `json:"bindingRef,omitempty"`
	NamespaceStates     []v1alpha1.TeamNamespaceState `json:"namespaceStates,omitempty"`
	Conditions          []v1alpha1.TeamCondition      `json:"conditions,omitempty"`
}

// teamFromCRD builds a teamEntry from a Team CRD.
func teamFromCRD(t *v1alpha1.Team) teamEntry {
	return teamEntry{
		Name:                t.Name,
		DisplayName:         t.Spec.DisplayName,
		Members:             t.Spec.Members,
		Subjects:            t.Spec.Subjects,
		Namespaces:          t.Spec.Namespaces,
		RoleRef:             t.Spec.RoleRef,
		ProvisionNamespaces: t.Spec.ProvisionNamespaces,
		ObservedGeneration:  t.Status.ObservedGeneration,
		GroupRef:            t.Status.GroupRef,
		BindingRef:          t.Status.BindingRef,
		NamespaceStates:     t.Status.NamespaceStates,
		Conditions:          t.Status.Conditions,
	}
}

// HandleTeams dispatches /teams requests:
//
//	GET    /teams          — list teams (admin)
//	POST   /teams          — create team (admin)
func (s *Server) HandleTeams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListTeams(w, r)
	case http.MethodPost:
		s.handleCreateTeam(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	teams, err := crdstore.List[v1alpha1.Team](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list teams")
		return
	}

	result := make([]teamEntry, 0, len(teams))
	for _, t := range teams {
		result = append(result, teamFromCRD(t))
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// createTeamRequest is the JSON body for POST /teams.
type createTeamRequest struct {
	Name                string                `json:"name"`
	DisplayName         string                `json:"displayName,omitempty"`
	Members             []string              `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef `json:"subjects,omitempty"`
	Namespaces          []string              `json:"namespaces"`
	RoleRef             string                `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                  `json:"provisionNamespaces,omitempty"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Validate: at least one of members or subjects.
	if len(req.Members) == 0 && len(req.Subjects) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one of members or subjects must be set")
		return
	}

	// Validate: at least one namespace.
	if len(req.Namespaces) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}

	// Validate: roleRef must not be "admin".
	if req.RoleRef == "admin" {
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Team")
		return
	}

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
		},
		Spec: v1alpha1.TeamSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
		},
	}

	ctx := context.Background()
	if err := crdstore.Apply[v1alpha1.Team](ctx, s.deps.Store, team); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create team")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// HandleTeamDispatch dispatches /teams/{name} requests:
//
//	GET    /teams/{name} — get team detail (admin)
//	PUT    /teams/{name} — update team (admin)
//	DELETE /teams/{name} — delete team (admin)
func (s *Server) HandleTeamDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/teams/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "team name required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetTeam(w, r, name)
	case http.MethodPut:
		s.handleUpdateTeam(w, r, name)
	case http.MethodDelete:
		s.handleDeleteTeam(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "team not found")
		return
	}

	s.writeJSON(w, http.StatusOK, teamFromCRD(team))
}

func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate: at least one of members or subjects.
	if len(req.Members) == 0 && len(req.Subjects) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one of members or subjects must be set")
		return
	}

	// Validate: at least one namespace.
	if len(req.Namespaces) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}

	// Validate: roleRef must not be "admin".
	if req.RoleRef == "admin" {
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Team")
		return
	}

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.TeamSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
		},
	}

	ctx := context.Background()
	if err := crdstore.Apply[v1alpha1.Team](ctx, s.deps.Store, team); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update team")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	if err := crdstore.Delete[v1alpha1.Team](ctx, s.deps.Store, name, ""); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete team")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
