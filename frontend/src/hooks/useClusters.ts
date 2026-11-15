import { useQuery } from "@tanstack/react-query";
import { listClusters } from "../api/client";
import type { ClusterEntry } from "../types";

/** useClusters reads GET /clusters on a 30-second refetch. enabled:false sends no read. */
export function useClusters(enabled = true) {
  return useQuery({
    queryKey: ["clusters"],
    queryFn: listClusters,
    refetchInterval: 30_000,
    enabled,
  });
}

export function coreOf(clusters: ClusterEntry[] | undefined): ClusterEntry | undefined {
  return clusters?.find((c) => c.core);
}
