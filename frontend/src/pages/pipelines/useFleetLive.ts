import { useCallback, useEffect, useRef, useState } from "react";
import { useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { fleetRunStreamUrl, listFleetRuns } from "../../api/client";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import type { FleetRunPage, RunNotification } from "../../types";
import { writeFeedQuery, type FeedQuery } from "./feedQuery";
import { feedParams, hasStateFilter, isDefaultSort, mergeFirstPage } from "./feedLive";

/** FEED_WINDOW_MS is the trailing window of the first-page read under the default sort. */
const FEED_WINDOW_MS = 2000;
/** OVERVIEW_WINDOW_MS bounds the overview invalidation to one per window. */
const OVERVIEW_WINDOW_MS = 5000;
/** TICK_MS re-renders the updated text. */
const TICK_MS = 10000;

type FeedPages = InfiniteData<FleetRunPage, string | undefined>;

export interface FleetLive {
  updatedAt: number | null;
  newRuns: number;
  onShowNew: () => void;
  readyState: string;
}

/**
 * useFleetLive applies the fleet run stream to the feed. Under the default
 * sort, a burst of frames causes one first-page read. Under any other sort,
 * a frame for a run that no loaded row holds counts as a new run, and the
 * rows stay where they are. Every burst also invalidates the overview, at
 * most once per window.
 */
export function useFleetLive(query: FeedQuery, enabled: boolean): FleetLive {
  const queryClient = useQueryClient();
  const keyString = writeFeedQuery(query).toString();
  const queryRef = useRef(query);
  queryRef.current = query;
  const keyRef = useRef(keyString);
  keyRef.current = keyString;

  const [newIds, setNewIds] = useState<Set<string>>(() => new Set());
  const [updatedAt, setUpdatedAt] = useState<number | null>(null);
  const [, setTick] = useState(0);

  const feedTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const overviewTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const loadedIds = useCallback((): Set<string> => {
    const data = queryClient.getQueryData<FeedPages>(["fleet-runs", keyRef.current]);
    const ids = new Set<string>();
    for (const page of data?.pages ?? []) {
      for (const r of page.items) ids.add(r.runId);
    }
    return ids;
  }, [queryClient]);

  const readFirstPage = useCallback(async () => {
    const q = queryRef.current;
    const key = keyRef.current;
    if (isDefaultSort(q) && hasStateFilter(q)) {
      // A row that leaves a filter must leave the feed, and a merge cannot
      // remove it. A narrowed view therefore refetches its loaded pages.
      await queryClient.invalidateQueries({ queryKey: ["fleet-runs", key] });
      return;
    }
    const page = await listFleetRuns(feedParams(q));
    if (keyRef.current !== key) return;
    if (isDefaultSort(q)) {
      queryClient.setQueryData<FeedPages>(["fleet-runs", key], (old) => {
        if (!old || old.pages.length === 0) return old;
        const merged = mergeFirstPage(
          old.pages.map((p) => p.items),
          page.items,
        );
        return { ...old, pages: old.pages.map((p, i) => ({ ...p, items: merged[i] })) };
      });
      return;
    }
    const loaded = loadedIds();
    const absent = page.items.map((r) => r.runId).filter((id) => !loaded.has(id));
    if (absent.length > 0) setNewIds((prev) => new Set([...prev, ...absent]));
  }, [loadedIds, queryClient]);

  const armFeedRead = useCallback(() => {
    if (feedTimer.current !== null) return;
    feedTimer.current = setTimeout(() => {
      feedTimer.current = null;
      void readFirstPage();
    }, FEED_WINDOW_MS);
  }, [readFirstPage]);

  const armOverview = useCallback(() => {
    if (overviewTimer.current !== null) return;
    overviewTimer.current = setTimeout(() => {
      overviewTimer.current = null;
      void queryClient.invalidateQueries({ queryKey: ["pipelines-overview"] });
    }, OVERVIEW_WINDOW_MS);
  }, [queryClient]);

  const onEvent = (ev: SSEEvent<RunNotification>) => {
    // A lock frame changes the overview counts, and it leaves the feed alone.
    if (ev.type === "lock") {
      armOverview();
      return;
    }
    const frame = ev.data;
    if (!frame || !frame.run_id) return;
    if (isDefaultSort(queryRef.current)) {
      armFeedRead();
    } else if (!loadedIds().has(frame.run_id)) {
      setNewIds((prev) => new Set([...prev, frame.run_id]));
    }
    armOverview();
  };

  const stream = useEventStream<RunNotification>(
    enabled ? fleetRunStreamUrl() : null,
    enabled ? "pipelines-runs" : null,
    ["run", "input", "lock"],
    { onEvent },
  );

  // A reconnect reads the first page again, because the stream replays
  // nothing. The first open does not read, because the feed read its own page.
  const opened = useRef(false);
  useEffect(() => {
    if (stream.readyState !== "open") return;
    if (!opened.current) {
      opened.current = true;
      return;
    }
    void readFirstPage();
  }, [stream.readyState, readFirstPage]);

  // A new view starts with no pending new runs, and its updated time is
  // unknown until its own read lands.
  // A cached view that is still fresh does not fetch on mount, so its time
  // comes from the cache state.
  useEffect(() => {
    setNewIds(new Set());
    const state = queryClient.getQueryState(["fleet-runs", keyString]);
    setUpdatedAt(state && state.dataUpdatedAt > 0 ? state.dataUpdatedAt : null);
  }, [keyString, queryClient]);

  // The updated text reads the feed's own successful fetches.
  useEffect(() => {
    return queryClient.getQueryCache().subscribe((event) => {
      if (event.type !== "updated") return;
      if (event.query.queryKey[0] !== "fleet-runs") return;
      if (event.query.queryKey[1] !== keyRef.current) return;
      if (event.query.state.status === "success") setUpdatedAt(event.query.state.dataUpdatedAt);
    });
  }, [queryClient]);

  useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), TICK_MS);
    return () => clearInterval(t);
  }, []);

  useEffect(() => {
    return () => {
      if (feedTimer.current !== null) clearTimeout(feedTimer.current);
      if (overviewTimer.current !== null) clearTimeout(overviewTimer.current);
      feedTimer.current = null;
      overviewTimer.current = null;
    };
  }, []);

  const onShowNew = useCallback(() => {
    setNewIds(new Set());
    void queryClient.resetQueries({ queryKey: ["fleet-runs", keyRef.current] });
  }, [queryClient]);

  return {
    updatedAt,
    newRuns: newIds.size,
    onShowNew,
    readyState: stream.readyState,
  };
}
