import { Activity, Bot, Boxes, CircleUserRound, Clock3, Hexagon, LayoutDashboard, Network, PlugZap, Server, Shield, Waypoints, Workflow, type LucideIcon } from "lucide-react";
import { canSeePipelines } from "../hooks/usePermissions";
import type { Permissions } from "../types/auth";
import { canAdminArea, canAgentsRow, canCatalogArea, canCellsRow, canControllersRow } from "./navPermissions";

/** The permission gate of a navigation row. A row with no gate is always visible. */
export type NavGate = "pipelines" | "controllers" | "cells" | "agents" | "catalog" | "admin";

export interface NavRow {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  /** The row opens an area shell. The sidebar shows a chevron on it. */
  door?: boolean;
  gate?: NavGate;
  /** The phone tab bar carries the row, so the More sheet leaves it out. */
  tab?: boolean;
  /** The row shows the live controllers count. */
  count?: "controllers";
}

export interface NavRowGroup {
  label: string;
  rows: readonly NavRow[];
}

/**
 * NAV_GROUPS is the one row table of the sidebar, the phone tab bar, and the
 * phone More sheet. Each surface filters it and keeps the table order.
 */
export const NAV_GROUPS: readonly NavRowGroup[] = [
  { label: "Operate", rows: [
    { to: "/", label: "Overview", icon: LayoutDashboard, end: true, tab: true },
    { to: "/pipelines", label: "Pipelines", icon: Waypoints, gate: "pipelines" },
    { to: "/controllers", label: "Controllers", icon: Server, gate: "controllers", tab: true, count: "controllers" },
    { to: "/cells", label: "Cells", icon: Hexagon, gate: "cells" },
    { to: "/agents", label: "Tenant agents", icon: Bot, gate: "agents" },
    { to: "/plugins", label: "Plugins", icon: PlugZap, gate: "controllers" },
    { to: "/clusters", label: "Clusters", icon: Network, tab: true },
    { to: "/activity", label: "Activity", icon: Activity, tab: true },
  ] },
  { label: "Brood", rows: [
    { to: "/brood-operations", label: "Operations", icon: Workflow },
    { to: "/brood-schedules", label: "Schedules", icon: Clock3 },
  ] },
  { label: "Manage", rows: [
    { to: "/catalog", label: "Catalog", icon: Boxes, door: true, gate: "catalog" },
    { to: "/settings", label: "Admin & access", icon: Shield, door: true, gate: "admin" },
  ] },
];

/** The sidebar foot and the More sheet Account group both render this row. */
export const PROFILE_ROW: NavRow = { to: "/profile", label: "My profile", icon: CircleUserRound };

/** The rows the phone tab bar carries, in table order. */
export const TAB_ROWS: readonly NavRow[] = NAV_GROUPS.flatMap((group) => group.rows.filter((row) => row.tab));

/** navRowVisible is the only visibility check of a navigation row. */
export function navRowVisible(row: NavRow, perms: Permissions | undefined): boolean {
  switch (row.gate) {
    case undefined:
      return true;
    // The pipelines gate reads the caller's grants, not the record store: the
    // row still renders when the pipelines API answers 503.
    case "pipelines":
      return canSeePipelines(perms);
    // A tenant-scoped grant reads no controller, so it admits neither row.
    case "controllers":
      return canControllersRow(perms);
    // CellPool is cluster scoped, so the row takes the cluster-wide partition.
    case "cells":
      return canCellsRow(perms);
    case "agents":
      return canAgentsRow(perms);
    case "catalog":
      return canCatalogArea(perms);
    case "admin":
      return canAdminArea(perms);
  }
}

/**
 * visibleNavGroups keeps the visible rows of each group, in table order. It
 * leaves out a group with no visible row, so no surface shows an empty group
 * label. A caller who holds the grant of one row alone still gets its group.
 */
export function visibleNavGroups(groups: readonly NavRowGroup[], perms: Permissions | undefined): NavRowGroup[] {
  return groups
    .map((group) => ({ label: group.label, rows: group.rows.filter((row) => navRowVisible(row, perms)) }))
    .filter((group) => group.rows.length > 0);
}
