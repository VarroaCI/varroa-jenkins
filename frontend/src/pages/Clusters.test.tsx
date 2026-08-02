import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import Clusters from "./Clusters";

vi.mock("../hooks/useClusters", () => ({
  useClusters: vi.fn(),
}));

import { useClusters } from "../hooks/useClusters";

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <Clusters />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

const coreCluster = {
  name: "core",
  core: true,
  healthy: true,
  lastHeartbeat: new Date(Date.now() - 120000).toISOString(),
  operatorVersion: "1.0.0",
  k8sVersion: "1.28",
  controllerCount: 5,
  connectedCount: 4,
  state: "active",
};

const hiveCluster = {
  name: "dev-cluster",
  core: false,
  healthy: false,
  lastHeartbeat: new Date(Date.now() - 180000).toISOString(),
  operatorVersion: "1.0.0",
  k8sVersion: "1.27",
  controllerCount: 3,
  connectedCount: 0,
  state: "active",
};

describe("Clusters", () => {
  beforeEach(() => {
    vi.mocked(useClusters).mockReturnValue({
      data: [coreCluster, hiveCluster],
      isLoading: false,
      error: null,
      isError: false,
      isSuccess: true,
      status: "success",
      isPending: false,
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: true,
      isFetchedAfterMount: true,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: false,
      isRefetchError: false,
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as any);
  });

  it("renders core first with core tag", () => {
    renderPage();
    const coreTexts = screen.getAllByText("core");
    expect(coreTexts.length).toBe(2); // "core" in cluster name column + "core" tag pill
  });

  it("shows unhealthy pill", () => {
    renderPage();
    expect(screen.getByText("Unhealthy")).toBeInTheDocument();
  });

  it("shows heartbeat age", () => {
    renderPage();
    expect(screen.getByText("2m ago")).toBeInTheDocument(); // core, 120s old
    expect(screen.getByText("3m ago")).toBeInTheDocument(); // agent, 180s old
  });

  it("renders drill-down link with cluster param", () => {
    renderPage();
    const links = screen.getAllByText("View controllers ›");
    expect(links.length).toBe(2);
    expect(links[0].closest("a")).toHaveAttribute("href", "/controllers?cluster=core");
  });

  it("shows empty state when no clusters", () => {
    vi.mocked(useClusters).mockReturnValue({
      data: [],
      isLoading: false,
      error: null,
      isError: false,
      isSuccess: true,
      status: "success",
      isPending: false,
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: true,
      isFetchedAfterMount: true,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: false,
      isRefetchError: false,
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as any);
    renderPage();
    expect(screen.getByText("No clusters registered")).toBeInTheDocument();
  });

  it("shows error banner on error", () => {
    vi.mocked(useClusters).mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new Error("Network error"),
      isError: true,
      status: "error",
      isPending: false,
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 1,
      failureReason: new Error("Network error"),
      errorUpdateCount: 1,
      isFetched: true,
      isFetchedAfterMount: true,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: true,
      isRefetchError: false,
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as any);
    renderPage();
    expect(screen.getByText(/Network error/)).toBeInTheDocument();
  });
});
