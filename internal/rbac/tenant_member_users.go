package rbac

import (
	"slices"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// userPrincipalAliases maps each configured user claim value to the Jenkins
// principal names of the people who hold it. The source is the idp-managed
// User objects of the operator namespace. Only the configured claim names
// count, and the display name never does. A nil directory yields nil.
func (r *Resolver) userPrincipalAliases() map[string][]string {
	if r.userIndex == nil {
		return nil
	}
	aliases := map[string][]string{}
	for _, obj := range r.userIndex.List() {
		u, ok := obj.(*v1alpha1.User)
		if !ok || u.Namespace != r.userNamespace {
			continue
		}
		if u.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByIDP {
			continue
		}
		principal := auth.PrincipalName(
			u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername],
			u.Spec.Email,
			u.Annotations[v1alpha1.AnnotationOIDCSubject],
		)
		// The agent prefix belongs to Team agent principals only. A person
		// never holds it, whatever the IdP sends.
		if principal == "" || accept.IsPrincipal(principal) {
			continue
		}
		for _, claim := range r.userClaimNames {
			v := userClaimValue(u, claim)
			if v == "" || v == principal || accept.IsPrincipal(v) {
				continue
			}
			if !slices.Contains(aliases[v], principal) {
				aliases[v] = append(aliases[v], principal)
			}
		}
	}
	for v := range aliases {
		slices.Sort(aliases[v])
	}
	return aliases
}

// userClaimValue returns the value of one configured claim for a User object.
// It returns "" for a claim that the User object does not store.
func userClaimValue(u *v1alpha1.User, claim string) string {
	switch claim {
	case "sub":
		return u.Annotations[v1alpha1.AnnotationOIDCSubject]
	case "preferred_username":
		return u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]
	case "email":
		return u.Spec.Email
	default:
		return ""
	}
}

// expandMemberUserSubjects copies the subjects in order. For each User subject
// with an alias, it appends one User entry per alias principal. The result
// holds no duplicate Kind and Name pair. The original subject stays in place.
func expandMemberUserSubjects(subjects []v1alpha1.SubjectRef, aliases map[string][]string) []v1alpha1.SubjectRef {
	out := make([]v1alpha1.SubjectRef, 0, len(subjects))
	seen := map[string]bool{}
	add := func(s v1alpha1.SubjectRef) {
		key := s.Kind + ":" + s.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	for _, s := range subjects {
		add(s)
	}
	for _, s := range subjects {
		if s.Kind != "User" || accept.IsPrincipal(s.Name) {
			continue
		}
		for _, p := range aliases[s.Name] {
			add(v1alpha1.SubjectRef{Kind: "User", Name: p})
		}
	}
	return out
}
