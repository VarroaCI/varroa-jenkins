import type { ControllerListItem } from "../../hooks/useControllers";

/** ControllerCounts names the controllers and the distinct namespaces that hold them. */
export interface ControllerCounts {
  controllers: number;
  namespaces: number;
}

/**
 * controllerCounts counts the controllers of a list. Namespaces are only unique
 * within a cluster, so a namespace is one (cluster, namespace) identity.
 */
export function controllerCounts(cs: ControllerListItem[]): ControllerCounts {
  return {
    controllers: cs.length,
    namespaces: new Set(cs.map((c) => `${c.cluster}/${c.namespace}`)).size,
  };
}

/** OverviewCounts are the counts the page subtitle names. A missing count is left out. */
export interface OverviewCounts {
  tenants?: number;
  controllers?: ControllerCounts;
}

/**
 * overviewSubtitle names the tenant count first, then the controller count.
 * It answers an empty string when neither count is given.
 */
export function overviewSubtitle({ tenants, controllers }: OverviewCounts): string {
  const parts: string[] = [];
  if (tenants !== undefined) {
    parts.push(tenants === 1 ? "1 tenant" : `${tenants} tenants`);
  }
  if (controllers !== undefined) {
    const c = controllers.controllers;
    const ns = controllers.namespaces;
    parts.push(
      `${c} controller${c === 1 ? "" : "s"} across ${ns} namespace${ns === 1 ? "" : "s"}`,
    );
  }
  return parts.join(" · ");
}
