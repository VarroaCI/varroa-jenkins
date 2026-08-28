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

// groupEntry is the JSON shape for GET /groups entries.
type groupEntry struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Members     []string `json:"members"`
	MemberCount int      `json:"memberCount"`
	Source      string   `json:"source"` // "local" | "idp" | "both"
}

// HandleGroups dispatches /groups requests:
//
//	GET    /groups          — list groups (admin)
//	POST   /groups          — create group (admin, local only)
func (s *Server) HandleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListGroups(w, r)
	case http.MethodPost:
		s.handleCreateGroup(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	groups, err := crdstore.List[v1alpha1.Group](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}

	// Build member counts from observed groups (users who have logged in).
	idpGroupCounts := make(map[string]int)
	users, err := crdstore.List[v1alpha1.User](ctx, s.deps.Store, s.deps.OperatorNamespace, "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	for _, u := range users {
		for _, g := range u.Status.ObservedGroups {
			idpGroupCounts[g]++
		}
	}

	// Collect all group names from both sources.
	allGroupNames := make(map[string]bool)
	for _, g := range groups {
		allGroupNames[g.Name] = true
	}
	for g := range idpGroupCounts {
		allGroupNames[g] = true
	}

	var result []groupEntry
	for name := range allGroupNames {
		var localGroup *v1alpha1.Group
		for _, g := range groups {
			if g.Name == name {
				localGroup = g
				break
			}
		}

		idpCount := idpGroupCounts[name]
		localCount := 0
		var members []string
		var displayName string

		if localGroup != nil {
			localCount = len(localGroup.Spec.Members)
			members = localGroup.Spec.Members
			displayName = localGroup.Spec.DisplayName
		}
		if members == nil {
			members = []string{}
		}

		source := "local"
		memberCount := localCount
		if localGroup != nil && idpCount > 0 {
			source = "both"
			if idpCount > localCount {
				memberCount = idpCount
			}
		} else if localGroup == nil {
			source = "idp"
			memberCount = idpCount
		}

		result = append(result, groupEntry{
			Name:        name,
			DisplayName: displayName,
			Members:     members,
			MemberCount: memberCount,
			Source:      source,
		})
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// createGroupRequest is the JSON body for POST /groups.
type createGroupRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Members     []string `json:"members,omitempty"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if s.deps.IdentityConfig.Mode != string(auth.AuthModeLocal) {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "group management is only available in local auth mode")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
		},
		Spec: v1alpha1.GroupSpec{
			DisplayName: req.DisplayName,
			Members:     req.Members,
		},
	}

	ctx := context.Background()
	if err := crdstore.Apply[v1alpha1.Group](ctx, s.deps.Store, group); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create group")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// HandleGroupDispatch dispatches /groups/{name} requests:
//
//	DELETE /groups/{name} — delete group (admin, local only)
func (s *Server) HandleGroupDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/groups/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "group name required")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.handleDeleteGroup(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request, name string) {
	if s.deps.IdentityConfig.Mode != string(auth.AuthModeLocal) {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "group management is only available in local auth mode")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	if err := crdstore.Delete[v1alpha1.Group](ctx, s.deps.Store, name, ""); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
