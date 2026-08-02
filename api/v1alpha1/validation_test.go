package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateJenkinsRole(t *testing.T) {
	tests := []struct {
		name    string
		role    *JenkinsRole
		wantErr string
	}{
		{
			name:    "nil role",
			role:    nil,
			wantErr: "nil",
		},
		{
			name: "empty name",
			role: &JenkinsRole{
				Spec: JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"hudson.model.Item.Read"}},
			},
			wantErr: "metadata.name",
		},
		{
			name: "invalid roleType",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:test"},
				Spec:       JenkinsRoleSpec{RoleType: "InvalidType", Permissions: []string{"hudson.model.Item.Read"}},
			},
			wantErr: "roleType",
		},
		{
			name: "empty roleType (defaults to Global)",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:test"},
				Spec:       JenkinsRoleSpec{Permissions: []string{"hudson.model.Item.Read"}},
			},
			wantErr: "", // empty roleType defaults to Global per CRD schema
		},
		{
			name: "empty permissions",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:test"},
				Spec:       JenkinsRoleSpec{RoleType: "Global"},
			},
			wantErr: "permissions",
		},
		{
			name: "empty permission string",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:test"},
				Spec:       JenkinsRoleSpec{RoleType: "Global", Permissions: []string{""}},
			},
			wantErr: "empty",
		},
		{
			name: "unqualified permission",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:test"},
				Spec:       JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Read"}},
			},
			wantErr: "qualified permission",
		},
		{
			name: "valid Global role",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:viewer"},
				Spec: JenkinsRoleSpec{
					RoleType:    "Global",
					Permissions: []string{"hudson.model.Hudson.Read", "hudson.model.Item.Read"},
				},
			},
			wantErr: "",
		},
		{
			name: "valid Item role",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:team-dev"},
				Spec: JenkinsRoleSpec{
					RoleType:    "Item",
					Permissions: []string{"hudson.model.Item.Build", "hudson.model.Item.Read"},
				},
			},
			wantErr: "",
		},
		{
			name: "valid Agent role",
			role: &JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa:agent-mgr"},
				Spec: JenkinsRoleSpec{
					RoleType:    "Agent",
					Permissions: []string{"hudson.model.Computer.Configure"},
				},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJenkinsRole(tt.role)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestValidateJenkinsRoleBinding(t *testing.T) {
	tests := []struct {
		name    string
		binding *JenkinsRoleBinding
		wantErr string
	}{
		{
			name:    "nil binding",
			binding: nil,
			wantErr: "nil",
		},
		{
			name: "empty name",
			binding: &JenkinsRoleBinding{
				Spec: JenkinsRoleBindingSpec{
					RoleRef:  "varroa:viewer",
					Subjects: []SubjectRef{{Kind: "Group", Name: "developers"}},
				},
			},
			wantErr: "metadata.name",
		},
		{
			name: "empty roleRef",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "test-binding"},
				Spec: JenkinsRoleBindingSpec{
					Subjects: []SubjectRef{{Kind: "Group", Name: "developers"}},
				},
			},
			wantErr: "roleRef",
		},
		{
			name: "empty subjects",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "test-binding"},
				Spec:       JenkinsRoleBindingSpec{RoleRef: "varroa:viewer"},
			},
			wantErr: "subjects",
		},
		{
			name: "empty subject name",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "test-binding"},
				Spec: JenkinsRoleBindingSpec{
					RoleRef:  "varroa:viewer",
					Subjects: []SubjectRef{{Kind: "Group"}},
				},
			},
			wantErr: "name",
		},
		{
			name: "invalid subject kind",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "test-binding"},
				Spec: JenkinsRoleBindingSpec{
					RoleRef:  "varroa:viewer",
					Subjects: []SubjectRef{{Kind: "ServiceAccount", Name: "default"}},
				},
			},
			wantErr: "kind",
		},
		{
			name: "valid binding with Global scope",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "dev-viewer-binding"},
				Spec: JenkinsRoleBindingSpec{
					RoleRef:      "varroa:viewer",
					Subjects:     []SubjectRef{{Kind: "Group", Name: "developers"}},
					JenkinsScope: &JenkinsScope{Type: "Global"},
				},
			},
			wantErr: "",
		},
		{
			name: "valid binding with Folder scope",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "team-folder-binding"},
				Spec: JenkinsRoleBindingSpec{
					RoleRef:      "varroa:developer",
					Subjects:     []SubjectRef{{Kind: "User", Name: "alice"}},
					JenkinsScope: &JenkinsScope{Type: "Folder", Folder: "team-a"},
				},
			},
			wantErr: "",
		},
		{
			name: "invalid scope type",
			binding: &JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-scope"},
				Spec: JenkinsRoleBindingSpec{
					RoleRef:      "varroa:viewer",
					Subjects:     []SubjectRef{{Kind: "Group", Name: "devs"}},
					JenkinsScope: &JenkinsScope{Type: "Cluster"},
				},
			},
			wantErr: "jenkinsScope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJenkinsRoleBinding(tt.binding)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
				}
			}
		})
	}
}
