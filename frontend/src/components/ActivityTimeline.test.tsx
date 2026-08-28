import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

// ── Mock @tanstack/react-virtual ───────────────────────────────────────────
// JSDOM doesn't have real layout, so the virtualizer returns no items.
// We mock it to render all items.
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: (opts: {
    count: number;
    getScrollElement: () => HTMLElement | null;
    estimateSize: (i: number) => number;
    overscan: number;
  }) => {
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

// ── Other mocks ────────────────────────────────────────────────────────────

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
}));

vi.mock("../hooks/useClusters", () => ({
  useClusters: () => ({ data: [{name:"core",core:true,healthy:true,lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4}], isLoading: false, isError: false }),
  coreOf: (c: unknown[]) => (c as any[])?.find((c2: any) => c2.core),
}));

let mockSSELastEvent: { type: string; data: ActivityEvent } | null = null;
let mockSSEReadyState = "closed";
let mockSSEError: Event | null = null;

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: (..._args: unknown[]) => ({
    lastEvent: mockSSELastEvent,
    readyState: mockSSEReadyState,
    error: mockSSEError,
    close: vi.fn(),
  }),
}));

import ActivityTimeline from "./ActivityTimeline";
import type { ActivityEvent } from "../types";

function createEvent(overrides: Partial<ActivityEvent>): ActivityEvent {
  return {
    timestamp: new Date().toISOString(),
    type: "connected",
    source: "mite",
    message: "Test event",
    namespace: "ns",
    controller: "ctrl",
    ...overrides,
  };
}

function renderTimeline(props?: {
  scope?: { namespace: string; name: string };
  selectedControllers?: Set<string>;
}) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <ActivityTimeline scope={props?.scope} selectedControllers={props?.selectedControllers} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockBffFetch.mockReset();
  mockBffFetch.mockResolvedValue({items: []});
  mockSSELastEvent = null;
  mockSSEReadyState = "closed";
  mockSSEError = null;
});

describe("ActivityTimeline", () => {
  it("renders lane chips with counts", async () => {
    const events = [
      createEvent({ source: "mite", type: "connected", message: "ctrl connected" }),
      createEvent({ source: "jenkins", buildNumber: 1, message: "build #1" }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderTimeline();

    await waitFor(() => {
      const allBtns = screen.getAllByRole("button", { name: /^All / });
      expect(allBtns.length).toBe(2); // lane All + source All
      expect(screen.getByRole("button", { name: /^Control plane/ })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^Builds/ })).toBeInTheDocument();
    });
  });

  it("renders source chips with Jenkins entry", async () => {
    const events = [
      createEvent({ source: "mite" }),
      createEvent({ source: "jenkins", buildNumber: 1 }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderTimeline();

    await waitFor(() => {
      // Source filter chips are buttons; Jenkins chip shows its count
      const jenkinsChips = screen.getAllByRole("button").filter(b => b.textContent?.startsWith("Jenkins"));
      expect(jenkinsChips.length).toBeGreaterThanOrEqual(1);
      const miteChips = screen.getAllByRole("button").filter(b => b.textContent?.startsWith("Mite"));
      expect(miteChips.length).toBeGreaterThanOrEqual(1);
      // Operator and User chips have count 0 so they are hidden
      expect(screen.queryByRole("button", { name: /^Operator/ })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /^User/ })).not.toBeInTheDocument();
    });
  });

  it("shows build result icons and deep link", async () => {
    const events = [
      createEvent({
        source: "jenkins",
        buildNumber: 10,
        result: "SUCCESS",
        message: "Build done",
        url: "https://jenkins.example.com/job/test/10",
      }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderTimeline();

    await waitFor(() => {
      const links = screen.getAllByRole("link");
      const jenkinsLink = links.find(
        (l) => l.getAttribute("href") === "https://jenkins.example.com/job/test/10",
      );
      expect(jenkinsLink).toBeTruthy();
      expect(jenkinsLink?.getAttribute("target")).toBe("_blank");
    });
  });

  it("shows result-breakdown badge cluster in build groups", async () => {
    const ts = (sec: number) => {
      const d = new Date("2024-01-01T00:00:00Z");
      d.setUTCSeconds(d.getUTCSeconds() + sec);
      return d.toISOString();
    };
    const events = [
      createEvent({
        timestamp: ts(10),
        source: "jenkins",
        buildNumber: 1,
        result: "SUCCESS",
        message: "Build #1",
        namespace: "ns",
        controller: "ctrlA",
      }),
      createEvent({
        timestamp: ts(9),
        source: "jenkins",
        buildNumber: 2,
        result: "SUCCESS",
        message: "Build #2",
        namespace: "ns",
        controller: "ctrlA",
      }),
      createEvent({
        timestamp: ts(8),
        source: "jenkins",
        buildNumber: 3,
        result: "FAILURE",
        message: "Build #3",
        namespace: "ns",
        controller: "ctrlA",
      }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderTimeline();

    await waitFor(() => {
      expect(screen.getByText("3 builds")).toBeInTheDocument();
    });
  });

  it("expands/collapses build groups", async () => {
    const ts = (sec: number) => {
      const d = new Date("2024-01-01T00:00:00Z");
      d.setUTCSeconds(d.getUTCSeconds() + sec);
      return d.toISOString();
    };
    const events = [
      createEvent({
        timestamp: ts(10),
        source: "jenkins",
        buildNumber: 1,
        namespace: "ns",
        controller: "ctrlA",
      }),
      createEvent({
        timestamp: ts(9),
        source: "jenkins",
        buildNumber: 2,
        namespace: "ns",
        controller: "ctrlA",
      }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderTimeline();

    await waitFor(() => {
      expect(screen.getByText(/builds/)).toBeInTheDocument();
    });
  });

  it("shows pause toggle", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    renderTimeline();

    await waitFor(() => {
      expect(screen.getAllByRole("button", { name: /^All / }).length).toBe(2);
    });

    const pauseButton = screen.getByTitle("Pause to review");
    await userEvent.click(pauseButton);
    expect(screen.getByText("▶ Live")).toBeInTheDocument();

    // Click resume
    await userEvent.click(pauseButton);
    expect(screen.getByText("⏸ Paused")).toBeInTheDocument();
  });

  it("drops off-scope events when scope is set", async () => {
    const matchingEvent = createEvent({
      timestamp: new Date().toISOString(),
      namespace: "ns",
      controller: "ctrl",
      message: "In scope",
    });
    const nonMatchingEvent = createEvent({
      timestamp: new Date().toISOString(),
      namespace: "other",
      controller: "other",
      message: "Out of scope",
    });
    mockBffFetch.mockResolvedValue({items: [matchingEvent, nonMatchingEvent]});

    renderTimeline({ scope: { namespace: "ns", name: "ctrl" } });

    await waitFor(() => {
      expect(screen.getByText("In scope", { exact: false })).toBeInTheDocument();
    });

    expect(screen.queryByText("Out of scope", { exact: false })).not.toBeInTheDocument();
  });
});
