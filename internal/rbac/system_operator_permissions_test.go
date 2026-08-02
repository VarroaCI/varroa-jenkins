package rbac

import (
	"testing"
)

func TestSystemOperatorPermissions(t *testing.T) {
	perms := SystemOperatorPermissions()
	expected := []string{"hudson.model.Hudson.Administer"}
	if len(perms) != len(expected) {
		t.Fatalf("expected %d permission(s), got %d: %v", len(expected), len(perms), perms)
	}
	for i, p := range expected {
		if perms[i] != p {
			t.Errorf("perms[%d] = %s, want %s", i, perms[i], p)
		}
	}
}
