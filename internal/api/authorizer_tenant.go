package api

import (
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/rbac/jobglob"
)

// CanReadPipeline reports whether the caller may read a tenant's jobs and
// runs. An empty jobPath asks about the tenant as a whole.
func (a *Authorizer) CanReadPipeline(claims *auth.Claims, ref TenantRef, jobPath string) bool {
	if a == nil {
		return false
	}
	return a.canAccessPipeline(claims, ref.Tenant, ref.Folder, jobPath, "read")
}

// CanWritePipeline reports whether the caller may start, cancel, or rerun a
// tenant's builds.
func (a *Authorizer) CanWritePipeline(claims *auth.Claims, ref TenantRef, jobPath string) bool {
	if a == nil {
		return false
	}
	return a.canAccessPipeline(claims, ref.Tenant, ref.Folder, jobPath, "write")
}

// CanReadRun reports whether the caller may read one stored run. The row's
// own tenant value supplies its folder, so no separate consistency check
// applies to it the way it does for a caller-supplied TenantRef.
func (a *Authorizer) CanReadRun(claims *auth.Claims, row RunTenantRow) bool {
	if a == nil {
		return false
	}
	parsed, err := rbac.ParseTenant(row.Tenant)
	if err != nil {
		return false
	}
	return a.canAccessPipeline(claims, row.Tenant, parsed.Folder, row.JobPath, "read")
}

// canAccessPipeline carries the five decision steps of design D6:
//
//  1. Nil claims deny. Default-read mode grants controllers:read only, and a
//     build log is not controller inventory.
//  2. tenant must parse, and the parsed folder must equal folder. A
//     caller-supplied reference that disagrees with itself decides nothing.
//  3. A non-empty jobPath must lie inside the tenant's folder. An empty
//     folder prefixes every path.
//  4. Resolve every grant the caller holds on the parsed tenant.
//  5. Allow when one grant, tested alone, holds verb on "pipelines" and
//     either carries no globs, or jobPath is empty, or one glob matches the
//     folder-relative job path. Grants are never unioned: the verb and the
//     glob match must come from the same entry.
func (a *Authorizer) canAccessPipeline(claims *auth.Claims, tenant, folder, jobPath, verb string) bool {
	if a == nil {
		return false
	}
	if claims == nil {
		return false
	}

	parsed, err := rbac.ParseTenant(tenant)
	if err != nil || parsed.Folder != folder {
		return false
	}

	if jobPath != "" && !withinFolder(folder, jobPath) {
		return false
	}

	grants := a.resolver.EffectiveTenantCapabilities(claims, parsed)

	relPath := stripFolder(folder, jobPath)
	for _, grant := range grants {
		if !hasVerb(grant.Capabilities, "pipelines", verb) {
			continue
		}
		if grant.Globs == nil || jobPath == "" {
			return true
		}
		for _, pattern := range grant.Globs {
			if jobglob.Match(pattern, relPath) {
				return true
			}
		}
	}
	return false
}

// withinFolder reports whether jobPath lies inside folder. An empty folder
// prefixes every path. A non-empty folder requires jobPath to equal it or
// to start with "<folder>/".
func withinFolder(folder, jobPath string) bool {
	if folder == "" || jobPath == folder {
		return true
	}
	return strings.HasPrefix(jobPath, folder+"/")
}

// stripFolder returns the folder-relative remainder of jobPath. The caller
// has already confirmed jobPath lies inside folder.
func stripFolder(folder, jobPath string) string {
	if folder == "" || jobPath == folder {
		return strings.TrimPrefix(jobPath, folder)
	}
	return strings.TrimPrefix(jobPath, folder+"/")
}
