package api

import (
	"net/url"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// ControllerAttention is the attention projection the dashboard pill reads.
// The MCP views read the same value, so the precedence lives in buildAttentionJSON only.
func ControllerAttention(cr *v1alpha1.Controller) *AttentionJSON {
	return buildAttentionJSON(cr)
}

// ControllerKind names a controller's role on the wire: "cell" or "stateful".
func ControllerKind(cr *v1alpha1.Controller) string {
	return controllerKind(cr)
}

// DashboardControllerPath is the dashboard path of one controller page.
func DashboardControllerPath(cluster, namespace, name string) string {
	return searchControllerLink(cluster, namespace, name)
}

// DashboardRunPath is the dashboard path of one build page. tenantName is the
// resolved route segment (TenantRef.TenantName), never the stored tenant value.
func DashboardRunPath(tenantName, jobPath, runID string) string {
	return searchRunLink(tenantName, jobPath, runID)
}

// DashboardTenantPath is the dashboard path of one tenant home.
func DashboardTenantPath(tenant string) string {
	return "/pipelines/" + url.PathEscape(tenant)
}
