package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// userListEntry is the JSON shape for each entry in GET /users.
type userListEntry struct {
	Name        string     `json:"name"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"displayName,omitempty"`
	Groups      []string   `json:"groups"`
	LastLogin   *time.Time `json:"lastLogin,omitempty"`
	ManagedBy   string     `json:"managedBy"`
}

// HandleUsers dispatches /users requests:
//
//	GET  /users       — list users (admin)
//	POST /users       — create user (admin, local only)
func (s *Server) HandleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListUsers(w, r)
	case http.MethodPost:
		s.handleCreateUser(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	users, err := crdstore.List[v1alpha1.User](ctx, s.deps.Store, s.deps.OperatorNamespace, "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	// Pre-fetch groups for local-mode membership resolution.
	mode := s.deps.IdentityConfig.Mode
	var groups []*v1alpha1.Group
	isLocal := mode == string(auth.AuthModeLocal)
	if isLocal {
		groups, _ = crdstore.List[v1alpha1.Group](ctx, s.deps.Store, "", "")
	}

	var result []userListEntry
	for _, u := range users {
		entry := userListEntry{
			Name:        u.Name,
			Email:       u.Spec.Email,
			DisplayName: u.Spec.DisplayName,
			ManagedBy:   managedByLabel(u, mode),
		}
		if u.Status.LastLogin != nil {
			t := u.Status.LastLogin.Time
			entry.LastLogin = &t
		}

		if isLocal {
			entry.Groups = groupsForUser(groups, u.Name)
		} else {
			entry.Groups = u.Status.ObservedGroups
		}
		if entry.Groups == nil {
			entry.Groups = []string{}
		}

		result = append(result, entry)
	}

	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// createUserRequest is the JSON body for POST /users.
type createUserRequest struct {
	Username    string   `json:"username"`
	Email       string   `json:"email,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Password    string   `json:"password"`
	Groups      []string `json:"groups,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if s.deps.IdentityConfig.Mode != string(auth.AuthModeLocal) {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "user creation is only available in local auth mode")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Username == "" {
		s.writeJSONError(w, http.StatusBadRequest, "username is required")
		return
	}
	if req.Password == "" {
		s.writeJSONError(w, http.StatusBadRequest, "password is required")
		return
	}

	// Validate username as DNS-1123 before creating the CRD.
	if err := identity.ValidateLocalUsername(req.Username); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "`+err.Error()+`")
		return
	}

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Username,
			Namespace: s.deps.OperatorNamespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal,
			},
		},
		Spec: v1alpha1.UserSpec{
			Email:       req.Email,
			DisplayName: req.DisplayName,
			Password:    req.Password,
		},
	}

	ctx := context.Background()
	if err := crdstore.Apply[v1alpha1.User](ctx, s.deps.Store, user); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Username})
}

// updateUserRequest is the JSON body for PUT /users/{name}. It edits identity
// fields only; password changes go through the dedicated password endpoint.
type updateUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// HandleUpdateUser handles PUT /users/{name}, updating a local user's email and
// display name. Group membership is managed separately via the Groups endpoints.
func (s *Server) HandleUpdateUser(w http.ResponseWriter, r *http.Request, name string) {
	if s.deps.IdentityConfig.Mode != string(auth.AuthModeLocal) {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "user editing is only available in local auth mode")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := context.Background()
	user, err := crdstore.Get[v1alpha1.User](ctx, s.deps.Store, name, s.deps.OperatorNamespace)
	if err != nil || user == nil {
		s.writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}

	// Update identity fields only; never touch spec.password or status here.
	user.Spec.Email = req.Email
	user.Spec.DisplayName = req.DisplayName
	if err := crdstore.Apply[v1alpha1.User](ctx, s.deps.Store, user); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// HandleDeleteUser handles DELETE /users/{name}.
func (s *Server) HandleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/users/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "user name required")
		return
	}

	ctx := context.Background()

	// Fetch user before deletion for deprovision metadata.
	user, err := crdstore.Get[v1alpha1.User](ctx, s.deps.Store, name, s.deps.OperatorNamespace)
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}

	// Deprovision: strip from both RBAC planes + group membership.
	if err := deprovisionUser(ctx, s.deps.Store, user, s.deps.IdentityConfig.Mode); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("deprovision failed: %s", err.Error()))
		return
	}

	// Delete the User CRD.
	if err := crdstore.Delete[v1alpha1.User](ctx, s.deps.Store, name, s.deps.OperatorNamespace); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// managedByLabel returns the effective managed-by source for display. An
// unlabeled user (pre-existing, heals on next login) is treated as the current
// auth mode's native kind: local mode ⇒ "local", OIDC mode ⇒ "idp".
func managedByLabel(u *v1alpha1.User, mode string) string {
	if v, ok := u.Labels[v1alpha1.LabelManagedBy]; ok {
		return v
	}
	if mode == string(auth.AuthModeLocal) {
		return v1alpha1.ManagedByLocal
	}
	return v1alpha1.ManagedByIDP
}

// groupsForUser returns group names the given username is a member of.
func groupsForUser(groups []*v1alpha1.Group, username string) []string {
	var result []string
	for _, g := range groups {
		for _, m := range g.Spec.Members {
			if m == username {
				result = append(result, g.Name)
				break
			}
		}
	}
	return result
}
