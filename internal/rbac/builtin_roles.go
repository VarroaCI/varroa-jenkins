package rbac

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

var builtinJenkinsRoleDefinitions = []v1alpha1.JenkinsRole{
	buildBuiltinJenkinsRole("varroa-admin", []string{
		"hudson.model.Hudson.Administer",
		"hudson.model.Hudson.Read",
		"hudson.model.Item.Read",
		"hudson.model.Item.Build",
		"hudson.model.Item.Configure",
		"hudson.model.Item.Create",
		"hudson.model.Item.Delete",
		"hudson.model.Item.Discover",
		"hudson.model.Item.Workspace",
		"hudson.model.Run.Delete",
		"hudson.model.Run.Update",
		"hudson.model.View.Read",
		"hudson.model.View.Configure",
		"hudson.model.View.Create",
		"hudson.model.View.Delete",
		"com.cloudbees.plugins.credentials.CredentialsProvider.View",
		"com.cloudbees.plugins.credentials.CredentialsProvider.ManageDomains",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Create",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Update",
		"com.cloudbees.plugins.credentials.CredentialsProvider.Delete",
	}),
	buildBuiltinJenkinsRole("varroa-operator", []string{
		"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Build",
		"hudson.model.Item.Configure", "hudson.model.Item.Create", "hudson.model.Item.Delete",
		"hudson.model.Item.Discover", "hudson.model.Item.Workspace", "hudson.model.Run.Delete",
		"hudson.model.Run.Update", "hudson.model.View.Read", "hudson.model.View.Configure",
	}),
	buildBuiltinJenkinsRole("varroa-developer", []string{
		"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Build",
		"hudson.model.Item.Configure", "hudson.model.Item.Create", "hudson.model.Item.Discover",
		"hudson.model.Item.Workspace", "hudson.model.Run.Update", "hudson.model.View.Read",
	}),
	buildBuiltinJenkinsRole("varroa-viewer", []string{
		"hudson.model.Hudson.Read", "hudson.model.Item.Read", "hudson.model.Item.Discover",
		"hudson.model.View.Read",
	}),
	buildBuiltinJenkinsRole("varroa-system-mite", MiteMinimalPermissions()),
	buildBuiltinJenkinsRole("varroa-system-operator", SystemOperatorPermissions()),
}

// BuiltinJenkinsRoles returns deep copies of the canonical built-in roles.
func BuiltinJenkinsRoles() []*v1alpha1.JenkinsRole {
	roles := make([]*v1alpha1.JenkinsRole, len(builtinJenkinsRoleDefinitions))
	for i := range builtinJenkinsRoleDefinitions {
		role := builtinJenkinsRoleDefinitions[i].DeepCopy()
		roles[i] = role
	}
	return roles
}

func buildBuiltinJenkinsRole(name string, permissions []string) v1alpha1.JenkinsRole {
	return v1alpha1.JenkinsRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1alpha1.LabelBuiltin: "true"},
		},
		Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: permissions},
	}
}
