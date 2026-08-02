import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { AreaShell } from "./AreaShell";
import { SETTINGS_ITEMS, CATALOG_ITEMS } from "../lib/navAreas";

vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

import { usePermissions } from "../hooks/usePermissions";

function createPermissionsFixture(global: Record<string, Record<string, boolean>>, scopes: unknown[] = []) {
  return {
    data: { global, scopes },
    isLoading: false,
    error: null,
    isError: false,
    isSuccess: true,
    status: "success" as const,
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
    fetchStatus: "idle" as const,
    promise: Promise.resolve(undefined),
    refetch: vi.fn(),
  };
}

function renderAreaShell(items: typeof SETTINGS_ITEMS, title: string, route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<AreaShell items={items} title={title} />}>
            {items.map((item) => (
              <Route
                key={item.to}
                path={item.to.replace(/^\//, "")}
                element={<div data-testid="page">{item.label} page</div>}
              />
            ))}
            {/* Catch-all so the shell layout renders when no child matches */}
            <Route path="*" element={<div data-testid="page">Content</div>} />
          </Route>
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AreaShell", () => {
  beforeEach(() => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({}, []) as unknown as ReturnType<typeof usePermissions>,
    );
  });

  it("shows only Provisioning and Versions for a provisioningdefaults:update-only user", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({ provisioningdefaults: { update: true } }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(SETTINGS_ITEMS, "Settings");

    expect(screen.getByText("Provisioning")).toBeInTheDocument();
    expect(screen.getByText("Versions")).toBeInTheDocument();
    // Admin-gated items should not appear
    expect(screen.queryByText("Users")).not.toBeInTheDocument();
    expect(screen.queryByText("Groups")).not.toBeInTheDocument();
    expect(screen.queryByText("Built-in Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Identity")).not.toBeInTheDocument();
    // globalOnly roles:read items should not appear
    expect(screen.queryByText("Varroa Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Varroa Role Bindings")).not.toBeInTheDocument();
    expect(screen.queryByText("Jenkins Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Jenkins Role Bindings")).not.toBeInTheDocument();
    expect(screen.queryByText("Teams")).not.toBeInTheDocument();
  });

  it("shows all 11 settings items for a global *:* user", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({ "*": { "*": true } }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(SETTINGS_ITEMS, "Settings");

    const expectedLabels = [
      "Users", "Groups", "Built-in Roles",
      "Varroa Roles", "Varroa Role Bindings",
      "Jenkins Roles", "Jenkins Role Bindings",
      "Teams", "Provisioning", "Versions", "Identity",
    ];

    for (const label of expectedLabels) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it("renders all 11 settings links with correct hrefs in order", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({ "*": { "*": true } }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(SETTINGS_ITEMS, "Settings");

    const expected = [
      { label: "Users", href: "/access/users" },
      { label: "Groups", href: "/access/groups" },
      { label: "Built-in Roles", href: "/access/builtin-roles" },
      { label: "Varroa Roles", href: "/access/roles" },
      { label: "Varroa Role Bindings", href: "/access/role-bindings" },
      { label: "Jenkins Roles", href: "/access/jenkins-roles" },
      { label: "Jenkins Role Bindings", href: "/access/jenkins-role-bindings" },
      { label: "Teams", href: "/access/teams" },
      { label: "Provisioning", href: "/administration/provisioning" },
      { label: "Versions", href: "/administration/versions" },
      { label: "Identity", href: "/administration/identity" },
    ];

    const links = screen.getAllByRole("link");
    for (let i = 0; i < expected.length; i++) {
      expect(links[i]).toHaveTextContent(expected[i].label);
      expect(links[i]).toHaveAttribute("href", expected[i].href);
    }
  });

  it("shows all 3 catalog items for a user with all catalog read perms", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({
        catalogsources: { read: true },
        catalogitems: { read: true },
        composedbundles: { read: true },
      }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(CATALOG_ITEMS, "Catalog");

    expect(screen.getByText("Catalog Sources")).toBeInTheDocument();
    expect(screen.getByText("Catalog Items")).toBeInTheDocument();
    expect(screen.getByText("Composed Bundles")).toBeInTheDocument();
  });

  it("enforces globalOnly — namespace-scoped roles:read does NOT show Varroa Roles", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [
          {
            namespaces: ["team-a"],
            hasControllerSelector: false,
            capabilities: { roles: { read: true } },
          },
        ],
      },
      isLoading: false,
      error: null,
      isError: false,
      isSuccess: true,
      status: "success" as const,
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
      fetchStatus: "idle" as const,
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderAreaShell(SETTINGS_ITEMS, "Settings");
    expect(screen.queryByText("Varroa Roles")).not.toBeInTheDocument();
  });

  it("sets aria-current on the active link", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({ "*": { "*": true } }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(SETTINGS_ITEMS, "Settings", "/access/users");
    const usersLink = screen.getByRole("link", { name: "Users" });
    expect(usersLink).toHaveAttribute("aria-current", "page");
  });

  it("does NOT set aria-current on Catalog Items (/catalog, end) when on /catalog/sources", () => {
    vi.mocked(usePermissions).mockReturnValue(
      createPermissionsFixture({
        catalogsources: { read: true },
        catalogitems: { read: true },
        composedbundles: { read: true },
      }) as unknown as ReturnType<typeof usePermissions>,
    );

    renderAreaShell(CATALOG_ITEMS, "Catalog", "/catalog/sources");
    const itemsLink = screen.getByRole("link", { name: "Catalog Items" });
    expect(itemsLink).not.toHaveAttribute("aria-current", "page");
    // The active one should be Catalog Sources
    const sourcesLink = screen.getByRole("link", { name: "Catalog Sources" });
    expect(sourcesLink).toHaveAttribute("aria-current", "page");
  });

  describe("route placement", () => {
    function renderRouteFixture(route: string) {
      const queryClient = createTestQueryClient();
      const { container } = render(
        <MemoryRouter initialEntries={[route]}>
          <QueryClientProvider client={queryClient}>
            <Routes>
              <Route element={<AreaShell items={SETTINGS_ITEMS} title="Admin & access" />}>
                <Route path="access/roles" element={<div data-testid="page">Roles page</div>} />
                <Route path="settings" element={<div data-testid="page">Settings index</div>} />
              </Route>
              {/* create route outside the shell (sibling) */}
              <Route path="access/roles/create" element={<div data-testid="page">Create role</div>} />
            </Routes>
          </QueryClientProvider>
        </MemoryRouter>,
      );
      return { container };
    }

    it("renders sub-nav on /access/roles (inside settings shell)", () => {
      vi.mocked(usePermissions).mockReturnValue(
        createPermissionsFixture({ "*": { "*": true } }) as unknown as ReturnType<typeof usePermissions>,
      );
      renderRouteFixture("/access/roles");
      // Sub-nav links render (AreaShell is the layout wrapper)
      expect(screen.getByRole("link", { name: "Varroa Roles" })).toBeInTheDocument();
      expect(screen.getByText("Roles page")).toBeInTheDocument();
    });

    it("renders sub-nav on /settings (inside settings shell)", () => {
      vi.mocked(usePermissions).mockReturnValue(
        createPermissionsFixture({ "*": { "*": true } }) as unknown as ReturnType<typeof usePermissions>,
      );
      renderRouteFixture("/settings");
      // The settings route has no matching item in SETTINGS_ITEMS so no nav link for /settings,
      // but the shell layout itself renders (the nav shows all filtered items)
      expect(screen.getByRole("link", { name: "Varroa Roles" })).toBeInTheDocument();
      expect(screen.getByText("Settings index")).toBeInTheDocument();
    });

    it("does NOT render sub-nav on /access/roles/create (outside settings shell)", () => {
      const { container } = renderRouteFixture("/access/roles/create");
      // Only the create page renders, no AreaShell sub-nav
      expect(screen.getByText("Create role")).toBeInTheDocument();
      expect(container.querySelector("nav")).toBeNull();
    });
  });

  describe("catalog route placement", () => {
    function renderCatalogRouteFixture(route: string) {
      const queryClient = createTestQueryClient();
      const { container } = render(
        <MemoryRouter initialEntries={[route]}>
          <QueryClientProvider client={queryClient}>
            <Routes>
              <Route element={<AreaShell items={CATALOG_ITEMS} title="Catalog" />}>
                <Route path="catalog" element={<div data-testid="page">Catalog page</div>} />
                <Route path="catalog/sources" element={<div data-testid="page">Sources</div>} />
                <Route path="catalog/bundles" element={<div data-testid="page">Bundles</div>} />
              </Route>
              {/* detail route outside the shell */}
              <Route path="catalog/bundles/:namespace/:name" element={<div data-testid="page">Bundle detail</div>} />
            </Routes>
          </QueryClientProvider>
        </MemoryRouter>,
      );
      return { container };
    }

    it("renders sub-nav on /catalog (inside catalog shell)", () => {
      vi.mocked(usePermissions).mockReturnValue(
        createPermissionsFixture({
          catalogsources: { read: true },
          catalogitems: { read: true },
          composedbundles: { read: true },
        }) as unknown as ReturnType<typeof usePermissions>,
      );
      renderCatalogRouteFixture("/catalog");
      expect(screen.getByRole("link", { name: "Catalog Items" })).toBeInTheDocument();
      expect(screen.getByText("Catalog page")).toBeInTheDocument();
    });

    it("does NOT render sub-nav on /catalog/bundles/ns/x (outside catalog shell)", () => {
      const { container } = renderCatalogRouteFixture("/catalog/bundles/ns/x");
      expect(screen.getByText("Bundle detail")).toBeInTheDocument();
      expect(container.querySelector("nav")).toBeNull();
    });
  });
});
