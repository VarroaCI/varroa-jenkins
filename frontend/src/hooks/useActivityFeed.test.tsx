import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useActivityFeed } from "./useActivityFeed";
import type { ActivityEvent } from "../types";
import type { ReactNode } from "react";

// ── Mocks ──────────────────────────────────────────────────────────────────

const mockBffFetch = vi.fn();
vi.mock("./useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
}));

let mockLastEvent: { type: string; data: ActivityEvent } | null = null;
let mockReadyState = "closed";
let mockSSEError: Event | null = null;
let mockStreamUrl: unknown = null;

vi.mock("./useEventStream", () => ({
  useEventStream: (...args: unknown[]) => {
    mockStreamUrl = args[0];
    return {
    lastEvent: mockLastEvent,
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
  mockLastEvent = null;
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
    const { result, rerender } = renderHook(() => useActivityFeed(), {
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
      mockLastEvent = { type: "activity", data: sseEvent };
    });
    // Force re-render so useEffect picks up the new mockLastEvent
    rerender();

    await waitFor(() => {
      expect(result.current.pendingCount).toBe(1);
    });
    expect(result.current.events.length).toBe(0);
  });

  it("resume flushes pendingQueue into buffer", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    const { result, rerender } = renderHook(() => useActivityFeed(), {
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
      mockLastEvent = { type: "activity", data: sseEvent };
    });
    rerender();

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
    const start = "2026-10-30T00:00:00.000Z";
    const end = "2026-10-30T06:00:00.000Z";
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
});

