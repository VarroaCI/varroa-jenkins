export interface CapabilitySet {
  [resource: string]: { [verb: string]: boolean };
}

/** One pipelines tenant name and the folder-relative job globs a scope covers.
 * An absent `jobs` list covers every job. */
export interface TenantScope {
  name: string;
  jobs?: string[];
}

export interface ScopedCapabilities {
  namespaces: string[];
  hasControllerSelector: boolean;
  /** The scope's pipelines-tenant allowlist. Absent or empty means the scope
   * names no tenant. */
  tenants?: TenantScope[];
  capabilities: CapabilitySet;
}

/** One grant the caller holds on one pipelines tenant. A tenant name may
 * appear in more than one entry, because the caller can hold two grants on
 * it with different job globs; entries are never merged. */
export interface TenantCapabilities {
  tenant: string;
  /** Folder-relative job globs the grant covers. Empty means every job. */
  jobs: string[];
  capabilities: CapabilitySet;
}

export interface Permissions {
  global: CapabilitySet;
  scopes: ScopedCapabilities[];
  /** One entry per pipelines-tenant grant the caller holds. This field can
   * be absent. Treat an absent value as an empty array. */
  tenants?: TenantCapabilities[];
}

/** Shape of GET /api/v1/me — the current authenticated user. */
export interface MeResponse {
  subject: string;
  preferredUsername?: string;
  email: string;
  name: string;
  groups: string[];
  displayName?: string;
  preferences?: {
    theme?: string;
    accent?: string;
    defaultLanding?: string;
  };
  /** Source of this session. A local account beside OIDC reports "local". */
  authMode?: "oidc" | "local" | "ldap";
  lastLogin?: string;
}

/** Shape of GET /api/v1/auth-config (unauthenticated). */
export interface AuthConfig {
  mode: "oidc" | "local" | "ldap";
  /** True when declared local accounts sign in beside OIDC. */
  localAccounts: boolean;
}
