package rbac

import "testing"

// TestMiteMinimalPermissionsExcludesAdmin pins the outcome: the mite role
// must never carry Hudson.Administer, and must include the MANAGE-based set it
// needs to apply config via reload and read system config.
func TestMiteMinimalPermissionsExcludesAdmin(t *testing.T) {
	perms := MiteMinimalPermissions()

	for _, p := range perms {
		if p == "hudson.model.Hudson.Administer" {
			t.Fatalf("mite role must not contain Hudson.Administer, got: %v", perms)
		}
	}

	required := []string{
		"hudson.model.Hudson.Manage",
		"hudson.model.Hudson.SystemRead",
		"hudson.model.Hudson.Read",
		"hudson.model.Item.Create",
		"hudson.model.Item.Configure",
		"hudson.model.View.Read",
	}
	for _, want := range required {
		found := false
		for _, p := range perms {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mite role missing required permission %q; got: %v", want, perms)
		}
	}
}
