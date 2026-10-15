import { useQuery } from "@tanstack/react-query";
import { getMyPermissions } from "../api/client";
import { jobGlobMatches } from "../lib/jobglob";
import type { Permissions, CapabilitySet, ScopedCapabilities, TenantCapabilities } from "../types/auth";

export function usePermissions(enabled: boolean = true) {
  return useQuery<Permissions>({
    queryKey: ["permissions"],
    queryFn: getMyPermissions,
    staleTime: 60_000,
    refetchInterval: 60_000,
    enabled,
  });
}

function matches(set: CapabilitySet | undefined, resource: string, verb: string): boolean {
  if (!set) return false;
  if (set["*"]?.["*"] || set["*"]?.[verb]) return true;
  if (set[resource]?.["*"] || set[resource]?.[verb]) return true;
  return false;
}

/**
 * cluster-wide only — for global-admin gates.
 */
export function canDoGlobal(perms: Permissions | undefined, resource: string, verb: string): boolean {
  return matches(perms?.global, resource, verb);
}

/**
 * A non-tenant gate excludes a `scopes` entry whose `tenants` list is
 * non-empty: that entry decides tenant grants only, through the `tenants`
 * partition, never a global or namespace-scoped check.
 */
function isNonTenantScope(s: ScopedCapabilities): boolean {
  return (s.tenants?.length ?? 0) === 0;
}

/**
 * global OR any non-tenant scope — legacy union; behavior-preserving for
 * namespace-relevant gates. A `scopes` entry whose `tenants` list is
 * non-empty is excluded: it contributes to no gate other than the tenant
 * gate.
 */
export function canDoAnywhere(perms: Permissions | undefined, resource: string, verb: string): boolean {
  if (matches(perms?.global, resource, verb)) return true;
  return !!perms?.scopes?.some((s) => isNonTenantScope(s) && matches(s.capabilities, resource, verb));
}

/**
 * global OR a non-tenant scope covering ns (selector scopes are opaque →
 * permissive to avoid hiding a button the caller can actually use; the
 * server still enforces). A `scopes` entry whose `tenants` list is
 * non-empty is excluded: it contributes to no gate other than the tenant
 * gate.
 */
export function canDoInNamespace(perms: Permissions | undefined, ns: string, resource: string, verb: string): boolean {
  if (matches(perms?.global, resource, verb)) return true;
  return !!perms?.scopes?.some(
    (s) => isNonTenantScope(s) &&
           matches(s.capabilities, resource, verb) &&
           (s.hasControllerSelector || (s.namespaces?.length ?? 0) === 0 || s.namespaces.includes(ns))
  );
}

const PIPELINES_VERBS = ["read", "write", "admin"] as const;

function matchesAnyPipelinesVerb(set: CapabilitySet | undefined): boolean {
  return PIPELINES_VERBS.some((verb) => matches(set, "pipelines", verb));
}

/**
 * canSeePipelines reports whether the Pipelines navigation entry may render:
 * true when `tenants` holds at least one entry, or when `global` or a
 * `scopes` entry grants a pipelines verb.
 */
export function canSeePipelines(perms: Permissions | undefined): boolean {
  if ((perms?.tenants?.length ?? 0) > 0) return true;
  if (matchesAnyPipelinesVerb(perms?.global)) return true;
  return !!perms?.scopes?.some((s) => isNonTenantScope(s) && matchesAnyPipelinesVerb(s.capabilities));
}

/** tenantEntryGrants reports whether one `TenantCapabilities` entry, tested
 * alone, admits the verb and job path. */
function tenantEntryGrants(entry: TenantCapabilities, verb: string, jobPath: string | undefined): boolean {
  if (!matches(entry.capabilities, "pipelines", verb)) return false;
  if (entry.jobs.length === 0 || jobPath === undefined) return true;
  return entry.jobs.some((pattern) => jobGlobMatches(pattern, jobPath));
}

/**
 * parseSyntheticNamespace returns the namespace of a synthetic
 * (single-controller) tenant value shaped "<namespace>/<controller>", or
 * undefined for a Team tenant (no "/") or a malformed value.
 */
function parseSyntheticNamespace(tenant: string): string | undefined {
  const idx = tenant.indexOf("/");
  if (idx <= 0 || idx === tenant.length - 1) return undefined;
  if (tenant.indexOf("/", idx + 1) !== -1) return undefined;
  return tenant.slice(0, idx);
}

/**
 * canAccessTenantPipeline tests one tenant's `tenants` entries **one at a
 * time** and never unions them: the verb and a job-path match must come
 * from the same entry. `tenant` is a Team name, or a synthetic
 * "<namespace>/<controller>" value.
 *
 * `jobPath`, when given, is folder-relative: the caller strips the tenant's
 * folder before calling, the same way the server does. The server's
 * `stripFolder` (`internal/api/authorizer_tenant.go`) trims the tenant
 * folder from a job path before matching it against a grant's globs, so a
 * caller-supplied `jobPath` here must already be relative to that folder,
 * not the full job path.
 *
 * `tenants` enumerates Team tenants only, so a synthetic tenant never has an
 * entry. For such a tenant this falls back to `global` plus a `scopes`
 * entry whose `namespaces` allowlist holds the tenant's namespace,
 * permissively for a controller-selector or namespace-unbounded scope. A
 * Team tenant with no entry holds no grant: the partition already
 * enumerated it and found nothing.
 */
export function canAccessTenantPipeline(
  perms: Permissions | undefined,
  tenant: string,
  verb: string,
  jobPath?: string,
): boolean {
  const entries = perms?.tenants?.filter((t) => t.tenant === tenant) ?? [];
  if (entries.length > 0) {
    return entries.some((entry) => tenantEntryGrants(entry, verb, jobPath));
  }

  const ns = parseSyntheticNamespace(tenant);
  if (ns === undefined) return false;

  if (matches(perms?.global, "pipelines", verb)) return true;
  return !!perms?.scopes?.some(
    (s) =>
      isNonTenantScope(s) &&
      matches(s.capabilities, "pipelines", verb) &&
      (s.hasControllerSelector || (s.namespaces?.length ?? 0) === 0 || s.namespaces.includes(ns)),
  );
}
