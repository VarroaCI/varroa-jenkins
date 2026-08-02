import { useState, useEffect, useCallback, useRef, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { bffFetch } from "./useApi";
import { useEventStream } from "./useEventStream";
import { BFF_BASE } from "../api/client";
import { mergeEvents, passesScope, MAX_BUFFER } from "../components/activityTimeline.util";
import type { ActivityEvent, ActivityFilters, ActivityPage } from "../types";

interface UseActivityFeedResult {
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
	retentionMode?: "on" | "off";
	retentionDays?: number;
}

export function useActivityFeed(
  scope?: { cluster?: string; namespace: string; name: string },
	filters: ActivityFilters = {},
): UseActivityFeedResult {
	const scopeQuery = useMemo(() => {
		const params = new URLSearchParams(); const effective = {...filters};
		if (scope) { effective.cluster = scope.cluster ?? ""; effective.controller = `${scope.namespace}/${scope.name}`; }
		for (const [key, value] of Object.entries(effective)) if ((value || (scope && key === "cluster")) && key !== "range") params.set(key, value ?? "");
		if (filters.range && filters.range !== "custom") { const amounts = {"15m": 15*60e3, "1h": 36e5, "6h": 6*36e5, "24h": 24*36e5, "7d": 7*24*36e5}; const end = new Date(); params.set("end", end.toISOString()); params.set("start", new Date(end.getTime()-amounts[filters.range]).toISOString()); }
		return params.size ? `?${params}` : "";
	}, [JSON.stringify(filters), scope?.cluster, scope?.namespace, scope?.name]);

  // Backfill via react-query
  const { data: backfillData, isLoading, error: fetchError } = useQuery({
    queryKey: ["activity", "feed", scopeQuery],
		queryFn: () => bffFetch<ActivityPage>("/activity" + scopeQuery),
    staleTime: 5_000,
  });

  // Live SSE stream
  // EventSource cannot send an Authorization header, so the bearer token is
  // passed as a query param (same pattern as the controller-events stream).
  // Also use BFF_BASE so the stream targets the API origin, not the SPA origin.
  const sseUrl = `${BFF_BASE}/activity/stream${scopeQuery}`;
  const { lastEvent, readyState, error: sseError } =
    useEventStream<ActivityEvent>(sseUrl, "activity");

  // Main ring buffer (newest-first)
  const [buffer, setBuffer] = useState<ActivityEvent[]>([]);
	const [nextCursor, setNextCursor] = useState<string>();
	const [hasMore, setHasMore] = useState(false);
	const [isLoadingMore, setLoadingMore] = useState(false);
  // Pending queue for paused arrivals — use a ref so resume always reads latest
  const [paused, setPaused] = useState(false);
  const pendingQueueRef = useRef<ActivityEvent[]>([]);
  const [pendingCount, setPendingCount] = useState(0);

  // Track whether we've seeded from backfill so we only do it once
	const backfillSeededRef = useRef<string | null>(null);
	const queryIdentity = scopeQuery;
	useEffect(() => { setBuffer([]); pendingQueueRef.current = []; setPendingCount(0); }, [queryIdentity]);

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

  // Ingest live SSE events
  useEffect(() => {
    if (lastEvent && lastEvent.type === "activity") {
      const incoming = lastEvent.data as ActivityEvent;
      // Apply defensive scope filter
      if (scope && !passesScope(incoming, scope)) return;

      if (!paused) {
        setBuffer((b) => mergeEvents(b, [incoming], MAX_BUFFER));
      } else {
        pendingQueueRef.current = mergeEvents(
          pendingQueueRef.current,
          [incoming],
          MAX_BUFFER,
        );
        setPendingCount(pendingQueueRef.current.length);
      }
    }
  }, [lastEvent, paused, scope]);

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

	const loadMore = useCallback(async () => {
		if (!nextCursor || isLoadingMore) return;
		setLoadingMore(true);
		try {
			const p = new URLSearchParams(scopeQuery.slice(1)); p.set("cursor", nextCursor);
			const page = await bffFetch<ActivityPage>(`/activity?${p}`);
			setBuffer((b) => mergeEvents(b, page.items, MAX_BUFFER)); setNextCursor(page.nextCursor); setHasMore(page.hasMore);
		} finally { setLoadingMore(false); }
	}, [nextCursor, isLoadingMore, scopeQuery]);

  return {
    events: buffer,
    pendingCount,
    paused,
    setPaused,
    resume,
    readyState,
    error: fetchError ?? (sseError ? new Error("SSE connection error") : null),
    isLoading,
		hasMore, loadMore, isLoadingMore,
		retentionMode: backfillData?.retentionMode,
		retentionDays: backfillData?.retentionDays,
  };
}
