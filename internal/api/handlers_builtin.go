package api

import (
	"context"
	"net/http"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// builtinRoleRef is the JSON shape for GET /builtin-roles.
type builtinRoleRef struct {
	Name               string       `json:"name"`
	APIRules           []apiRuleRef `json:"apiRules"`
	JenkinsRoleRef     string       `json:"jenkinsRoleRef"`
	JenkinsPermissions []string     `json:"jenkinsPermissions"`
}

type apiRuleRef struct {
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// HandleBuiltinRoles returns the built-in roles derived live from
// VarroaRole CRDs labeled varroa.dev/builtin: "true". It resolves
// the corresponding JenkinsRole permissions for the data-plane view.
func (s *Server) HandleBuiltinRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := context.Background()
	roles, err := crdstore.List[v1alpha1.VarroaRole](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list roles")
		return
	}

	var result []builtinRoleRef
	for _, role := range roles {
		if role.Labels == nil || role.Labels[v1alpha1.LabelBuiltin] != "true" {
			continue
		}

		ref := builtinRoleRef{
			Name:               role.Name,
			JenkinsRoleRef:     role.Spec.JenkinsRoleRef,
			JenkinsPermissions: role.Spec.JenkinsPermissions,
		}
		for _, rule := range role.Spec.APIRules {
			ref.APIRules = append(ref.APIRules, apiRuleRef{
				Resources: rule.Resources,
				Verbs:     rule.Verbs,
			})
		}

		// Resolve JenkinsRole permissions if a ref is set.
		if role.Spec.JenkinsRoleRef != "" {
			jr, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, s.deps.Store, role.Spec.JenkinsRoleRef, "")
			if err == nil && jr != nil {
				ref.JenkinsPermissions = jr.Spec.Permissions
			}
		}

		result = append(result, ref)
	}

	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}
