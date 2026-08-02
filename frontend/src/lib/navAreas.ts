import { Cable, Fingerprint, GitBranch, GitCompareArrows, Link2, PackageCheck, PackageSearch, Boxes, Settings2, Shield, Users, Users2, UsersRound, Wrench, type LucideIcon } from "lucide-react";
import { canDoAnywhere, canDoGlobal } from "../hooks/usePermissions";
import type { Permissions } from "../types/auth";

export interface AreaNavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  description: string;
  gate: "admin" | { resource: string; verb: string; globalOnly?: boolean };
  end?: boolean;
}

export const SETTINGS_ITEMS: AreaNavItem[] = [
  { to: "/access/users", label: "Users", icon: Users, description: "Manage dashboard user accounts", gate: "admin" },
  { to: "/access/groups", label: "Groups", icon: UsersRound, description: "Manage user groups", gate: "admin" },
  { to: "/access/builtin-roles", label: "Built-in Roles", icon: Shield, description: "View the built-in Varroa roles", gate: "admin" },
  { to: "/access/roles", label: "Varroa Roles", icon: Shield, description: "Define Varroa API permissions", gate: { resource: "roles", verb: "read", globalOnly: true } },
  { to: "/access/role-bindings", label: "Varroa Role Bindings", icon: Link2, description: "Bind Varroa roles to subjects", gate: { resource: "rolebindings", verb: "read", globalOnly: true } },
  { to: "/access/jenkins-roles", label: "Jenkins Roles", icon: Wrench, description: "Jenkins permission sets (Global/Item/Agent)", gate: { resource: "jenkinsroles", verb: "read", globalOnly: true } },
  { to: "/access/jenkins-role-bindings", label: "Jenkins Role Bindings", icon: Cable, description: "Assign Jenkins roles to subjects", gate: { resource: "jenkinsrolebindings", verb: "read", globalOnly: true } },
  { to: "/access/teams", label: "Teams", icon: Users2, description: "Team-scoped access and publishing", gate: { resource: "roles", verb: "read", globalOnly: true } },
  { to: "/administration/provisioning", label: "Provisioning", icon: Settings2, description: "Cluster provisioning defaults", gate: { resource: "provisioningdefaults", verb: "update", globalOnly: true } },
  { to: "/administration/versions", label: "Versions", icon: GitCompareArrows, description: "Jenkins version profiles and channels", gate: { resource: "provisioningdefaults", verb: "update", globalOnly: true } },
  { to: "/administration/identity", label: "Identity", icon: Fingerprint, description: "OIDC identity provider settings", gate: "admin" },
  { to: "/administration/update-center", label: "Update Center", icon: PackageSearch, description: "Update center status and plugin inventory", gate: "admin" },
];

export const CATALOG_ITEMS: AreaNavItem[] = [
  { to: "/catalog/sources", label: "Catalog Sources", icon: GitBranch, description: "Git sources feeding the catalog", gate: { resource: "catalogsources", verb: "read" } },
  { to: "/catalog", label: "Catalog Items", icon: Boxes, description: "Browse and compose catalog items", gate: { resource: "catalogitems", verb: "read" }, end: true },
  { to: "/catalog/bundles", label: "Composed Bundles", icon: PackageCheck, description: "Ordered bundle compositions", gate: { resource: "composedbundles", verb: "read" } },
];

export function areaItemAllowed(item: AreaNavItem, perms: Permissions | undefined): boolean {
  if (typeof item.gate === "string") return canDoGlobal(perms, "*", "*");
  if (item.gate.globalOnly) return canDoGlobal(perms, item.gate.resource, item.gate.verb);
  return canDoAnywhere(perms, item.gate.resource, item.gate.verb);
}
