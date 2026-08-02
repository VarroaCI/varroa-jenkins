package rbac

// SystemOperatorPermissions returns the Jenkins permission set for the
// varroa:system-operator role, used by the operator's direct-to-Jenkins
// executeGroovy identity (system:varroa-operator). hudson.model.Hudson.Administer
// is the only permission Jenkins accepts for /scriptText (the script console) —
// there is no narrower permission that authorizes it. This is deliberately the
// only built-in role carrying Administer; the risk reduction for executeGroovy
// comes from the short-lived (2-minute), per-dispatch token lifecycle in
// internal/mite/jenkinstoken.go, not from limiting this permission set.
func SystemOperatorPermissions() []string {
	return []string{
		"hudson.model.Hudson.Administer",
	}
}
