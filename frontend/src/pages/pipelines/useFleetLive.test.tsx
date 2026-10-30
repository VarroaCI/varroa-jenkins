import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { FleetRunPage, RunNotification } from "../../types";
import { DEFAULT_FEED_QUERY, parseFeedQuery, type FeedQuery } from "./feedQuery";

type Handler = (ev: { type: string; data: RunNotification }) => void;

const streamState: { onEvent: Handler | null; readyState: string } = { onEvent: null, readyState: "connecting" };
const mockListFleetRuns = vi.fn();

vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: (_url: string | null, _scope: string | null, _names: string[], options: { onEvent?: Handler }) => {
    streamState.onEvent = options?.onEvent ?? null;
    return { lastEvent: null, readyState: streamState.readyState, error: null };
  },
}));
vi.mock("../../api/client", () => ({
  listFleetRuns: (...args: unknown[]) => mockListFleetRuns(...args),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));

import { useFleetLive } from "./useFleetLive";

function frame(runId: string): { type: string; data: RunNotification } {
  return {
    type: "run",
    data: { kind: "run", cluster: "core", tenant: "pay", job_path: "pay/api", run_id: runId, seq: 1, at: "2026-10-30T10:00:00Z" },
  };
}

function emptyPage(): FleetRunPage {
  return { items: [] };
}

function wrapper(client: QueryClient) {
  return function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

function setup(query: FeedQuery = DEFAULT_FEED_QUERY) {
  const client = new QueryClient();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  const hook = renderHook(({ q }: { q: FeedQuery }) => useFleetLive(q, true), {
    initialProps: { q: query },
    wrapper: wrapper(client),
  });
  return { client, invalidate, hook };
}

describe("useFleetLive", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockListFleetRuns.mockReset();
    mockListFleetRuns.mockResolvedValue(emptyPage());
    streamState.readyState = "connecting";
    streamState.onEvent = null;
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("two frames in 2 s cause one first-page read under the default sort", async () => {
    const { hook } = setup();
    act(() => {
      streamState.onEvent?.(frame("a"));
    });
    act(() => {
      vi.advanceTimersByTime(500);
      streamState.onEvent?.(frame("b"));
    });
    await act(async () => {
      vi.advanceTimersByTime(2000);
    });
    expect(mockListFleetRuns).toHaveBeenCalledTimes(1);
    expect(hook.result.current.newRuns).toBe(0);
  });

  it("a non-default sort counts new runs and does not read", () => {
    const { hook } = setup(parseFeedQuery(new URLSearchParams("sort=duration")));
    act(() => {
      streamState.onEvent?.(frame("x"));
      streamState.onEvent?.(frame("y"));
    });
    expect(mockListFleetRuns).not.toHaveBeenCalled();
    expect(hook.result.current.newRuns).toBe(2);
  });

  it("onShowNew clears the count", () => {
    const { hook } = setup(parseFeedQuery(new URLSearchParams("sort=duration")));
    act(() => {
      streamState.onEvent?.(frame("x"));
    });
    expect(hook.result.current.newRuns).toBe(1);
    act(() => {
      hook.result.current.onShowNew();
    });
    expect(hook.result.current.newRuns).toBe(0);
  });

  it("the overview invalidates at most once per 5 s", async () => {
    const { invalidate } = setup();
    act(() => {
      streamState.onEvent?.(frame("a"));
      vi.advanceTimersByTime(1000);
      streamState.onEvent?.(frame("b"));
      vi.advanceTimersByTime(1000);
      streamState.onEvent?.(frame("c"));
    });
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    const overviewCalls = invalidate.mock.calls.filter(
      (c) => JSON.stringify(c[0]) === JSON.stringify({ queryKey: ["pipelines-overview"] }),
    );
    expect(overviewCalls).toHaveLength(1);
  });

  it("a filtered default-sort view refetches its pages instead of reading the first page", async () => {
    const { invalidate } = setup(parseFeedQuery(new URLSearchParams("result=FAILURE")));
    act(() => {
      streamState.onEvent?.(frame("a"));
    });
    await act(async () => {
      vi.advanceTimersByTime(2000);
    });
    expect(mockListFleetRuns).not.toHaveBeenCalled();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["fleet-runs", "result=FAILURE"] });
  });

  it("a lock frame refreshes the overview and leaves the feed alone", async () => {
    const { invalidate } = setup();
    act(() => {
      streamState.onEvent?.({ type: "lock", data: frame("a").data });
    });
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(mockListFleetRuns).not.toHaveBeenCalled();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["pipelines-overview"] });
  });

  it("the first open does not read, and a reconnect reads the first page once", async () => {
    const { hook } = setup();
    streamState.readyState = "open";
    hook.rerender({ q: DEFAULT_FEED_QUERY });
    await act(async () => {
      await Promise.resolve();
    });
    expect(mockListFleetRuns).not.toHaveBeenCalled();
    streamState.readyState = "closed";
    hook.rerender({ q: DEFAULT_FEED_QUERY });
    streamState.readyState = "open";
    hook.rerender({ q: DEFAULT_FEED_QUERY });
    await act(async () => {
      await Promise.resolve();
    });
    expect(mockListFleetRuns).toHaveBeenCalledTimes(1);
  });
});
