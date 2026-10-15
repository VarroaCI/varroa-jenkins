package api

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// BuildJenkinsServiceURL builds a Controller's in-cluster Jenkins Service
// base URL. The Service name carries the first eight characters of the
// Controller's UID, so the URL cannot be derived from name and namespace
// alone: <name>-<uid[:8]>-svc.<namespace>.svc.cluster.local:8080.
//
// This helper lives here, not in internal/mite/proxy, because the agent
// WebSocket door (design D7, internal/api) needs it and internal/mcp
// already imports internal/api; moving it the other way would cycle.
func BuildJenkinsServiceURL(cr *v1alpha1.Controller) string {
	uid := string(cr.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	prefix := cr.Name + "-" + uid
	return fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", prefix, cr.Namespace)
}
