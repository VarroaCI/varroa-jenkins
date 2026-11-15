import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { fleetRunStreamUrl } from "../../api/client";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import type { RunNotification } from "../../types";

/** OVERVIEW_WINDOW_MS bounds the overview invalidation to one per window. */
export const OVERVIEW_WINDOW_MS = 5000;
/** GROUPS_WINDOW_MS bounds the run-group invalidation to one per window. */
export const GROUPS_WINDOW_MS = 15000;

/**
 * useOverviewLive opens the one pipelines-runs stream of the Overview. A burst
 * of frames invalidates the overview at most once per OVERVIEW_WINDOW_MS. A
 * run frame invalidates the run groups at most once per GROUPS_WINDOW_MS. A
 * reconnect invalidates both reads, because the stream replays nothing.
 */
export function useOverviewLive(enabled: boolean): { readyState: string } {
  const queryClient = useQueryClient();
  const overviewTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const groupsTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const armOverview = () => {
    if (overviewTimer.current !== null) return;
    overviewTimer.current = setTimeout(() => {
      overviewTimer.current = null;
      void queryClient.invalidateQueries({ queryKey: ["pipelines-overview"] });
    }, OVERVIEW_WINDOW_MS);
  };
  const armGroups = () => {
    if (groupsTimer.current !== null) return;
    groupsTimer.current = setTimeout(() => {
      groupsTimer.current = null;
      void queryClient.invalidateQueries({ queryKey: ["pipelines-run-groups"] });
    }, GROUPS_WINDOW_MS);
  };

  const onEvent = (ev: SSEEvent<RunNotification>) => {
    armOverview();
    if (ev.type === "run") armGroups();
  };

  const stream = useEventStream<RunNotification>(
    enabled ? fleetRunStreamUrl() : null,
    enabled ? "pipelines-runs" : null,
    ["run", "input", "lock"],
    { onEvent },
  );

  const opened = useRef(false);
  useEffect(() => {
    if (stream.readyState !== "open") return;
    if (!opened.current) {
      opened.current = true;
      return;
    }
    void queryClient.invalidateQueries({ queryKey: ["pipelines-overview"] });
    void queryClient.invalidateQueries({ queryKey: ["pipelines-run-groups"] });
  }, [stream.readyState, queryClient]);

  useEffect(() => {
    return () => {
      if (overviewTimer.current !== null) clearTimeout(overviewTimer.current);
      if (groupsTimer.current !== null) clearTimeout(groupsTimer.current);
      overviewTimer.current = null;
      groupsTimer.current = null;
    };
  }, []);

  return { readyState: stream.readyState };
}
