import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useActivityFeed } from "./useActivityFeed";
import type { ActivityEvent, ActivityFilters, ActivityPage } from "../types";
import type { ReactNode } from "react";

// ── Mocks ──────────────────────────────────────────────────────────────────
// Unlike useActivityFeed.test.tsx, this file does NOT mock ./useEventStream:
// the point is to prove that a NAMED `activity` SSE frame dispatched on a real
// EventSource (via addEventListener) flows all the way into the feed's events.

const mockBffFetch = vi.fn();
vi.mock("./useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
}));

// EventSource stub: the hook sets `onmessage` and, for named events, calls
// `addEventListener(name, handler)`. emitNamedEvent drives the latter.
class MockEventSource {
  static instances: MockEventSource[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  listeners = new Map<string, (e: MessageEvent) => void>();
  url: string;
  wasClosed = false;

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }
  close() {
    this.wasClosed = true;
  }
  addEventListener(type: string, listener: (e: MessageEvent) => void) {
    this.listeners.set(type, listener);
  }
  emitOpen() {
    this.onopen?.();
  }
  emitNamedEvent(type: string, data: string) {
    this.listeners.get(type)?.(new MessageEvent(type, { data }));
  }
}

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
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource as unknown as typeof EventSource);
  mockBffFetch.mockReset();
  mockBffFetch.mockImplementation((path: string) => {
    // Ticket mint for the stream; anything else is the backfill page fetch.
    if (path === "/stream/ticket") {
      return Promise.resolve({ ticket: "vst_abc", expiresInSeconds: 30 });
    }
    return Promise.resolve({ items: [] });
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("useActivityFeed live ingest (named `activity` frames)", () => {
  it("lands an in-scope named activity frame in `events`", async () => {
    const { result } = renderHook(
      () => useActivityFeed({ namespace: "ns", name: "ctrl" }),
      { wrapper: createWrapper() },
    );
    // Stream connects after the ticket mint resolves.
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];

    const inScope = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      namespace: "ns",
      controller: "ctrl",
      message: "in scope",
    });
    act(() => {
      es.emitOpen();
      es.emitNamedEvent("activity", JSON.stringify(inScope));
    });

    await waitFor(() => {
      expect(result.current.events.some((e) => e.message === "in scope")).toBe(true);
    });
  });

  it("filters an out-of-scope named activity frame", async () => {
    const { result } = renderHook(
      () => useActivityFeed({ namespace: "ns", name: "ctrl" }),
      { wrapper: createWrapper() },
    );
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];

    const outOfScope = createEvent({
      timestamp: "2024-01-01T00:00:02Z",
      namespace: "other",
      controller: "other-ctrl",
      message: "out of scope",
    });
    act(() => {
      es.emitNamedEvent("activity", JSON.stringify(outOfScope));
    });

    // Give the ingest effect a tick; nothing should be added to the buffer.
    await new Promise((r) => setTimeout(r, 50));
    expect(result.current.events.length).toBe(0);
  });

  it("does not re-ingest the retained lastEvent after filters change, but inserts a genuinely new event", async () => {
    const { result, rerender } = renderHook(
      (props: { filters: ActivityFilters }) =>
        useActivityFeed({ namespace: "ns", name: "ctrl" }, props.filters),
      {
        initialProps: { filters: { type: "build.completed" } },
        wrapper: createWrapper(),
      },
    );
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es1 = MockEventSource.instances[0];

    // Scoped feed ingests an in-scope event.
    const first = createEvent({
      timestamp: "2024-01-01T00:00:01Z",
      namespace: "ns",
      controller: "ctrl",
      type: "build.completed",
      message: "first",
    });
    act(() => {
      es1.emitOpen();
      es1.emitNamedEvent("activity", JSON.stringify(first));
    });
    await waitFor(() => {
      expect(result.current.events.some((e) => e.message === "first")).toBe(true);
    });

    // Change filters → new queryIdentity → stream URL changes. The buffer is
    // cleared, but useEventStream still retains the PREVIOUS stream's lastEvent
    // (first) until the new stream delivers a frame.
    act(() => {
      rerender({ filters: { type: "controller.updated" } });
    });

    // The retained stale frame must NOT be re-inserted into the fresh buffer.
    await new Promise((r) => setTimeout(r, 50));
    expect(result.current.events).toHaveLength(0);

    // The new stream reconnects; a genuinely new frame IS inserted.
    await waitFor(() =>
      expect(MockEventSource.instances.length).toBeGreaterThanOrEqual(2),
    );
    const es2 = MockEventSource.instances[MockEventSource.instances.length - 1];
    const second = createEvent({
      timestamp: "2024-01-01T00:00:02Z",
      namespace: "ns",
      controller: "ctrl",
      type: "controller.updated",
      message: "second",
    });
    act(() => {
      es2.emitOpen();
      es2.emitNamedEvent("activity", JSON.stringify(second));
    });
    await waitFor(() => {
      expect(result.current.events.some((e) => e.message === "second")).toBe(true);
    });
    expect(result.current.events.some((e) => e.message === "first")).toBe(false);
  });

  it("resets pagination on queryIdentity change: hasMore false and loadMore a no-op until the new backfill seeds", async () => {
    // Hold the new identity's backfill open so we can observe the window in
    // which pagination must already be reset (the old cursor must never be
    // fired against the new query).
    let resolveSecondBackfill!: (page: ActivityPage) => void;
    const secondBackfill = new Promise<ActivityPage>((resolve) => {
      resolveSecondBackfill = resolve;
    });

    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/stream/ticket") {
        return Promise.resolve({ ticket: "vst_abc", expiresInSeconds: 30 });
      }
      if (path.includes("ctrl-a")) {
        // First identity: a paged backfill, so hasMore/nextCursor are live.
        return Promise.resolve({
          items: [
            createEvent({
              timestamp: "2024-01-01T00:00:01Z",
              namespace: "ns",
              controller: "ctrl-a",
              message: "page one",
            }),
          ],
          hasMore: true,
          nextCursor: "page-2",
          retentionMode: "off" as const,
        } satisfies ActivityPage);
      }
      return secondBackfill;
    });

    const { result, rerender } = renderHook(
      (props: { name: string }) =>
        useActivityFeed({ namespace: "ns", name: props.name }),
      { initialProps: { name: "ctrl-a" }, wrapper: createWrapper() },
    );

    // First backfill seeds pagination.
    await waitFor(() => expect(result.current.hasMore).toBe(true));

    // Change the scope → new queryIdentity. The clear effect must reset
    // pagination so a stale "Load more" can never fire the OLD cursor against
    // the NEW query while the new backfill is still in flight.
    act(() => {
      rerender({ name: "ctrl-b" });
    });
    expect(result.current.hasMore).toBe(false);

    // Until the new backfill seeds, loadMore is a no-op: no cursor fetch fires
    // and pagination stays reset.
    const callsBefore = mockBffFetch.mock.calls.length;
    await act(async () => {
      await result.current.loadMore();
    });
    expect(mockBffFetch.mock.calls.length).toBe(callsBefore);
    expect(result.current.hasMore).toBe(false);

    // The new backfill seeds → pagination is driven by the NEW page.
    await act(async () => {
      resolveSecondBackfill({
        items: [
          createEvent({
            timestamp: "2024-01-01T00:00:03Z",
            namespace: "ns",
            controller: "ctrl-b",
            message: "new page one",
          }),
        ],
        hasMore: true,
        nextCursor: "new-page-2",
        retentionMode: "off" as const,
      } satisfies ActivityPage);
    });
    await waitFor(() => expect(result.current.hasMore).toBe(true));
    expect(result.current.events.some((e) => e.message === "new page one")).toBe(true);
  });
});
