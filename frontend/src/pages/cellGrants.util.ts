import type { CellGrant, CellTenantGrants } from "../types";

/** The grant source the BFF stamps on a cluster-wide VarroaRoleBinding row. */
const CLUSTER_WIDE_SOURCE = "clusterWideBinding";

/**
 * A row reaches every tenant of the cell when it is a cluster-wide binding or
 * a federated copy that the BFF marks allTenants.
 */
function reachesEveryTenant(grant: CellGrant): boolean {
  return grant.source === CLUSTER_WIDE_SOURCE || grant.allTenants === true;
}

/**
 * Splits the grants of every tenant of a cell into the cluster-wide set and
 * the rows each tenant holds alone. A cluster-wide binding and a federated
 * all-tenants copy reach every tenant, so the BFF repeats each row inside
 * each tenant entry. The panel shows the set once and the rest per tenant.
 * The set is sorted by source, then by binding, so clusterWideBinding rows
 * come before federatedBinding rows.
 */
export function splitClusterWideGrants(entries: CellTenantGrants[]): {
  clusterWide: CellGrant[];
  tenants: Array<{ entry: CellTenantGrants; own: CellGrant[] }>;
} {
  const seen = new Map<string, CellGrant>();
  const tenants = entries.map((entry) => {
    const own: CellGrant[] = [];
    for (const grant of entry.grants) {
      if (reachesEveryTenant(grant)) {
        // A tenant list stops at 50 grants. A binding that one list cut
        // can survive in another list, so every list feeds the set.
        // A cluster-wide binding and a federated copy can share a name, so the key holds the source too.
        const key = `${grant.source}:${grant.binding ?? ""}`;
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
