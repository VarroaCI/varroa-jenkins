package activity

import (
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// TestScopeFromPermissions_FourShapes covers the four real binding shapes
// VarroaRoleBindingScope can produce: a nil scope (cluster-wide), a
// Namespaces list, a ControllerSelector, and no binding at all. There is no
// binding that names one controller directly, so that shape is not covered.
func TestScopeFromPermissions_FourShapes(t *testing.T) {
	tests := []struct {
		name          string
		perms         rbac.PermissionScopes
		canReadGlobal bool
		want          Scope
	}{
		{
			name:          "nil scope (cluster-wide binding)",
			perms:         rbac.PermissionScopes{Global: rbac.APICapabilitySet{"controllers": {"read": true}}},
			canReadGlobal: true,
			want:          Scope{All: true, Global: true},
		},
		{
			name: "Namespaces list binding",
			perms: rbac.PermissionScopes{Scopes: []rbac.ScopedCapabilities{
				{Namespaces: []string{"ns-a"}, Capabilities: rbac.APICapabilitySet{"controllers": {"read": true}}},
			}},
			canReadGlobal: false,
			want:          Scope{Namespaces: []string{"ns-a"}},
		},
		{
			name: "ControllerSelector binding falls back to All",
			perms: rbac.PermissionScopes{Scopes: []rbac.ScopedCapabilities{
				{HasControllerSelector: true, Capabilities: rbac.APICapabilitySet{"controllers": {"read": true}}},
			}},
			canReadGlobal: false,
			want:          Scope{All: true},
		},
		{
			name:          "no binding at all",
			perms:         rbac.PermissionScopes{},
			canReadGlobal: false,
			want:          Scope{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScopeFromPermissions(tt.perms, tt.canReadGlobal)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ScopeFromPermissions() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestScope_Admits exercises the Go-side mirror of the SQL clause a reader
// pushes down, so a divergence between the two shows up here first.
func TestScope_Admits(t *testing.T) {
	all := Scope{All: true}
	if !all.Admits(Event{Namespace: "anything", Controller: "anything"}) {
		t.Error("All scope must admit every row")
	}

	nsScoped := Scope{Namespaces: []string{"ns-a"}}
	if !nsScoped.Admits(Event{Namespace: "ns-a", Controller: "ctrl"}) {
		t.Error("namespace-scoped Scope must admit its own namespace")
	}
	if nsScoped.Admits(Event{Namespace: "ns-b", Controller: "ctrl"}) {
		t.Error("namespace-scoped Scope must deny a different namespace")
	}
	if nsScoped.Admits(Event{Namespace: "", Controller: ""}) {
		t.Error("namespace-scoped Scope with Global=false must deny a global event")
	}

	globalOnly := Scope{Global: true}
	if !globalOnly.Admits(Event{Namespace: "", Controller: ""}) {
		t.Error("Global scope must admit a global event")
	}
	if globalOnly.Admits(Event{Namespace: "ns-a", Controller: "ctrl"}) {
		t.Error("Global-only scope must deny a controller-scoped event")
	}

	if (Scope{}).Admits(Event{Namespace: "ns-a", Controller: "ctrl"}) {
		t.Error("the zero-value Scope must deny by default")
	}
}
