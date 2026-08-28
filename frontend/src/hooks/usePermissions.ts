import { useQuery } from "@tanstack/react-query";
import { getMyPermissions } from "../api/client";
import type { Permissions, CapabilitySet } from "../types/auth";

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
 * global OR any scope — legacy union; behavior-preserving for namespace-relevant gates.
 */
export function canDoAnywhere(perms: Permissions | undefined, resource: string, verb: string): boolean {
  if (matches(perms?.global, resource, verb)) return true;
  return !!perms?.scopes?.some((s) => matches(s.capabilities, resource, verb));
}

/**
 * global OR a scope covering ns (selector scopes are opaque → permissive to avoid
 * hiding a button the caller can actually use; the server still enforces).
 */
export function canDoInNamespace(perms: Permissions | undefined, ns: string, resource: string, verb: string): boolean {
  if (matches(perms?.global, resource, verb)) return true;
  return !!perms?.scopes?.some(
    (s) => matches(s.capabilities, resource, verb) &&
           (s.hasControllerSelector || (s.namespaces?.length ?? 0) === 0 || s.namespaces.includes(ns))
  );
}
