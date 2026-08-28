package main

import (
	"fmt"
	"strings"
)

// ErrUnrecognizedScheme is returned by ParseOCIDest when the scheme is not one of
// the recognized OCI destination schemes (oci://, dir://, tar://, uc://).
type ErrUnrecognizedScheme struct {
	Scheme string
}

func (e *ErrUnrecognizedScheme) Error() string {
	return fmt.Sprintf("unrecognized scheme %q: expected oci://, dir://, tar://, or uc://", e.Scheme)
}

// ParseOCIDest parses an OCI destination string into its scheme and target parts.
// Recognized schemes (case-sensitive): oci://, dir://, tar://, uc://.
// If the scheme is not recognized, it returns an *ErrUnrecognizedScheme.
func ParseOCIDest(s string) (scheme, target string, err error) {
	idx := strings.Index(s, "://")
	if idx < 0 {
		return "", "", &ErrUnrecognizedScheme{Scheme: ""}
	}
	scheme = s[:idx]
	switch scheme {
	case "oci", "dir", "tar", "uc":
		return scheme, s[idx+3:], nil
	default:
		return "", "", &ErrUnrecognizedScheme{Scheme: scheme}
	}
}
