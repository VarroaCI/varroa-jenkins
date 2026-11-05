import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  useActivityFeed,
  LOAD_MORE_EXPIRED,
  LOAD_MORE_INVALID,
  LOAD_MORE_FAILED,
} from "./useActivityFeed";
import { ApiError } from "./useApi";
import type { ActivityEvent } from "../types";
import type { ReactNode } from "react";

// ── Mocks ──────────────────────────────────────────────────────────────────

const mockBffFetch = vi.fn();
vi.mock("./useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./useApi")>()),
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
}));

let mockOnEvent: ((ev: { type: string; data: unknown }) => void) | undefined;
let mockReadyState = "closed";
let mockSSEError: Event | null = null;
let mockStreamUrl: unknown = null;

vi.mock("./useEventStream", () => ({
  useEventStream: (...args: unknown[]) => {
    mockStreamUrl = args[0];
    mockOnEvent = (args[3] as { onEvent?: typeof mockOnEvent } | undefined)?.onEvent;
    return {
    lastEvent: null,
    readyState: mockReadyState,
    error: mockSSEError,
    close: vi.fn(),
    };
  },
}));

function createEvent(overrides: Partial<ActivityEvent>): ActivityEvent {
  return {
    timestamp: new Date().toISOString(),
    type: "build.completed",
    source: "jenkins",
    message: "Test event",
    ...overrides,
  };
}

function createWrapper() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  // eslint-disable-next-line react/display-name
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  mockBffFetch.mockReset();
  mockBffFetch.mockResolvedValue({items: []});
  mockOnEvent = undefined;
  mockReadyState = "closed";
  mockSSEError = null;
  mockStreamUrl = null;
});

describe("useActivityFeed", () => {
  it("sends scope hint to backfill endpoint", async () => {
    renderHook(() => useActivityFeed({ namespace: "ns", name: "ctrl" }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalledWith(
        "/activity?cluster=&controller=ns%2Fctrl",
      );
    });
  });

  it("seeds events from backfill", async () => {
    const evt = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      buildNumber: 1,
    });
    mockBffFetch.mockResolvedValue({items: [evt]});

    const { result } = renderHook(() => useActivityFeed(), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(result.current.events.length).toBe(1);
    });
  });

  it("buffers paused arrivals in pendingQueue", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    const { result } = renderHook(() => useActivityFeed(), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    // Pause
    act(() => {
      result.current.setPaused(true);
    });
    expect(result.current.paused).toBe(true);

    // Simulate SSE arrival while paused
    const sseEvent = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      buildNumber: 1,
    });
    act(() => {
      mockOnEvent!({ type: "activity", data: sseEvent });
    });

    await waitFor(() => {
      expect(result.current.pendingCount).toBe(1);
    });
    expect(result.current.events.length).toBe(0);
  });

  it("resume flushes pendingQueue into buffer", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    const { result } = renderHook(() => useActivityFeed(), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    // Pause
    act(() => {
      result.current.setPaused(true);
    });

    // Add SSE event while paused
    const sseEvent = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      buildNumber: 1,
    });
    act(() => {
      mockOnEvent!({ type: "activity", data: sseEvent });
    });

    await waitFor(() => {
      expect(result.current.pendingCount).toBe(1);
    });

    // Resume
    act(() => {
      result.current.resume();
    });

    expect(result.current.paused).toBe(false);
    expect(result.current.pendingCount).toBe(0);
    expect(result.current.events.length).toBe(1);
  });

  it("drops off-scope events when scope is set", async () => {
    const matchingEvent = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      namespace: "ns",
      controller: "ctrl",
    });
    const nonMatchingEvent = createEvent({
      timestamp: "2024-01-01T00:00:02Z",
      namespace: "other",
      controller: "other-ctrl",
    });
    mockBffFetch.mockResolvedValue({items: [matchingEvent, nonMatchingEvent]});

    const { result } = renderHook(
      () => useActivityFeed({ namespace: "ns", name: "ctrl" }),
      { wrapper: createWrapper() },
    );

    await waitFor(() => {
      expect(result.current.events.length).toBe(1);
      expect(result.current.events[0].controller).toBe("ctrl");
    });
  });
});

describe("useActivityFeed load more", () => {
  const firstPage = {
    items: [createEvent({ timestamp: "2024-01-02T00:00:00Z", message: "newest", namespace: "ns", controller: "ctrl" })],
    hasMore: true,
    nextCursor: "c1",
    retentionMode: "on",
    retentionDays: 30,
  };

  it("an expired cursor ends paging and sets the retention note", async () => {
    mockBffFetch.mockImplementation(async (path: string) => {
      if (path.includes("cursor=")) {
        throw new ApiError(410, "Varroa could not complete the request.", { error: "activity cursor expired" });
      }
      return firstPage;
    });
    const { result } = renderHook(() => useActivityFeed({ namespace: "ns", name: "ctrl" }), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    await act(async () => { await result.current.loadMore(); });

    expect(result.current.hasMore).toBe(false);
    expect(result.current.loadMoreNote).toBe(LOAD_MORE_EXPIRED);
    expect(result.current.isLoadingMore).toBe(false);
    expect(result.current.events).toHaveLength(1);

    const calls = mockBffFetch.mock.calls.length;
    await act(async () => { await result.current.loadMore(); });
    expect(mockBffFetch.mock.calls.length).toBe(calls);
  });

  it("a failed page keeps the cursor, sets the retry note, and a retry clears it", async () => {
    let cursorReads = 0;
    mockBffFetch.mockImplementation(async (path: string) => {
      if (path.includes("cursor=")) {
        cursorReads += 1;
        if (cursorReads === 1) {
          throw new ApiError(500, "Varroa could not complete the request.", { error: "failed to load activity" });
        }
        return {
          items: [createEvent({ timestamp: "2024-01-01T00:00:00Z", message: "older" })],
          hasMore: false,
          retentionMode: "on",
        };
      }
      return firstPage;
    });
    const { result } = renderHook(() => useActivityFeed({ namespace: "ns", name: "ctrl" }), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    await act(async () => { await result.current.loadMore(); });
    expect(result.current.hasMore).toBe(true);
    expect(result.current.loadMoreNote).toBe(LOAD_MORE_FAILED);

    await act(async () => { await result.current.loadMore(); });
    expect(result.current.loadMoreNote).toBeNull();
    expect(result.current.hasMore).toBe(false);
    expect(result.current.events.map((e) => e.message)).toContain("older");

    const cursorPaths = mockBffFetch.mock.calls
      .map((c) => c[0] as string)
      .filter((p) => p.includes("cursor="));
    const cursorPath = "/activity?cluster=&controller=ns%2Fctrl&cursor=c1";
    expect(cursorPaths).toEqual([cursorPath, cursorPath]);
  });

  it("an invalid cursor ends paging with the plain note", async () => {
    mockBffFetch.mockImplementation(async (path: string) => {
      if (path.includes("cursor=")) {
        throw new ApiError(400, "Varroa could not complete the request.", { error: "invalid cursor" });
      }
      return firstPage;
    });
    const { result } = renderHook(() => useActivityFeed({ namespace: "ns", name: "ctrl" }), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    await act(async () => { await result.current.loadMore(); });

    expect(result.current.hasMore).toBe(false);
    expect(result.current.loadMoreNote).toBe(LOAD_MORE_INVALID);
  });

  it("a scope change clears the note", async () => {
    mockBffFetch.mockImplementation(async (path: string) => {
      if (path.includes("cursor=")) {
        throw new ApiError(410, "Varroa could not complete the request.", { error: "activity cursor expired" });
      }
      return firstPage;
    });
    const { result, rerender } = renderHook(
      ({ name }: { name: string }) => useActivityFeed({ namespace: "ns", name }),
      { wrapper: createWrapper(), initialProps: { name: "a" } },
    );
    await waitFor(() => expect(result.current.hasMore).toBe(true));
    await act(async () => { await result.current.loadMore(); });
    expect(result.current.loadMoreNote).toBe(LOAD_MORE_EXPIRED);

    act(() => rerender({ name: "b" }));

    expect(result.current.loadMoreNote).toBeNull();
  });

  it("drops a load-more answer that lands after the scope changed", async () => {
    let rejectOld: (e: unknown) => void = () => {};
    mockBffFetch.mockImplementation((path: string) => {
      if (path.includes("cursor=old")) {
        return new Promise((_resolve, reject) => { rejectOld = reject; });
      }
      if (path.includes("ctrl-a")) {
        return Promise.resolve({ items: firstPage.items, hasMore: true, nextCursor: "old" });
      }
      return Promise.resolve({ items: firstPage.items, hasMore: true, nextCursor: "new" });
    });
    const { result, rerender } = renderHook(
      ({ name }: { name: string }) => useActivityFeed({ namespace: "ns", name }),
      { wrapper: createWrapper(), initialProps: { name: "ctrl-a" } },
    );
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    let pending: Promise<void> = Promise.resolve();
    act(() => { pending = result.current.loadMore(); });
    act(() => rerender({ name: "ctrl-b" }));
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    await act(async () => {
      rejectOld(new ApiError(410, "x", { error: "activity cursor expired" }));
      await pending;
    });

    expect(result.current.hasMore).toBe(true);
    expect(result.current.loadMoreNote).toBeNull();
    expect(result.current.isLoadingMore).toBe(false);
  });
});

describe("useActivityFeed range bounds", () => {
  it("a preset range sends start and no end to history and the live stream", async () => {
    renderHook(() => useActivityFeed(undefined, { range: "1h" }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalled();
    });
    const historyUrl = String(mockBffFetch.mock.calls[0][0]);
    expect(historyUrl).toMatch(/^\/activity\?start=/);
    expect(historyUrl).not.toContain("end=");

    const streamUrl = String(mockStreamUrl);
    expect(streamUrl).toBe("/api/v1/activity/stream" + historyUrl.slice("/activity".length));
    expect(streamUrl).not.toContain("end=");

    const start = new URL(streamUrl, "http://x").searchParams.get("start");
    expect(start).not.toBeNull();
    expect(Math.abs(Date.parse(start!) - (Date.now() - 36e5))).toBeLessThan(5000);
  });

  it("a preset range drops a stale end from the URL", async () => {
    renderHook(
      () =>
        useActivityFeed(undefined, {
          range: "1h",
          end: "2020-01-01T00:00:00.000Z",
        }),
      { wrapper: createWrapper() },
    );

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalled();
    });
    expect(String(mockBffFetch.mock.calls[0][0])).not.toContain("end=");
    expect(String(mockStreamUrl)).not.toContain("end=");
  });

  it("a custom range sends its start and end to the live stream", async () => {
    const start = new Date(Date.now() - 36e5).toISOString();
    const end = new Date(Date.now() + 36e5).toISOString();
    renderHook(() => useActivityFeed(undefined, { range: "custom", start, end }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockStreamUrl).not.toBeNull();
    });
    const params = new URL(String(mockStreamUrl), "http://x").searchParams;
    expect(params.get("start")).toBe(start);
    expect(params.get("end")).toBe(end);
  });

  it("a custom range whose end has passed opens no live stream", async () => {
    const end = new Date(Date.now() - 60e3).toISOString();
    const start = new Date(Date.parse(end) - 36e5).toISOString();
    renderHook(() => useActivityFeed(undefined, { range: "custom", start, end }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalled();
    });
    expect(String(mockBffFetch.mock.calls[0][0])).toContain("end=");
    expect(mockStreamUrl).toBeNull();
  });

  it("a custom range closes the live stream at its end", async () => {
    const end = new Date(Date.now() + 300).toISOString();
    const start = new Date(Date.parse(end) - 36e5).toISOString();
    renderHook(() => useActivityFeed(undefined, { range: "custom", start, end }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockStreamUrl).not.toBeNull();
    });
    await waitFor(() => expect(mockStreamUrl).toBeNull(), { timeout: 3000 });
  });

  it("a stream closed at its range end is not reported as an error", async () => {
    const end = new Date(Date.now() - 60e3).toISOString();
    const start = new Date(Date.parse(end) - 36e5).toISOString();
    mockSSEError = new Event("error");
    const { result } = renderHook(() => useActivityFeed(undefined, { range: "custom", start, end }), {
      wrapper: createWrapper(),
    });

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalled();
    });
    expect(result.current.error).toBeNull();
  });
});

