package bus

import (
	"fmt"
	"os"
	"regexp"
)

// DefaultCluster is the cluster name used when VARROA_CLUSTER_NAME is unset (D7).
const DefaultCluster = "core"

// dns1123Label matches a valid DNS-1123 label (same constraint as ns/name tokens).
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ClusterFromEnv resolves this process's cluster identity from VARROA_CLUSTER_NAME.
// Empty/unset returns DefaultCluster. A value that is not a DNS-1123 label (or is longer
// than 63 chars) is an error — callers treat it as fatal at startup, because an
// invalid token would silently corrupt the NATS subject space.
func ClusterFromEnv() (string, error) {
	v := os.Getenv("VARROA_CLUSTER_NAME")
	if v == "" {
		return DefaultCluster, nil
	}
	if len(v) > 63 || !dns1123Label.MatchString(v) {
		return "", fmt.Errorf("VARROA_CLUSTER_NAME %q is not a DNS-1123 label", v)
	}
	return v, nil
}
