import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { Sidebar } from "./Sidebar";

// Mock usePermissions so we can control which nav items are visible
vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

// Mock useControllers so we can control the badge value
vi.mock("../hooks/useControllers", () => ({
  useControllers: vi.fn(),
}));

import { usePermissions } from "../hooks/usePermissions";
import { useControllers } from "../hooks/useControllers";

function renderSidebar(route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Sidebar />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

function renderSidebarCollapsed(route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Sidebar collapsed={true} />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("Sidebar", () => {
  beforeEach(() => {
    vi.mocked(usePermissions).mockReturnValue({
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
    } as unknown as ReturnType<typeof usePermissions>);

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
  });

  it("renders the app logo and brand name", () => {
    renderSidebar("/");
    expect(screen.getByText("Varroa")).toBeInTheDocument();
    expect(screen.getByText("Jenkins control plane")).toBeInTheDocument();
  });

  it("renders group labels Operate, Brood, Manage", () => {
    renderSidebar("/");
    expect(screen.getByText("Operate")).toBeInTheDocument();
    expect(screen.getByText("Brood")).toBeInTheDocument();
    expect(screen.getByText("Manage")).toBeInTheDocument();
  });

  it("does NOT render retired group labels Configuration, Access, Administration", () => {
    renderSidebar("/");
    expect(screen.queryByText("Configuration")).not.toBeInTheDocument();
    expect(screen.queryByText("Access")).not.toBeInTheDocument();
    expect(screen.queryByText("Administration")).not.toBeInTheDocument();
  });

  it("labels the Brood items Operations (linked to /brood-operations) and Schedules (linked to /brood-schedules)", () => {
    renderSidebar("/");
    // Operations is a link, not a section label — prove by href
    const opsLink = screen.getByRole("link", { name: "Operations" });
    expect(opsLink).toHaveAttribute("href", "/brood-operations");
    const schedLink = screen.getByRole("link", { name: "Schedules" });
    expect(schedLink).toHaveAttribute("href", "/brood-schedules");
    // Old prefixed labels are gone
    expect(screen.queryByText("Brood Operations")).not.toBeInTheDocument();
    expect(screen.queryByText("Brood Schedules")).not.toBeInTheDocument();
  });

  it("renders all 9 nav rows when every permission is held", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { "*": { "*": true } }, scopes: [] },
      isLoading: false,
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
    } as unknown as ReturnType<typeof usePermissions>);

    renderSidebar("/");
    // Exactly the 9 rows
    expect(screen.getByText("Dashboard")).toBeInTheDocument();
    expect(screen.getByText("Controllers")).toBeInTheDocument();
    expect(screen.getByText("Plugins")).toBeInTheDocument();
    expect(screen.getByText("Clusters")).toBeInTheDocument();
    expect(screen.getByText("Activity")).toBeInTheDocument();
    expect(screen.getByText("Operations")).toBeInTheDocument();
    expect(screen.getByText("Schedules")).toBeInTheDocument();
    expect(screen.getByText("Catalog")).toBeInTheDocument();
    expect(screen.getByText("Admin & access")).toBeInTheDocument();

    // Retired sidebar rows are all absent
    const retired = [
      "Catalog Sources", "Catalog Items", "Composed Bundles",
      "Users", "Groups", "Built-in Roles",
      "Varroa Roles", "Varroa Role Bindings",
      "Jenkins Roles", "Jenkins Role Bindings",
      "Teams", "Provisioning", "Versions", "Identity",
    ];
    for (const label of retired) {
      expect(screen.queryByText(label)).not.toBeInTheDocument();
    }
  });

  describe("door visibility", () => {
    it("shows Catalog door but not Admin & access when only catalogitems:read is held (namespace-scoped)", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: {
          global: {},
          scopes: [
            {
              namespaces: ["team-a"],
              hasControllerSelector: false,
              capabilities: { catalogitems: { read: true } },
            },
          ],
        },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByText("Catalog")).toBeInTheDocument();
      expect(screen.queryByText("Admin & access")).not.toBeInTheDocument();
    });

    it("shows Admin & access door but not Catalog when only global provisioningdefaults:update is held", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: {
          global: { provisioningdefaults: { update: true } },
          scopes: [],
        },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByText("Admin & access")).toBeInTheDocument();
      expect(screen.queryByText("Catalog")).not.toBeInTheDocument();
    });

    it("shows Admin & access door when only global jenkinsroles:read is held", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: {
          global: { jenkinsroles: { read: true } },
          scopes: [],
        },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByText("Admin & access")).toBeInTheDocument();
      expect(screen.queryByText("Catalog")).not.toBeInTheDocument();
    });

    it("shows both doors when user has global *:*", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: { global: { "*": { "*": true } }, scopes: [] },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByText("Catalog")).toBeInTheDocument();
      expect(screen.getByText("Admin & access")).toBeInTheDocument();
    });

    it("hides both doors when no permissions are held", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: { global: {}, scopes: [] },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.queryByText("Catalog")).not.toBeInTheDocument();
      expect(screen.queryByText("Admin & access")).not.toBeInTheDocument();
    });

    it("hides both doors when permissions data is undefined (loading)", () => {
      // beforeEach already sets data: undefined
      renderSidebar("/");
      expect(screen.queryByText("Catalog")).not.toBeInTheDocument();
      expect(screen.queryByText("Admin & access")).not.toBeInTheDocument();
    });
  });

  it("Door links point to the correct routes", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { "*": { "*": true } }, scopes: [] },
      isLoading: false,
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
    } as unknown as ReturnType<typeof usePermissions>);

    renderSidebar("/");
    expect(screen.getByRole("link", { name: "Catalog" })).toHaveAttribute("href", "/catalog");
    expect(screen.getByRole("link", { name: "Admin & access" })).toHaveAttribute("href", "/settings");
  });

  it("every non-door link also navigates correctly", () => {
    renderSidebar("/");
    const checks: { name: string; to: string }[] = [
      { name: "Dashboard", to: "/" },
      { name: "Controllers", to: "/controllers" },
      { name: "Clusters", to: "/clusters" },
      { name: "Activity", to: "/activity" },
      { name: "Operations", to: "/brood-operations" },
      { name: "Schedules", to: "/brood-schedules" },
    ];
    for (const { name, to } of checks) {
      const linkEl = screen.getByRole("link", { name });
      expect(linkEl).toHaveAttribute("href", to);
    }
  });

  it("renders the Dashboard link with end prop (exact match only)", () => {
    renderSidebar("/controllers");
    const dashLink = screen.getByRole("link", { name: "Dashboard" });
    expect(dashLink).not.toHaveAttribute("aria-current", "page");
  });

  it("highlights the Dashboard link as active on root route", () => {
    renderSidebar("/");
    const dashLink = screen.getByRole("link", { name: "Dashboard" });
    expect(dashLink).toHaveAttribute("aria-current", "page");
  });

  it("highlights the Controllers link when on /controllers", () => {
    renderSidebar("/controllers");
    const controllersLink = screen.getByRole("link", { name: "Controllers" });
    expect(controllersLink).toHaveAttribute("aria-current", "page");
  });

  it("renders My profile and Operator healthy in the footer", () => {
    renderSidebar("/");
    expect(screen.getByText("My profile")).toBeInTheDocument();
    expect(screen.getByText("Operator healthy")).toBeInTheDocument();
  });

  describe("badge", () => {
    it("shows the controller count when useControllers resolves data", () => {
      vi.mocked(useControllers).mockReturnValue({
        data: [{ cluster: "c1", name: "n1", namespace: "ns1", phase: "running", endpoint: "https://e1", miteConnected: false }],
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

      renderSidebar("/");
      expect(screen.getByText("1")).toBeInTheDocument();
    });

    it("does not render a count badge when useControllers data is undefined", () => {
      // beforeEach already sets data: undefined
      renderSidebar("/");
      // The Controllers row exists but has no count element
      expect(screen.getByRole("link", { name: "Controllers" })).toBeInTheDocument();
      // There should be no element containing just a number
      expect(screen.queryByText("1")).not.toBeInTheDocument();
    });
  });

  describe("accessible names", () => {
    it("exposes Catalog as a single accessible name (chevron is aria-hidden)", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: { global: { "*": { "*": true } }, scopes: [] },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByRole("link", { name: "Catalog" })).toBeInTheDocument();
    });

    it("exposes Admin & access as a single accessible name (chevron is aria-hidden)", () => {
      vi.mocked(usePermissions).mockReturnValue({
        data: { global: { "*": { "*": true } }, scopes: [] },
        isLoading: false,
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
      } as unknown as ReturnType<typeof usePermissions>);

      renderSidebar("/");
      expect(screen.getByRole("link", { name: "Admin & access" })).toBeInTheDocument();
    });
  });

  describe("collapsed state", () => {
    it("carries the collapsed class on the aside", () => {
      const { container } = renderSidebarCollapsed("/");
      const aside = container.querySelector("aside");
      expect(aside).toBeInTheDocument();
      expect(aside!.className).toContain("collapsed");
    });

    it("keeps aria-current on the active link when collapsed", () => {
      renderSidebarCollapsed("/controllers");
      const controllersLink = screen.getByRole("link", { name: "Controllers" });
      expect(controllersLink).toHaveAttribute("aria-current", "page");
    });

    it("includes count in aria-label when controllers data is available", () => {
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

      renderSidebarCollapsed("/");
      expect(screen.getByRole("link", { name: "Controllers, 5" })).toBeInTheDocument();
    });

    it("omits count in aria-label when controllers data is undefined", () => {
      // beforeEach already sets data: undefined
      renderSidebarCollapsed("/");
      expect(screen.getByRole("link", { name: "Controllers" })).toBeInTheDocument();
    });
  });

  describe("flyout", () => {
    it("appears on mouseOver of a nav item in collapsed state (with count)", () => {
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

      renderSidebarCollapsed("/");
      const controllersLink = screen.getByRole("link", { name: "Controllers, 5" });
      fireEvent.mouseOver(controllersLink);
      expect(screen.getByText(/Controllers\s*5/)).toBeInTheDocument();
    });

    it("disappears on mouseLeave of a nav item", () => {
      vi.mocked(useControllers).mockReturnValue({
        data: [
          { cluster: "c1", name: "n1", namespace: "ns1", phase: "running", endpoint: "https://e1", miteConnected: false },
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

      renderSidebarCollapsed("/");
      const controllersLink = screen.getByRole("link", { name: "Controllers, 1" });
      fireEvent.mouseOver(controllersLink);
      expect(screen.getByText(/Controllers\s*1/)).toBeInTheDocument();
      fireEvent.mouseLeave(controllersLink);
      expect(screen.queryByText(/Controllers\s*1/)).not.toBeInTheDocument();
    });

    it("appears on focus of a nav item in collapsed state", () => {
      const { container } = renderSidebarCollapsed("/");
      const dashLink = screen.getByRole("link", { name: "Dashboard" });
      fireEvent.focus(dashLink);
      const flyoutEl = container.querySelector('div[aria-hidden="true"]');
      expect(flyoutEl).toBeInTheDocument();
      expect(flyoutEl!.textContent).toBe("Dashboard");
    });

    it("disappears on blur of a nav item", () => {
      const { container } = renderSidebarCollapsed("/");
      const dashLink = screen.getByRole("link", { name: "Dashboard" });
      fireEvent.focus(dashLink);
      expect(container.querySelector('div[aria-hidden="true"]')).toBeInTheDocument();
      fireEvent.blur(dashLink);
      expect(container.querySelector('div[aria-hidden="true"]')).toBeNull();
    });

    it("does NOT appear on mouseOver when expanded", () => {
      const { container } = renderSidebar("/");
      const controllersLink = screen.getByRole("link", { name: "Controllers" });
      fireEvent.mouseOver(controllersLink);
      expect(container.querySelector('div[aria-hidden="true"]')).toBeNull();
    });

    it("flyout element has aria-hidden='true'", () => {
      const { container } = renderSidebarCollapsed("/");
      const dashLink = screen.getByRole("link", { name: "Dashboard" });
      fireEvent.focus(dashLink);
      const flyoutEl = container.querySelector('div[aria-hidden="true"]');
      expect(flyoutEl).toBeInTheDocument();
      expect(flyoutEl!.textContent).toBe("Dashboard");
    });
  });
});
