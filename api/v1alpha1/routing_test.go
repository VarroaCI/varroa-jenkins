package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRoutingMode(t *testing.T) {
	tests := []struct {
		name     string
		spec     *IngressSpec
		expected string
	}{
		{"nil receiver", nil, RoutingModeSubdomain},
		{"empty mode", &IngressSpec{Mode: ""}, RoutingModeSubdomain},
		{"subdomain mode", &IngressSpec{Mode: "subdomain"}, RoutingModeSubdomain},
		{"path mode", &IngressSpec{Mode: "path"}, RoutingModePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.RoutingMode(); got != tt.expected {
				t.Errorf("RoutingMode() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestPathPrefix(t *testing.T) {
	got := PathPrefix("team-a", "ci")
	want := "/jenkins/team-a/ci"
	if got != want {
		t.Errorf("PathPrefix() = %q, want %q", got, want)
	}
}

func TestResolveHost(t *testing.T) {
	tests := []struct {
		name string
		cr   *Controller
		root string
		want string
	}{
		{"explicit host", &Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci"}, Spec: ControllerSpec{IngressSpec: &IngressSpec{Host: "jenkins.example"}}}, "root.example", "jenkins.example"},
		{"subdomain derived", &Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci"}}, "root.example", "ci.root.example"},
		{"path without host", &Controller{ObjectMeta: metav1.ObjectMeta{Name: "ci"}, Spec: ControllerSpec{IngressSpec: &IngressSpec{Mode: RoutingModePath}}}, "root.example", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveHost(tt.cr, tt.root); got != tt.want {
				t.Fatalf("ResolveHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeStringMaps(t *testing.T) {
	tests := []struct {
		name     string
		base     map[string]string
		override map[string]string
		want     map[string]string
	}{
		{
			name:     "both nil",
			base:     nil,
			override: nil,
			want:     nil,
		},
		{
			name:     "base nil, override set",
			base:     nil,
			override: map[string]string{"a": "1"},
			want:     map[string]string{"a": "1"},
		},
		{
			name:     "base set, override nil",
			base:     map[string]string{"b": "2"},
			override: nil,
			want:     map[string]string{"b": "2"},
		},
		{
			name:     "key conflict — override wins",
			base:     map[string]string{"x": "base", "y": "base"},
			override: map[string]string{"x": "override"},
			want:     map[string]string{"x": "override", "y": "base"},
		},
		{
			name:     "disjoint keys — union",
			base:     map[string]string{"a": "1", "b": "2"},
			override: map[string]string{"c": "3"},
			want:     map[string]string{"a": "1", "b": "2", "c": "3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeStringMaps(tt.base, tt.override)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("MergeStringMaps() = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("MergeStringMaps() key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
