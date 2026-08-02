export interface CapabilitySet {
  [resource: string]: { [verb: string]: boolean };
}

export interface ScopedCapabilities {
  namespaces: string[];
  hasControllerSelector: boolean;
  capabilities: CapabilitySet;
}

export interface Permissions {
  global: CapabilitySet;
  scopes: ScopedCapabilities[];
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
  authMode?: "oidc" | "local" | "ldap";
  lastLogin?: string;
}

/** Shape of GET /api/v1/auth-config (unauthenticated). */
export interface AuthConfig {
  mode: "oidc" | "local" | "ldap";
}
