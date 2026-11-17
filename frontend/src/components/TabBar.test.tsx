import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { TabBar } from "./TabBar";

vi.mock("../hooks/useControllers", () => ({
  useControllers: vi.fn(),
}));

vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

import { useControllers } from "../hooks/useControllers";
import { usePermissions } from "../hooks/usePermissions";
import type { Permissions } from "../types/auth";

function renderTabBar(route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <TabBar />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

// mockPermissionsData sets the permissions read. Undefined data is a read
// that has not answered yet.
function mockPermissionsData(data: Permissions | undefined) {
  const answered = data !== undefined;
  vi.mocked(usePermissions).mockReturnValue({
    data,
    isLoading: false,
    error: null,
    isError: false,
    isSuccess: answered,
    isPending: !answered,
    status: answered ? "success" : "pending",
    dataUpdatedAt: 0,
    errorUpdatedAt: 0,
    failureCount: 0,
    failureReason: null,
    errorUpdateCount: 0,
    isFetched: answered,
    isFetchedAfterMount: answered,
    isFetching: false,
    isPlaceholderData: false,
    isPaused: false,
    isRefetching: false,
    isStale: false,
    isInitialLoading: false,
    isLoadingError: false,
    isRefetchError: false,
    fetchStatus: "idle",
    promise: Promise.resolve(data),
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof usePermissions>);
}

describe("TabBar", () => {
  beforeEach(() => {
    vi.mocked(useControllers).mockReturnValue({
      data: undefined,
      isLoading: false,
      error: null,
      isError: false,
      isSuccess: false,
      isPending: true,
      status: "pending",
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: false,
      isFetchedAfterMount: false,
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
    } as unknown as ReturnType<typeof useControllers>);

    mockPermissionsData({ global: { "*": { "*": true } }, scopes: [] });
  });

  it("renders four links with correct hrefs and a More button", () => {
    renderTabBar("/");
    expect(screen.getByRole("link", { name: "Overview" })).toHaveAttribute("href", "/");
    expect(screen.getByRole("link", { name: "Controllers" })).toHaveAttribute("href", "/controllers");
    expect(screen.getByRole("link", { name: "Clusters" })).toHaveAttribute("href", "/clusters");
    expect(screen.getByRole("link", { name: "Activity" })).toHaveAttribute("href", "/activity");

    const moreBtn = screen.getByRole("button", { name: "More" });
    expect(moreBtn).toHaveAttribute("aria-haspopup", "dialog");
    expect(moreBtn).toHaveAttribute("aria-expanded", "false");
  });

  it("flips More aria-expanded on click", () => {
    renderTabBar("/");
    const moreBtn = screen.getByRole("button", { name: "More" });
    expect(moreBtn).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(moreBtn);
    expect(moreBtn).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(moreBtn);
    expect(moreBtn).toHaveAttribute("aria-expanded", "false");
  });

  it("shows controller count badge when useControllers has data", () => {
    vi.mocked(useControllers).mockReturnValue({
      data: [
        { cluster: "c1", name: "n1", namespace: "ns1", phase: "running", endpoint: "https://e1", miteConnected: false },
        { cluster: "c2", name: "n2", namespace: "ns2", phase: "running", endpoint: "https://e2", miteConnected: false },
        { cluster: "c3", name: "n3", namespace: "ns3", phase: "running", endpoint: "https://e3", miteConnected: false },
        { cluster: "c4", name: "n4", namespace: "ns4", phase: "running", endpoint: "https://e4", miteConnected: false },
        { cluster: "c5", name: "n5", namespace: "ns5", phase: "running", endpoint: "https://e5", miteConnected: false },
      ],
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
    } as unknown as ReturnType<typeof useControllers>);

    renderTabBar("/");
    expect(screen.getByText("5")).toBeInTheDocument();
  });

  it("hides controller count badge when useControllers data is undefined", () => {
    renderTabBar("/");
    expect(screen.queryByText("5")).not.toBeInTheDocument();
  });

  it("sets aria-current on the active tab", () => {
    renderTabBar("/clusters");
    expect(screen.getByRole("link", { name: "Clusters" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveAttribute("aria-current", "page");
  });

  it("Overview uses end prop (not active on /controllers)", () => {
    renderTabBar("/controllers");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveAttribute("aria-current", "page");
  });

  it("accessible names resolve uniquely (icons are aria-hidden)", () => {
    renderTabBar("/");
    expect(screen.getByRole("link", { name: "Controllers" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Clusters" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Activity" })).toBeInTheDocument();
  });

  it("hides the Controllers tab for a pipelines-only caller and sends no controllers read", () => {
    mockPermissionsData({
      global: {},
      scopes: [],
      tenants: [{ tenant: "team-a", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    vi.mocked(useControllers).mockClear();
    renderTabBar("/");
    expect(screen.queryByRole("link", { name: "Controllers" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Clusters" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Activity" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "More" })).toBeInTheDocument();
    expect(vi.mocked(useControllers)).toHaveBeenCalledWith(false);
    expect(vi.mocked(useControllers)).not.toHaveBeenCalledWith(true);
  });

  it("shows the Controllers tab for a namespace-scoped controllers:read grant", () => {
    mockPermissionsData({
      global: {},
      scopes: [{ namespaces: ["ns-a"], hasControllerSelector: false, capabilities: { controllers: { read: true } } }],
    });
    vi.mocked(useControllers).mockClear();
    renderTabBar("/");
    expect(screen.getByRole("link", { name: "Controllers" })).toHaveAttribute("href", "/controllers");
    expect(vi.mocked(useControllers)).toHaveBeenCalledWith(true);
  });

  it("hides the Controllers tab while the permissions read has not answered", () => {
    mockPermissionsData(undefined);
    renderTabBar("/");
    expect(screen.queryByRole("link", { name: "Controllers" })).not.toBeInTheDocument();
  });

  it("returns focus to the More button when the sheet closes via Escape", () => {
    renderTabBar("/");
    const moreBtn = screen.getByRole("button", { name: "More" });

    // Open the sheet
    fireEvent.click(moreBtn);
    // Sheet is open — first link has focus
    const firstLink = screen.getByRole("link", { name: "Operations" });
    expect(firstLink).toBeInTheDocument();

    // Press Escape on the dialog
    const dialog = screen.getByRole("dialog");
    fireEvent.keyDown(dialog, { key: "Escape" });

    // Focus returns to the More button
    expect(document.activeElement).toBe(moreBtn);
  });
});
