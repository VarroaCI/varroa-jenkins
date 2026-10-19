package auth

// PrincipalName returns the Jenkins principal of a human caller. It returns the
// first non-empty value of preferred username, email, then subject.
// The order is the principal a cell acting user carries. The operator member
// role uses the same order, so keep both in step.
func PrincipalName(preferredUsername, email, subject string) string {
	for _, v := range []string{preferredUsername, email, subject} {
		if v != "" {
			return v
		}
	}
	return ""
}
