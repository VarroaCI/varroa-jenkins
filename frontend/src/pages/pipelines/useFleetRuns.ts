import { useInfiniteQuery } from "@tanstack/react-query";
import { listFleetRuns } from "../../api/client";
import { feedParams } from "./feedLive";
import { writeFeedQuery, type FeedQuery } from "./feedQuery";

/**
 * useFleetRuns reads the fleet run feed for one query, one page at a time.
 * The key is the query string, so the command center and the Overview share
 * one cache entry for the default view.
 */
export function useFleetRuns(query: FeedQuery, enabled = true) {
  return useInfiniteQuery({
    queryKey: ["fleet-runs", writeFeedQuery(query).toString()],
    queryFn: ({ pageParam }) => listFleetRuns({ ...feedParams(query), cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor,
    enabled,
  });
}
