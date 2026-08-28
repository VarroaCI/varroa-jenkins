import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

// Mock useApi bffFetch
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: () => null,
}));

vi.mock("../api/client", () => ({
  BFF_BASE: "/api/v1",
  listClusters: vi.fn(),
}));

// Mock useEventStream
const mockUseEventStream = vi.fn();
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: (...args: unknown[]) => mockUseEventStream(...args),
}));

// Mock @tanstack/react-virtual
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

import Activity from "./Activity";
import { createActivityEvent } from "../test/factories";

function renderActivity() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <Activity />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockBffFetch.mockReset();
  mockUseEventStream.mockReset();
  mockUseEventStream.mockReturnValue({
    lastEvent: null,
    readyState: "closed",
    error: null,
    close: vi.fn(),
  });
});

describe("Activity page", () => {
  it("renders page heading and description", () => {
    mockBffFetch.mockResolvedValue({items: []});
    renderActivity();
    expect(screen.getByText("Activity")).toBeInTheDocument();
    expect(
      screen.getByText(/Real-time event feed/),
    ).toBeInTheDocument();
  });

  it("shows the controller picker", async () => {
    // Mock controllers endpoint and activity endpoint
    mockBffFetch.mockImplementation((url: string) => {
      if (url === "/controllers") {
        return Promise.resolve({items: [
          { name: "ctrlA", namespace: "ns1", phase: "Running" },
          { name: "ctrlB", namespace: "ns2", phase: "Connected" },
        ]});
      }
      return Promise.resolve({items: []});
    });
    renderActivity();

    await waitFor(() => {
			expect(screen.getByRole("combobox", { name: "Controller" })).toBeInTheDocument();
    });
  });

  it("renders events from initial fetch", async () => {
    const events = [
      createActivityEvent({ type: "connected", message: "Controller connected", controller: "test-ctrl" }),
      createActivityEvent({ type: "phase", message: "Phase changed to Running", source: "mite" }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderActivity();

    await waitFor(() => {
			expect(screen.getAllByText("connected").length).toBeGreaterThan(0);
      expect(screen.getByText("Controller connected", { exact: false })).toBeInTheDocument();
    });
  });

  it("shows empty state when no events exist", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    renderActivity();
    await screen.findByText(/No activity yet/);
  });

  it("renders controller links for events with controller", async () => {
    const events = [
      createActivityEvent({
        type: "phase",
        message: "Updated",
        controller: "my-ctrl",
        namespace: "my-ns",
        cluster: "core",
      }),
    ];
    mockBffFetch.mockResolvedValue({items: events});
    renderActivity();

    await screen.findByText("my-ctrl");
    const link = screen.getByText("my-ctrl");
    expect(link.tagName).toBe("A");
    expect(link.getAttribute("href")).toContain("my-ctrl");
  });

  it("shows connection status indicator", async () => {
    mockBffFetch.mockResolvedValue({items: []});
    mockUseEventStream.mockReturnValue({
      lastEvent: null,
      readyState: "open",
      error: null,
      close: vi.fn(),
    });
    renderActivity();

    await screen.findByText("Live");
  });

  describe("Namespace filter", () => {
    it("renders a namespace select with options derived from controllers and events", async () => {
      mockBffFetch.mockImplementation((url: string) => {
        if (url === "/controllers") {
          return Promise.resolve({items: [
            { name: "ctrlA", namespace: "ns1", phase: "Running" },
            { name: "ctrlB", namespace: "ns2", phase: "Connected" },
          ]});
        }
        return Promise.resolve({items: []});
      });
      renderActivity();

      await waitFor(() => {
        expect(screen.getByRole("combobox", { name: "Namespace" })).toBeInTheDocument();
      });

      // Wait for controllers query to resolve and populate the options
      await waitFor(() => {
        const options = screen.getAllByRole("option");
        expect(options.length).toBeGreaterThan(1);
      });

      const options = screen.getAllByRole("option");
      expect(options.map((o) => o.textContent)).toEqual(
        expect.arrayContaining(["ns1", "ns2"]),
      );
    });
  });
});
