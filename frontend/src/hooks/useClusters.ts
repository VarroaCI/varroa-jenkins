import { useQuery } from "@tanstack/react-query";
import { listClusters } from "../api/client";
import type { ClusterEntry } from "../types";

export function useClusters() {
  return useQuery({
    queryKey: ["clusters"],
    queryFn: listClusters,
    refetchInterval: 30_000,
  });
}

export function coreOf(clusters: ClusterEntry[] | undefined): ClusterEntry | undefined {
  return clusters?.find((c) => c.core);
}
