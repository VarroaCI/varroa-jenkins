import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import Activity from "./Activity";
import { createActivityEvent } from "../test/factories";

// This file keeps the real useEventStream. It counts the history reads and the
// live streams that one Activity page opens.

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
  listClusters: vi.fn(() => Promise.resolve([])),
}));

vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: (opts: { count: number; estimateSize: (i: number) => number }) => {
    const items: Array<{key: number; index: number; start: number; end: number; size: number; measureElement: () => void}> = [];
    for (let i = 0; i < opts.count; i++) {
      items.push({
        key: i,
        index: i,
        start: i * opts.estimateSize(i),
        end: (i + 1) * opts.estimateSize(i),
        size: opts.estimateSize(i),
        measureElement: () => {},
      });
    }
    return {
      getVirtualItems: () => items,
      getTotalSize: () => opts.count * opts.estimateSize(0),
      scrollToIndex: vi.fn(),
      measureElement: vi.fn(),
    };
  },
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

function renderActivity() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/activity?range=1h"]}>
        <Activity />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource as unknown as typeof EventSource);
  mockBffFetch.mockReset();
  mockBffFetch.mockImplementation((path: string) => {
    if (path === "/stream/ticket") {
      return Promise.resolve({ ticket: "vst_abc", expiresInSeconds: 30 });
    }
    return Promise.resolve({ items: [] });
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Activity page feed", () => {
  it("reads history once and opens one live stream", async () => {
    renderActivity();
    await screen.findByText(/No activity yet/);
    // Wait on the ticket count, which a duplicate feed would double, so a
    // second stream cannot resolve after the assertions.
    await waitFor(() => {
      const tickets = mockBffFetch.mock.calls.filter(([p]) => p === "/stream/ticket");
      expect(tickets).toHaveLength(1);
      expect(MockEventSource.instances).toHaveLength(1);
    });
    const historyCalls = mockBffFetch.mock.calls.filter(
      ([p]) => typeof p === "string" && p.startsWith("/activity"),
    );
    expect(historyCalls).toHaveLength(1);
  });

  it("shows every event of a live burst", async () => {
    renderActivity();
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];

    act(() => {
      es.emitOpen();
      es.emitNamedEvent("activity", JSON.stringify(createActivityEvent({
        timestamp: "2024-01-01T00:00:01Z",
        message: "burst alpha",
      })));
      es.emitNamedEvent("activity", JSON.stringify(createActivityEvent({
        timestamp: "2024-01-01T00:00:02Z",
        message: "burst beta",
      })));
      es.emitNamedEvent("activity", JSON.stringify(createActivityEvent({
        timestamp: "2024-01-01T00:00:03Z",
        message: "burst gamma",
      })));
    });

    expect(await screen.findByText(/burst alpha/)).toBeInTheDocument();
    expect(screen.getByText(/burst beta/)).toBeInTheDocument();
    expect(screen.getByText(/burst gamma/)).toBeInTheDocument();
  });
});
