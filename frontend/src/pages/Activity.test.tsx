import fs from "fs";
import path from "path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ApiError } from "../hooks/useApi";

// Mock useApi bffFetch. ApiError and isRecordStoreUnreachable stay real.
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
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

function renderActivity(initialEntries?: string[]) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={initialEntries}>
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

  it("says the record store did not answer, and offers Retry", async () => {
    mockBffFetch.mockImplementation((url: string) =>
      url.startsWith("/activity")
        ? Promise.reject(new ApiError(503, "record store unreachable", { error: "record store unreachable", code: "record_store_unreachable" }))
        : Promise.resolve({ items: [] }),
    );
    renderActivity();

    expect(
      await screen.findByText("The build record store did not answer, so activity is not available."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText(/Failed to load activity/)).not.toBeInTheDocument();
  });

  it("keeps each filter inside its grid column", () => {
    const css = fs.readFileSync(path.join(__dirname, "Activity.module.css"), "utf-8");
    expect(css).toMatch(/\.filterField \{[^}]*grid-template-columns: minmax\(0, 1fr\)/);
    expect(css).toMatch(/\.filterField \{[^}]*min-width: 0/);
    expect(css).toMatch(/\.filterField select, \.filterField input \{[^}]*width: 100%/);
  });
  describe("Controller filter", () => {
    const sameNamed = {
      "/controllers": { items: [
        { name: "cell-0", namespace: "varroa-system", cluster: "core", phase: "Running" },
        { name: "cell-0", namespace: "varroa-system", cluster: "cluster-b", phase: "Running" },
      ] },
    };

    function mockControllers() {
      mockBffFetch.mockImplementation((url: string) =>
        Promise.resolve(url in sameNamed ? sameNamed[url as keyof typeof sameNamed] : { items: [] }),
      );
    }

    function controllerSelect() {
      return screen.getByRole("combobox", { name: "Controller" }) as HTMLSelectElement;
    }

    it("gives same-named controllers on two clusters distinct option values", async () => {
      mockControllers();
      renderActivity();

      await waitFor(() => expect(controllerSelect().options.length).toBe(3));
      expect(Array.from(controllerSelect().options).map((o) => o.value)).toEqual([
        "",
        "core/varroa-system/cell-0",
        "cluster-b/varroa-system/cell-0",
      ]);
    });

    it("writes the cluster and controller parameters and sends both on history and the live stream", async () => {
      mockControllers();
      renderActivity();
      await waitFor(() => expect(controllerSelect().options.length).toBe(3));

      fireEvent.change(controllerSelect(), { target: { value: "cluster-b/varroa-system/cell-0" } });

      expect(controllerSelect().value).toBe("cluster-b/varroa-system/cell-0");
      expect(screen.getByRole("button", { name: "cluster: cluster-b x" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "controller: varroa-system/cell-0 x" })).toBeInTheDocument();
      await waitFor(() =>
        expect(mockBffFetch).toHaveBeenCalledWith("/activity?cluster=cluster-b&controller=varroa-system%2Fcell-0"),
      );
      expect(String(mockUseEventStream.mock.calls[mockUseEventStream.mock.calls.length - 1]?.[0])).toBe(
        "/api/v1/activity/stream?cluster=cluster-b&controller=varroa-system%2Fcell-0",
      );
    });

    it("selects the option that the cluster and controller parameters name", async () => {
      mockControllers();
      renderActivity(["/activity?cluster=cluster-b&controller=varroa-system/cell-0"]);
      await waitFor(() => expect(controllerSelect().options.length).toBe(3));

      expect(controllerSelect().value).toBe("cluster-b/varroa-system/cell-0");
    });

    it("a controller parameter that no listed controller names shows a disabled placeholder", async () => {
      mockControllers();
      renderActivity(["/activity?controller=varroa-system/cell-0"]);
      await waitFor(() => expect(controllerSelect().options.length).toBe(4));

      expect(controllerSelect().selectedOptions[0].textContent).toBe("Not in the controller list");
      expect(controllerSelect().selectedOptions[0].disabled).toBe(true);
      expect(Array.from(controllerSelect().options).map((o) => o.value)).not.toContain("/varroa-system/cell-0");
    });

    it("All clears the cluster and controller parameters", async () => {
      mockControllers();
      renderActivity(["/activity?cluster=cluster-b&controller=varroa-system/cell-0"]);
      await waitFor(() => expect(controllerSelect().options.length).toBe(3));

      fireEvent.change(controllerSelect(), { target: { value: "" } });

      await waitFor(() => expect(mockBffFetch).toHaveBeenLastCalledWith("/activity"));
      expect(controllerSelect().value).toBe("");
    });

    it("clearing the cluster chip also clears the controller", async () => {
      mockControllers();
      renderActivity(["/activity?cluster=cluster-b&controller=varroa-system/cell-0"]);
      await waitFor(() => expect(controllerSelect().options.length).toBe(3));

      fireEvent.click(screen.getByRole("button", { name: "cluster: cluster-b x" }));

      await waitFor(() => expect(controllerSelect().value).toBe(""));
      expect(screen.queryByRole("button", { name: /controller: / })).toBeNull();
    });
  });
});
