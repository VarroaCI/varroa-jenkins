package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ErrNotLocalAccount is returned by UpdateLocalUser when the stored User is
// not a local account.
var ErrNotLocalAccount = errors.New("only a local account can be edited")

// PurposeError wraps a purpose validation failure raised by UpdateLocalUser,
// so a caller can answer with the validation message.
type PurposeError struct{ err error }

func (p PurposeError) Error() string { return p.err.Error() }

func (p PurposeError) Unwrap() error { return p.err }

// userListEntry is the JSON shape for each entry in GET /users.
type userListEntry struct {
	Name        string     `json:"name"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"displayName,omitempty"`
	Groups      []string   `json:"groups"`
	LastLogin   *time.Time `json:"lastLogin,omitempty"`
	ManagedBy   string     `json:"managedBy"`
	Purpose     string     `json:"purpose,omitempty"`
}

// HandleUsers dispatches /users requests:
//
//	GET  /users       — list users (admin)
//	POST /users       — create user (admin, local accounts only)
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

	ctx := r.Context()
	users, err := crdstore.List[v1alpha1.User](ctx, s.deps.Store, s.deps.OperatorNamespace, "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	// Pre-fetch groups for local account membership resolution.
	mode := s.deps.IdentityConfig.Mode
	var groups []*v1alpha1.Group
	if s.localAccountsOn() {
		groups, _ = crdstore.List[v1alpha1.Group](ctx, s.deps.Store, "", "")
	}

	var result []userListEntry
	for _, u := range users {
		entry := userListEntry{
			Name:        u.Name,
			Email:       u.Spec.Email,
			DisplayName: u.Spec.DisplayName,
			ManagedBy:   managedByLabel(u, mode),
			Purpose:     string(u.Spec.Purpose),
		}
		if u.Status.LastLogin != nil {
			t := u.Status.LastLogin.Time
			entry.LastLogin = &t
		}

		if entry.ManagedBy == v1alpha1.ManagedByLocal {
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
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Password    string `json:"password"`
	Purpose     string `json:"purpose,omitempty"`
}

// validateUserPurpose checks a local account purpose. Beside OIDC a purpose is
// required; in local mode it is optional.
func validateUserPurpose(cfg IdentityConfig, purpose string) error {
	switch v1alpha1.UserPurpose(purpose) {
	case v1alpha1.UserPurposeService, v1alpha1.UserPurposeBreakGlass:
		return nil
	case "":
		if cfg.LocalAccounts {
			return fmt.Errorf("purpose is required for a local account beside OIDC: service or break-glass")
		}
		return nil
	default:
		return fmt.Errorf("purpose must be service or break-glass")
	}
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.localAccountsOn() {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "user creation is only available with local accounts")
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
	if err := validateUserPurpose(s.deps.IdentityConfig, req.Purpose); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate username as DNS-1123 before creating the CRD.
	if err := identity.ValidateLocalUsername(req.Username); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
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
			Purpose:     v1alpha1.UserPurpose(req.Purpose),
		},
	}

	ctx := r.Context()
	if err := crdstore.Create[v1alpha1.User](ctx, s.deps.Store, user); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeJSONError(w, http.StatusConflict, "user already exists")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	s.notifyUserActivity(claims, UserCreated, req.Username)
	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Username})
}

// updateUserRequest is the JSON body for PUT /users/{name}. It edits identity
// fields only; password changes go through the dedicated password endpoint.
type updateUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Purpose     string `json:"purpose,omitempty"`
}

// HandleUpdateUser handles PUT /users/{name}, updating a local user's email,
// display name, and purpose. Group membership is managed separately via the Groups endpoints.
func (s *Server) HandleUpdateUser(w http.ResponseWriter, r *http.Request, name string) {
	if !s.localAccountsOn() {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "user editing is only available with local accounts")
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
	_, err := UpdateLocalUser(r.Context(), s.deps.Store, s.deps.IdentityConfig, s.deps.OperatorNamespace, name, LocalUserEdit{
		Email:       &req.Email,
		DisplayName: &req.DisplayName,
		Purpose:     v1alpha1.UserPurpose(req.Purpose),
	})
	var pe PurposeError
	switch {
	case apierrors.IsNotFound(err):
		s.writeJSONError(w, http.StatusNotFound, "user not found")
		return
	case errors.Is(err, ErrNotLocalAccount):
		s.writeJSONError(w, http.StatusBadRequest, ErrNotLocalAccount.Error())
		return
	case errors.As(err, &pe):
		s.writeJSONError(w, http.StatusBadRequest, pe.Error())
		return
	case err != nil:
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	s.notifyUserActivity(claims, UserUpdated, name)
	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// LocalUserEdit holds the identity fields that UpdateLocalUser changes. A nil
// Email or DisplayName keeps the stored value. An empty Purpose keeps the
// stored purpose.
type LocalUserEdit struct {
	Email       *string
	DisplayName *string
	Purpose     v1alpha1.UserPurpose
}

// UpdateLocalUser edits the identity fields of a local account. It refuses a
// User that is not a local account. It never touches spec.password or status.
// Every surface that edits a User calls this function.
func UpdateLocalUser(ctx context.Context, store crdstore.Backend, cfg IdentityConfig,
	namespace, name string, edit LocalUserEdit) (*v1alpha1.User, error) {
	var updated *v1alpha1.User
	_, err := crdstore.Mutate[v1alpha1.User](ctx, store, name, namespace, func(user *v1alpha1.User) (bool, error) {
		if managedByLabel(user, cfg.Mode) != v1alpha1.ManagedByLocal {
			return false, ErrNotLocalAccount
		}

		// An empty purpose keeps the stored one, so an edit of other fields
		// never clears the purpose that the sign-in gate reads.
		purpose := string(edit.Purpose)
		if purpose == "" {
			purpose = string(user.Spec.Purpose)
		}
		if err := validateUserPurpose(cfg, purpose); err != nil {
			return false, PurposeError{err: err}
		}

		if edit.Email != nil {
			user.Spec.Email = *edit.Email
		}
		if edit.DisplayName != nil {
			user.Spec.DisplayName = *edit.DisplayName
		}
		user.Spec.Purpose = v1alpha1.UserPurpose(purpose)
		updated = user
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
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

	if err := DeleteUser(r.Context(), s.deps.Store, s.deps.OperatorNamespace, name, s.deps.IdentityConfig.Mode); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			s.writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyUserActivity(claims, UserDeleted, name)
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
