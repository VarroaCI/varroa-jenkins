import { canDoAnywhere, canDoGlobal } from "../hooks/usePermissions";
import type { Permissions } from "../types/auth";

/**
 * Whether the user can see the Catalog door — true if they have read
 * permission on any of the catalog resource types in any scope.
 */
export function canCatalogArea(perms: Permissions | undefined): boolean {
  return (
    canDoAnywhere(perms, "catalogsources", "read") ||
    canDoAnywhere(perms, "catalogitems", "read") ||
    canDoAnywhere(perms, "composedbundles", "read")
  );
}

/**
 * Whether the user can see the Admin & access door — true if they have
 * global admin or read on any admin-resource type, or can update
 * provisioning defaults.
 */
export function canAdminArea(perms: Permissions | undefined): boolean {
  return (
    canDoGlobal(perms, "*", "*") ||
    canDoGlobal(perms, "roles", "read") ||
    canDoGlobal(perms, "rolebindings", "read") ||
    canDoGlobal(perms, "jenkinsroles", "read") ||
    canDoGlobal(perms, "jenkinsrolebindings", "read") ||
    canDoGlobal(perms, "provisioningdefaults", "update")
  );
}
