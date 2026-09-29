import { useQueries, useQuery } from "@tanstack/react-query";
import { ApiError } from "./useApi";
import type { CellUsage } from "../types";
import { getCellGrants, getCellPool, getCellPools, getCellUsage, listCellUsage } from "../api/client";

/**
 * Every cell surface polls, and no cell surface opens a stream. 15 seconds
 * is the cadence the controllers list uses, so a Cells page row and a
 * controllers row never age differently.
 */
const CELL_POLL_MS = 15_000;

/**
 * A role binding changes rarely, so the grants read holds a stale time and
 * no interval.
 */
const GRANTS_STALE_MS = 60_000;

/**
 * Lists the cell pools of every cluster, or of one cluster. The data is the
 * whole envelope: the page needs the clusters status beside the items.
 */
export function useCellPools(cluster?: string) {
  return useQuery({
    queryKey: ["cellpools", cluster ?? null],
    queryFn: () => getCellPools(cluster),
    refetchInterval: CELL_POLL_MS,
  });
}

/**
 * Reads one cell pool. The Cell tab passes enabled false while another tab
 * is active, so a page open on Overview costs nothing.
 */
export function useCellPool(name: string | undefined, enabled: boolean, cluster?: string) {
  return useQuery({
    queryKey: ["cellpool", cluster ?? null, name ?? null],
    queryFn: () => getCellPool(name as string, cluster),
    enabled: enabled && !!name,
    refetchInterval: CELL_POLL_MS,
  });
}

/** Lists the load of every cell the caller may read in one cluster. */
export function useCellUsageList(params?: { cluster?: string; namespace?: string }) {
  return useQuery({
    queryKey: ["cell-usage", params?.cluster ?? null, params?.namespace ?? null],
    queryFn: () => listCellUsage(params).then((r) => r.items),
    refetchInterval: CELL_POLL_MS,
  });
}

/** Reads one cell's load, tenant by tenant. */
export function useCellUsage(namespace: string | undefined, name: string | undefined, enabled: boolean, cluster?: string) {
  return useQuery({
    queryKey: ["cell-usage", cluster ?? null, namespace ?? null, name ?? null],
    queryFn: () => getCellUsage(namespace as string, name as string, cluster),
    enabled: enabled && !!namespace && !!name,
    refetchInterval: CELL_POLL_MS,
  });
}

/** Reads which subjects may build on each tenant of a cell. */
export function useCellGrants(namespace: string | undefined, name: string | undefined, enabled: boolean, cluster?: string) {
  return useQuery({
    queryKey: ["cell-grants", cluster ?? null, namespace ?? null, name ?? null],
    queryFn: () => getCellGrants(namespace as string, name as string, cluster),
    enabled: enabled && !!namespace && !!name,
    staleTime: GRANTS_STALE_MS,
    retry: false,
  });
}

/**
 * CellUsageIndex is one cluster-keyed view of the batch cell usage read.
 *
 * rows is keyed "cluster/namespace/name". A cell name is unique inside one
 * cluster only, so a namespace-and-name key hands one cluster's counts to
 * another cluster's cell of the same name.
 *
 * covered names the clusters whose read answered. A cell whose cluster is
 * absent from it has no usage data at all, which is not the same state as a
 * count of zero.
 */
export interface CellUsageIndex {
  rows: Map<string, CellUsage>;
  covered: Set<string>;
  recordStoreDisabled: Set<string>;
  isLoading: boolean;
}

/** cellUsageKey builds the index key of one cell. */
export function cellUsageKey(cluster: string, namespace: string, name: string): string {
  return `${cluster}/${namespace}/${name}`;
}

/**
 * Reads the cell usage of each named cluster, one request per cluster, and
 * returns one cluster-keyed index.
 *
 * The batch read covers a single cluster, so a fleet page that spans
 * clusters issues one request for each. A row is keyed by the cluster the
 * request named, never by the cluster the response reports, so the join
 * cannot cross a cluster boundary.
 */
export function useCellUsageIndex(clusters: string[]): CellUsageIndex {
  const results = useQueries({
    queries: clusters.map((cluster) => ({
      queryKey: ["cell-usage", cluster || null, null],
      queryFn: () => listCellUsage(cluster ? { cluster } : undefined).then((r) => r.items),
      refetchInterval: CELL_POLL_MS,
      retry: false,
    })),
  });

  const rows = new Map<string, CellUsage>();
  const covered = new Set<string>();
  const recordStoreDisabled = new Set<string>();
  let isLoading = false;

  results.forEach((result, i) => {
    const cluster = clusters[i];
    if (result.isLoading) isLoading = true;
    if (result.error instanceof ApiError && result.error.status === 503) {
      recordStoreDisabled.add(cluster);
      return;
    }
    if (!result.data) return;
    covered.add(cluster);
    for (const row of result.data) {
      rows.set(cellUsageKey(cluster, row.namespace, row.name), row);
    }
  });

  return { rows, covered, recordStoreDisabled, isLoading };
}
