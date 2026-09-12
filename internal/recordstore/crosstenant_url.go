package recordstore

import (
	"net/url"

	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// crossTenantRunURL builds a run's Varroa run page link (design.md D1's
// handle.url), the same relative "/pipelines/<team>/<job>/<run>" shape
// internal/api's search results already use. It answers "" for a tenant
// value rbac.ParseTenant rejects, rather than a malformed link: this
// field is documentation for a human reading the handle, never an
// authorization input.
func crossTenantRunURL(tenant, jobPath, runID string) string {
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil {
		return ""
	}
	team := parsed.Team
	if parsed.Kind == rbac.TenantController {
		team = parsed.Controller
	}
	return "/pipelines/" + url.PathEscape(team) + "/" + url.PathEscape(jobPath) + "/" + url.PathEscape(runID)
}
