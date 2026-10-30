import type { CellGrant, CellTenantGrants } from "../types";

/** The grant source the BFF stamps on a cluster-wide VarroaRoleBinding row. */
const CLUSTER_WIDE_SOURCE = "clusterWideBinding";

/**
 * Splits the grants of every tenant of a cell into the cluster-wide set and
 * the rows each tenant holds alone. A cluster-wide binding reaches every
 * tenant, so the BFF repeats its row inside each tenant entry. The panel
 * shows the set once and the rest per tenant.
 */
export function splitClusterWideGrants(entries: CellTenantGrants[]): {
  clusterWide: CellGrant[];
  tenants: Array<{ entry: CellTenantGrants; own: CellGrant[] }>;
} {
  const seen = new Map<string, CellGrant>();
  const tenants = entries.map((entry) => {
    const own: CellGrant[] = [];
    for (const grant of entry.grants) {
      if (grant.source === CLUSTER_WIDE_SOURCE) {
        // A tenant list stops at 50 grants. A binding that one list cut
        // can survive in another list, so every list feeds the set.
        const key = grant.binding ?? "";
        if (!seen.has(key)) seen.set(key, grant);
      } else {
        own.push(grant);
      }
    }
    return { entry, own };
  });
  const clusterWide = Array.from(seen.entries())
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([, grant]) => grant);
  return { clusterWide, tenants };
}
