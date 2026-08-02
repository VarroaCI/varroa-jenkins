package controller

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"

	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// RoleReconciler seeds and reconciles built-in VarroaRoles and JenkinsRoles.
type RoleReconciler struct {
	client ResourceClient
	store  crdstore.Backend
	Logger *slog.Logger
}

// NewRoleReconciler creates a RoleReconciler.
func NewRoleReconciler(client ResourceClient, store crdstore.Backend) *RoleReconciler {
	return &RoleReconciler{client: client, store: store}
}

// Reconcile ensures the built-in VarroaRoles and JenkinsRoles exist with correct specs.
// Built-in roles are labeled varroa.dev/builtin: "true" and are recreated
// if deleted or restored to spec if tampered with.
func (r *RoleReconciler) Reconcile(ctx context.Context) error {
	if err := r.reconcileVarroaRoles(ctx); err != nil {
		return err
	}
	if err := r.reconcileJenkinsRoles(ctx); err != nil {
		return err
	}
	return r.migrateCustomRoles(ctx)
}

func (r *RoleReconciler) reconcileVarroaRoles(ctx context.Context) error {
	for _, want := range builtinRoles() {
		existing, err := crdstore.Get[v1alpha1.VarroaRole](ctx, r.store, want.Name, "")
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get varroarole %s: %w", want.Name, err)
			}
			if err := crdstore.Apply[v1alpha1.VarroaRole](ctx, r.store, want); err != nil {
				return fmt.Errorf("create varroarole %s: %w", want.Name, err)
			}
			r.Logger.Info("created built-in VarroaRole", "role", want.Name)
			continue
		}

		needsUpdate := false
		if existing.Labels == nil || existing.Labels[v1alpha1.LabelBuiltin] != "true" {
			needsUpdate = true
		}
		if !needsUpdate {
			if !apiRulesEqual(existing.Spec.APIRules, want.Spec.APIRules) ||
				!stringSlicesEqual(existing.Spec.JenkinsPermissions, want.Spec.JenkinsPermissions) ||
				existing.Spec.JenkinsRoleRef != want.Spec.JenkinsRoleRef {
				needsUpdate = true
			}
		}

		if needsUpdate {
			if existing.Labels == nil {
				existing.Labels = make(map[string]string)
			}
			existing.Labels[v1alpha1.LabelBuiltin] = "true"
			existing.Spec = want.Spec
			if err := crdstore.Apply[v1alpha1.VarroaRole](ctx, r.store, existing); err != nil {
				return fmt.Errorf("reconcile varroarole %s: %w", want.Name, err)
			}
			r.Logger.Info("reconciled built-in VarroaRole", "role", want.Name)
		}
	}
	return nil
}

func (r *RoleReconciler) reconcileJenkinsRoles(ctx context.Context) error {
	for _, want := range builtinJenkinsRoles() {
		// Defensive validation: built-in roles must be valid.
		if err := v1alpha1.ValidateJenkinsRole(want); err != nil {
			r.Logger.Error("built-in JenkinsRole validation failed (BUG)", "role", want.Name, "error", err)
			continue
		}

		existing, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, r.store, want.Name, "")
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get jenkinsrole %s: %w", want.Name, err)
			}
			if err := crdstore.Apply[v1alpha1.JenkinsRole](ctx, r.store, want); err != nil {
				return fmt.Errorf("create jenkinsrole %s: %w", want.Name, err)
			}
			r.Logger.Info("created built-in JenkinsRole", "role", want.Name)
			continue
		}

		needsUpdate := false
		if existing.Labels == nil || existing.Labels[v1alpha1.LabelBuiltin] != "true" {
			needsUpdate = true
		}
		if !needsUpdate {
			if existing.Spec.RoleType != want.Spec.RoleType ||
				!stringSlicesEqual(existing.Spec.Permissions, want.Spec.Permissions) {
				needsUpdate = true
			}
		}

		if needsUpdate {
			if existing.Labels == nil {
				existing.Labels = make(map[string]string)
			}
			existing.Labels[v1alpha1.LabelBuiltin] = "true"
			existing.Spec = want.Spec
			if err := crdstore.Apply[v1alpha1.JenkinsRole](ctx, r.store, existing); err != nil {
				return fmt.Errorf("reconcile jenkinsrole %s: %w", want.Name, err)
			}
			r.Logger.Info("reconciled built-in JenkinsRole", "role", want.Name)
		}
	}
	return nil
}

// migrateCustomRoles finds custom VarroaRoles that have JenkinsPermissions but no
// JenkinsRoleRef, synthesizes a JenkinsRole for them, and sets JenkinsRoleRef.
func (r *RoleReconciler) migrateCustomRoles(ctx context.Context) error {
	roles, err := crdstore.List[v1alpha1.VarroaRole](ctx, r.store, "", "")
	if err != nil {
		return fmt.Errorf("list varroaroles for migration: %w", err)
	}
	for _, role := range roles {
		// Skip built-in roles (already have JenkinsRoleRef set by reconcileVarroaRoles)
		if role.Labels != nil && role.Labels[v1alpha1.LabelBuiltin] == "true" {
			continue
		}
		if len(role.Spec.JenkinsPermissions) == 0 || role.Spec.JenkinsRoleRef != "" {
			continue
		}
		// Synthesize a JenkinsRole from the inline permissions
		jrName := "varroa-custom-" + role.Name
		jr := &v1alpha1.JenkinsRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRole"},
			ObjectMeta: metav1.ObjectMeta{Name: jrName},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Global",
				Permissions: role.Spec.JenkinsPermissions,
				Description: "Migrated from VarroaRole " + role.Name,
			},
		}
		if err := v1alpha1.ValidateJenkinsRole(jr); err != nil {
			r.Logger.Warn("skipping migration of invalid custom VarroaRole",
				"varroaRole", role.Name, "error", err)
			continue
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRole](ctx, r.store, jr); err != nil {
			return fmt.Errorf("migrate custom role %s: %w", role.Name, err)
		}
		role.Spec.JenkinsRoleRef = jrName
		if err := crdstore.Apply[v1alpha1.VarroaRole](ctx, r.store, role); err != nil {
			return fmt.Errorf("set jenkinsRoleRef on custom role %s: %w", role.Name, err)
		}
		r.Logger.Info("migrated custom VarroaRole to JenkinsRole", "varroaRole", role.Name, "jenkinsRole", jrName)
	}
	return nil
}

func apiRulesEqual(a, b []v1alpha1.APIRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !stringSlicesEqual(a[i].Resources, b[i].Resources) ||
			!stringSlicesEqual(a[i].Verbs, b[i].Verbs) {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func builtinRoles() []*v1alpha1.VarroaRole {
	return []*v1alpha1.VarroaRole{
		buildRole("admin",
			[]v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
			adminPermissions(),
			"varroa-admin"),
		buildRole("operator",
			[]v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"read", "create", "update", "delete", "approve-restart", "approve-deletion", "manage"}},
				{Resources: []string{"templates", "provisioningdefaults"}, Verbs: []string{"read", "update"}},
				{Resources: []string{"roles", "rolebindings", "jenkinsroles", "jenkinsrolebindings"}, Verbs: []string{"read"}},
				{Resources: []string{"catalogsources"}, Verbs: []string{"read", "create", "update", "delete"}},
				{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
				{Resources: []string{"composedbundles"}, Verbs: []string{"read", "create", "update", "delete"}},
				{Resources: []string{"version-profiles"}, Verbs: []string{"read", "create", "update", "delete"}},
				{Resources: []string{"updatecenter"}, Verbs: []string{"upload"}},
			},
			operatorPermissions(),
			"varroa-operator"),
		buildRole("developer",
			[]v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"read", "approve-restart"}},
				{Resources: []string{"templates", "provisioningdefaults", "roles", "rolebindings", "jenkinsroles", "jenkinsrolebindings"}, Verbs: []string{"read"}},
				{Resources: []string{"catalogsources"}, Verbs: []string{"read", "create", "update", "delete"}},
				{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
				{Resources: []string{"composedbundles"}, Verbs: []string{"read", "create", "update"}},
			},
			developerPermissions(),
			"varroa-developer"),
		buildRole("viewer",
			[]v1alpha1.APIRule{
				{Resources: []string{"controllers", "templates", "provisioningdefaults", "roles", "rolebindings", "jenkinsroles", "jenkinsrolebindings"}, Verbs: []string{"read"}},
				{Resources: []string{"catalogsources", "catalogitems", "composedbundles"}, Verbs: []string{"read"}},
			},
			viewerPermissions(),
			"varroa-viewer"),
	}
}

func buildRole(name string, apiRules []v1alpha1.APIRule, perms []string, jenkinsRoleRef string) *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "VarroaRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.LabelBuiltin: "true"}},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: apiRules, JenkinsPermissions: perms, JenkinsRoleRef: jenkinsRoleRef},
	}
}

func builtinJenkinsRoles() []*v1alpha1.JenkinsRole {
	return BuiltinJenkinsRoles()
}

// BuiltinJenkinsRoles returns deep copies of the canonical JenkinsRole definitions.
func BuiltinJenkinsRoles() []*v1alpha1.JenkinsRole {
	return rbac.BuiltinJenkinsRoles()
}

// Permission sets used by the built-in VarroaRoles.
func adminPermissions() []string {
	return []string{
		"hudson.model.Hudson.Administer", "hudson.model.Hudson.Read", "hudson.model.Item.Read",
		"hudson.model.Item.Build", "hudson.model.Item.Configure", "hudson.model.Item.Create",
		"hudson.model.Item.Delete", "hudson.model.Item.Discover", "hudson.model.Item.Workspace",
		"hudson.model.Run.Delete", "hudson.model.Run.Update", "hudson.model.View.Read",
		"hudson.model.View.Configure", "hudson.model.View.Create", "hudson.model.View.Delete",
		"com.cloudbees.plugins.credentials.CredentialsProvider.View",
		"com.cloudbees.plugins.credentials.CredentialsProvider.ManageDomains",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Create",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Update",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Delete",
	}
}

func operatorPermissions() []string {
	return []string{"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Build", "hudson.model.Item.Configure", "hudson.model.Item.Create", "hudson.model.Item.Delete", "hudson.model.Item.Discover", "hudson.model.Item.Workspace", "hudson.model.Run.Delete", "hudson.model.Run.Update", "hudson.model.View.Read", "hudson.model.View.Configure"}
}

func developerPermissions() []string {
	return []string{"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Build", "hudson.model.Item.Configure", "hudson.model.Item.Create", "hudson.model.Item.Discover", "hudson.model.Item.Workspace", "hudson.model.Run.Update", "hudson.model.View.Read"}
}

func viewerPermissions() []string {
	return []string{"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Discover", "hudson.model.View.Read"}
}
