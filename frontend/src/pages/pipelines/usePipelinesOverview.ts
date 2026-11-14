import { useQuery } from "@tanstack/react-query";
import { getPipelinesOverview } from "../../api/client";

/**
 * usePipelinesOverview reads GET /pipelines/overview on a 30-second refetch.
 * The command center and the Overview Pipelines section share this read.
 */
export function usePipelinesOverview(enabled = true) {
  return useQuery({
    queryKey: ["pipelines-overview"],
    queryFn: getPipelinesOverview,
    refetchInterval: 30_000,
    enabled,
  });
}
