import { useState, useEffect, useCallback, useRef, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError, bffFetch } from "./useApi";
import { useEventStream } from "./useEventStream";
import { useFrameQueue } from "./useFrameQueue";
import { BFF_BASE } from "../api/client";
import {
  mergeEvents,
  passesScope,
  isActivityEvent,
  MAX_BUFFER,
} from "../components/activityTimeline.util";
import type { ActivityEvent, ActivityFilters, ActivityPage } from "../types";

export const LOAD_MORE_EXPIRED = "Older events are past the retention window.";
export const LOAD_MORE_INVALID = "Older events did not load.";
export const LOAD_MORE_FAILED = "Older events did not load. Select Load more history to try again.";

export interface UseActivityFeedResult {
  events: ActivityEvent[];
  pendingCount: number;
  paused: boolean;
  setPaused: (v: boolean) => void;
  resume: () => void;
  readyState: string;
  error: Error | null;
  isLoading: boolean;
	hasMore: boolean;
	loadMore: () => Promise<void>;
	isLoadingMore: boolean;
	loadMoreNote: string | null;
	retentionMode?: "on" | "off";
	retentionDays?: number;
}

/** useActivityFeed reads one history page and opens one live stream. A page calls it once and passes the result to each view of the feed. */
export function useActivityFeed(
  scope?: { cluster?: string; namespace: string; name: string },
	filters: ActivityFilters = {},
): UseActivityFeedResult {
	const scopeQuery = useMemo(() => {
		const params = new URLSearchParams(); const effective = {...filters};
		if (scope) { effective.cluster = scope.cluster ?? ""; effective.controller = `${scope.namespace}/${scope.name}`; }
		for (const [key, value] of Object.entries(effective)) if ((value || (scope && key === "cluster")) && key !== "range") params.set(key, value ?? "");
		if (filters.range && filters.range !== "custom") {
			const amounts = {"15m": 15*60e3, "1h": 36e5, "6h": 6*36e5, "24h": 24*36e5, "7d": 7*24*36e5};
			// A preset sends no end, because the live stream drops each event at or after end.
			params.delete("end");
			params.set("start", new Date(Date.now()-amounts[filters.range]).toISOString());
		}
		return params.size ? `?${params}` : "";
	}, [JSON.stringify(filters), scope?.cluster, scope?.namespace, scope?.name]);

	// The BFF closes the live stream at end, and useEventStream reconnects a closed
	// stream. A range whose end has passed therefore opens no stream.
	const liveEnd = useMemo(() => {
		const end = new URLSearchParams(scopeQuery.slice(1)).get("end");
		const ms = end ? Date.parse(end) : NaN;
		return Number.isNaN(ms) ? null : ms;
	}, [scopeQuery]);
	const [endTick, setEndTick] = useState(0);
	useEffect(() => {
		if (liveEnd === null) return;
		const wait = liveEnd - Date.now();
		if (wait <= 0) return;
		const timer = setTimeout(() => setEndTick((n) => n + 1), Math.min(wait + 100, 2_147_483_647));
		return () => clearTimeout(timer);
	}, [liveEnd, endTick]);
	const liveOpen = liveEnd === null || liveEnd > Date.now();

  // Backfill via react-query
  const { data: backfillData, isLoading, error: fetchError } = useQuery({
    queryKey: ["activity", "feed", scopeQuery],
		queryFn: () => bffFetch<ActivityPage>("/activity" + scopeQuery),
    staleTime: 5_000,
  });

  // The queue keeps every live frame until the drain effect below merges it.
  // The feed never reads lastEvent, because lastEvent drops all but the last
  // frame of a burst.
  const { onEvent, drain: drainLive, clear: clearLive, tick: liveTick } = useFrameQueue<ActivityEvent>();

  // EventSource cannot send an Authorization header, so the bearer token is
  // passed as a query param (same pattern as the controller-events stream).
  // Also use BFF_BASE so the stream targets the API origin, not the SPA origin.
  const sseUrl = liveOpen ? `${BFF_BASE}/activity/stream${scopeQuery}` : null;
  const { readyState, error: sseError } =
    useEventStream<ActivityEvent>(sseUrl, "activity", ["activity"], { onEvent });

  // Main ring buffer (newest-first)
  const [buffer, setBuffer] = useState<ActivityEvent[]>([]);
	const [nextCursor, setNextCursor] = useState<string>();
	const [hasMore, setHasMore] = useState(false);
	const [isLoadingMore, setLoadingMore] = useState(false);
	const [loadMoreNote, setLoadMoreNote] = useState<string | null>(null);
  // Pending queue for paused arrivals — use a ref so resume always reads latest
  const [paused, setPaused] = useState(false);
  const pendingQueueRef = useRef<ActivityEvent[]>([]);
  const [pendingCount, setPendingCount] = useState(0);

  // Track whether we've seeded from backfill so we only do it once
	const backfillSeededRef = useRef<string | null>(null);
	const queryIdentity = scopeQuery;
	const identityRef = useRef(queryIdentity);

  useEffect(() => {
    identityRef.current = queryIdentity;
    setBuffer([]); pendingQueueRef.current = []; setPendingCount(0);
    // A frame queued before this effect came from the stream of the previous
    // query. useEventStream closes that stream in the same commit.
    clearLive();
    // Reset pagination alongside the buffer: nextCursor/hasMore/isLoadingMore
    // describe the PREVIOUS query's scope, so a stale "Load more" must never
    // fire the old cursor against the new query while the new backfill is
    // still in flight. The seeding effect below re-drives them from the fresh
    // page once it lands.
    setNextCursor(undefined); setHasMore(false); setLoadingMore(false); setLoadMoreNote(null);
  }, [queryIdentity]);

  // Seed from backfill — no "seed only if empty" guard; always merge
  useEffect(() => {
		if (backfillData && backfillSeededRef.current !== queryIdentity) {
			backfillSeededRef.current = queryIdentity;
      // Apply defensive scope filter
      const filtered = scope
				? backfillData.items.filter((e) => passesScope(e, scope))
				: backfillData.items;
			setBuffer((b) => mergeEvents(b, filtered, MAX_BUFFER));
			setNextCursor(backfillData.nextCursor); setHasMore(backfillData.hasMore);
    }
	}, [backfillData, scope, queryIdentity]);

  // Merge every queued live frame in one buffer update.
  useEffect(() => {
    const frames = drainLive();
    if (frames.length === 0) return;
    // The stream names every frame "activity", but brood telemetry can share
    // the name. Drop any frame of another type or without the ActivityEvent
    // shape, then apply the defensive scope filter.
    const incoming = frames
      .filter((ev) => ev.type === "activity")
      .map((ev): unknown => ev.data)
      .filter(isActivityEvent)
      .filter((e) => !scope || passesScope(e, scope));
    if (incoming.length === 0) return;
    if (!paused) {
      setBuffer((b) => mergeEvents(b, incoming, MAX_BUFFER));
    } else {
      pendingQueueRef.current = mergeEvents(pendingQueueRef.current, incoming, MAX_BUFFER);
      setPendingCount(pendingQueueRef.current.length);
    }
    // The effect does not depend on `scope`: callers such as ControllerDetail
    // pass a fresh object literal on every render. The effect reads the scope
    // of the render that runs it.
  }, [liveTick, paused]);

  // Resume: merge pending into buffer, clear queue
  const resume = useCallback(() => {
    const pending = pendingQueueRef.current;
    if (pending.length > 0) {
      setBuffer((b) => mergeEvents(b, pending, MAX_BUFFER));
    }
    pendingQueueRef.current = [];
    setPendingCount(0);
    setPaused(false);
  }, []);

	// A 410 means every older row is past the retention edge, so paging ends.
	// A 400 means the cursor is not valid, and a retry answers 400 again, so
	// paging ends too. An answer for an earlier query must not change the
	// current query's state.
	const loadMore = useCallback(async () => {
		if (!nextCursor || isLoadingMore) return;
		const identity = queryIdentity;
		setLoadingMore(true); setLoadMoreNote(null);
		try {
			const p = new URLSearchParams(scopeQuery.slice(1)); p.set("cursor", nextCursor);
			const page = await bffFetch<ActivityPage>(`/activity?${p}`);
			if (identityRef.current !== identity) return;
			setBuffer((b) => mergeEvents(b, page.items, MAX_BUFFER)); setNextCursor(page.nextCursor); setHasMore(page.hasMore);
		} catch (e) {
			if (identityRef.current !== identity) return;
			if (e instanceof ApiError && e.status === 410) {
				setNextCursor(undefined); setHasMore(false); setLoadMoreNote(LOAD_MORE_EXPIRED);
			} else if (e instanceof ApiError && e.status === 400) {
				setNextCursor(undefined); setHasMore(false); setLoadMoreNote(LOAD_MORE_INVALID);
			} else {
				setLoadMoreNote(LOAD_MORE_FAILED);
			}
		} finally {
			if (identityRef.current === identity) setLoadingMore(false);
		}
	}, [nextCursor, isLoadingMore, scopeQuery, queryIdentity]);

  return {
    events: buffer,
    pendingCount,
    paused,
    setPaused,
    resume,
    readyState,
    // The stream closes on purpose at end, so its close is not an error.
    error: fetchError ?? (sseError && liveOpen ? new Error("SSE connection error") : null),
    isLoading,
		hasMore, loadMore, isLoadingMore, loadMoreNote,
		retentionMode: backfillData?.retentionMode,
		retentionDays: backfillData?.retentionDays,
  };
}
