package rbac

import "testing"

func TestFederatedCopyMessage(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		labels      map[string]string
		annotations map[string]string
		want        string
	}{
		{"nil labels", "JenkinsRole", nil, nil, ""},
		{"label missing", "JenkinsRole", map[string]string{"x": "y"}, nil, ""},
		{"label empty", "JenkinsRoleBinding", map[string]string{LabelFederatedFrom: ""}, nil, ""},
		{
			name:        "role with roles annotation",
			kind:        "JenkinsRole",
			labels:      map[string]string{LabelFederatedFrom: "alpha"},
			annotations: map[string]string{AnnotationFederatedRoles: "alpha,zeta"},
			want:        "JenkinsRole copy-1 is a federated copy of VarroaRole alpha, zeta. Federation replaces each change to the copy. Change the source on the core cluster.",
		},
		{
			name:   "role without annotation names the label",
			kind:   "JenkinsRole",
			labels: map[string]string{LabelFederatedFrom: "tenant-admin"},
			want:   "JenkinsRole copy-1 is a federated copy of VarroaRole tenant-admin. Federation replaces each change to the copy. Change the source on the core cluster.",
		},
		{
			name:        "binding with bindings annotation",
			kind:        "JenkinsRoleBinding",
			labels:      map[string]string{LabelFederatedFrom: "writer"},
			annotations: map[string]string{AnnotationFederatedBindings: "bind-a,bind-b"},
			want:        "JenkinsRoleBinding copy-1 is a federated copy of VarroaRoleBinding bind-a, bind-b. Federation replaces each change to the copy. Change the source on the core cluster.",
		},
		{
			name:   "binding without annotation names the role",
			kind:   "JenkinsRoleBinding",
			labels: map[string]string{LabelFederatedFrom: "writer"},
			want:   "JenkinsRoleBinding copy-1 is a federated copy of VarroaRole writer. Federation replaces each change to the copy. Change the source on the core cluster.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FederatedCopyMessage(tc.kind, "copy-1", tc.labels, tc.annotations); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFederatedLabelMessage(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		labels map[string]string
		want   string
	}{
		{"nil labels", "JenkinsRole", nil, ""},
		{"label missing", "JenkinsRole", map[string]string{"x": "y"}, ""},
		{"label empty", "JenkinsRoleBinding", map[string]string{LabelFederatedFrom: ""}, ""},
		{
			name:   "role sets label",
			kind:   "JenkinsRole",
			labels: map[string]string{LabelFederatedFrom: "alpha"},
			want:   "The JenkinsRole request sets the label varroa.dev/federated-from. The label marks a federated copy, and only federation writes it. Remove the label from the request.",
		},
		{
			name:   "binding sets label",
			kind:   "JenkinsRoleBinding",
			labels: map[string]string{LabelFederatedFrom: "alpha"},
			want:   "The JenkinsRoleBinding request sets the label varroa.dev/federated-from. The label marks a federated copy, and only federation writes it. Remove the label from the request.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FederatedLabelMessage(tc.kind, tc.labels); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
