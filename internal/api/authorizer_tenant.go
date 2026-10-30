package api

import (
	"regexp"
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
//
// tenantGrants holds steps 1, 2, and 4, and grantsAllow holds steps 3 and
// 5. Step 4 therefore runs before step 3. The result is the same: steps 1,
// 2, and 3 are pure denials with no side effect, and step 4 only reads
// grants.
func (a *Authorizer) canAccessPipeline(claims *auth.Claims, tenant, folder, jobPath, verb string) bool {
	grants, ok := a.tenantGrants(claims, tenant, folder)
	if !ok {
		return false
	}
	return grantsAllow(grants, folder, jobPath, verb)
}

// tenantGrants carries decision steps 1, 2, and 4: nil claims deny, the
// tenant value must parse, its parsed folder must equal folder, and the
// resolver then answers every grant the caller holds on that tenant. The
// bool is false when one of the three denies.
//
// A listing resolves the grants once per tenant and then tests each row
// with grantsAllow, instead of resolving them once per row. A nil receiver
// denies before this method reads a field, so the listing keeps the
// nil-authorizer rule.
func (a *Authorizer) tenantGrants(claims *auth.Claims, tenant, folder string) ([]rbac.TenantGrant, bool) {
	if a == nil || claims == nil {
		return nil, false
	}
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil || parsed.Folder != folder {
		return nil, false
	}
	return a.resolver.EffectiveTenantCapabilities(claims, parsed), true
}

// grantsAllow carries decision steps 3 and 5: a non-empty jobPath must lie
// inside folder, and one grant, tested alone, must hold verb on "pipelines"
// and either carry no globs, or meet an empty jobPath, or match the
// folder-relative job path. Grants are never unioned: the verb and the glob
// match must come from the same entry.
func grantsAllow(grants []rbac.TenantGrant, folder, jobPath, verb string) bool {
	if jobPath != "" && !withinFolder(folder, jobPath) {
		return false
	}

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

// TenantGrantSources answers which grants reach a tenant. It is the reverse
// of the tenant checks above: it names no caller, and it decides nothing.
// The route that serves it applies its own two gates.
//
// The bool reports whether the grant cap dropped a grant of this tenant.
func (a *Authorizer) TenantGrantSources(tenant string) ([]rbac.TenantGrantSource, bool) {
	if a == nil || a.resolver == nil {
		return nil, false
	}
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil {
		return nil, false
	}
	return a.resolver.TenantGrantSourcesTruncated(parsed)
}

// TenantMemberRoles answers which generated Jenkins member roles a tenant
// holds. It names no caller and is never capped.
func (a *Authorizer) TenantMemberRoles(tenant string) []rbac.TenantMemberRole {
	if a == nil || a.resolver == nil {
		return nil
	}
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil {
		return nil
	}
	return a.resolver.TenantMemberRoles(parsed)
}

// PipelineReadJobPatterns answers the anchored job-path regexps that the
// fleet run reads push into SQL. It holds decision steps 1, 2, and 4 for
// the read verb, and it reads each grant alone, as grantsAllow does.
//
// The bool is false when no grant of the caller holds pipelines read on the
// tenant. A nil slice with true means one read grant has no glob restriction,
// so every job of the tenant is readable. Otherwise the slice is the union of
// the read grants' patterns and never mixes in a write grant.
func (a *Authorizer) PipelineReadJobPatterns(claims *auth.Claims, ref TenantRef) ([]string, bool) {
	grants, ok := a.tenantGrants(claims, ref.Tenant, ref.Folder)
	if !ok {
		return nil, false
	}
	patterns := []string{}
	found := false
	for _, grant := range grants {
		if !hasVerb(grant.Capabilities, "pipelines", "read") {
			continue
		}
		found = true
		if grant.Globs == nil {
			return nil, true
		}
		patterns = append(patterns, jobPatternRegexps(ref.Folder, grant.Globs)...)
	}
	if !found {
		return nil, false
	}
	return patterns, true
}

// jobPatternRegexps translates each folder-relative glob into an anchored
// regular expression over the full job path. The Postgres operator "~"
// evaluates these expressions, so they use only the forms that
// jobglob.RegexpBody emits plus regexp.QuoteMeta output and "^", "$", "(?:",
// and ")". A nil globs answers nil.
//
// A glob that matches the empty folder-relative path also admits the folder
// itself. grantsAllow admits that path, so the patterns must too.
func jobPatternRegexps(folder string, globs []string) []string {
	if globs == nil {
		return nil
	}
	out := make([]string, 0, len(globs)+1)
	admitsFolder := false
	quoted := regexp.QuoteMeta(folder)
	for _, glob := range globs {
		body := jobglob.RegexpBody(glob)
		if folder == "" {
			out = append(out, "^(?:"+body+")$")
		} else {
			out = append(out, "^"+quoted+"/(?:"+body+")$")
		}
		if jobglob.Match(glob, "") {
			admitsFolder = true
		}
	}
	if folder != "" && admitsFolder {
		out = append(out, "^"+quoted+"$")
	}
	return out
}
