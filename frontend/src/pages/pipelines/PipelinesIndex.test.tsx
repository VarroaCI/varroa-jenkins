import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useParams } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import PipelinesIndex from "./PipelinesIndex";

const mockGetMyPermissions = vi.fn();
const mockListPipelineTenants = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
  listPipelineTenants: (...args: unknown[]) => mockListPipelineTenants(...args),
}));

function TenantPage() {
  const { tenant } = useParams();
  return <div>tenant page: {tenant}</div>;
}

function renderPage() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={["/pipelines"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines" element={<PipelinesIndex />} />
          <Route path="/pipelines/:tenant" element={<TenantPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

const noGrants = { global: {}, scopes: [], tenants: [] };

describe("PipelinesIndex", () => {
  beforeEach(() => {
    mockGetMyPermissions.mockReset();
    mockListPipelineTenants.mockReset();
  });

  it("sets the document title to Pipelines while the tenant list loads", () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTenants.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(document.title).toBe("Pipelines");
  });

  it("redirects to the single Tenant the caller may open", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTenants.mockResolvedValue({
      items: [{ name: "acme", cluster: "core", placed: true, cell: "cell-a", cellNamespace: "cells" }],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("tenant page: acme")).toBeInTheDocument();
    });
  });

  it("does not link a grant name that GET /pipelines does not answer", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [
        { tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } },
        { tenant: "beta", jobs: [], capabilities: { pipelines: { read: true } } },
        { tenant: "mt-tenant", jobs: [], capabilities: { pipelines: { read: true } } },
      ],
    });
    mockListPipelineTenants.mockResolvedValue({
      items: [
        { name: "acme", cluster: "core", placed: false },
        { name: "beta", cluster: "core", placed: false },
      ],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /beta/ })).toBeInTheDocument();
    });
    expect(screen.queryByText("mt-tenant")).toBeNull();
  });

  it("lists a Tenant that a member cluster holds", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTenants.mockResolvedValue({
      items: [
        { name: "acme", cluster: "core", placed: true, cell: "cell-a", cellNamespace: "cells" },
        { name: "member-a", cluster: "member-c", placed: true, cell: "cell-m", cellNamespace: "cells" },
      ],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /member-a/ })).toHaveAttribute("href", "/pipelines/member-a");
    });
  });

  it("lists every row for a viewer with no tenant grants", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [],
    });
    mockListPipelineTenants.mockResolvedValue({
      items: [
        { name: "north", cluster: "core", placed: false },
        { name: "shop", cluster: "member-a", placed: true, cell: "cell-a", cellNamespace: "cells" },
        { name: "shop", cluster: "member-b", placed: true, cell: "cell-b", cellNamespace: "cells" },
        { name: "south", cluster: "member-c", placed: true, cell: "cell-s", cellNamespace: "cells" },
      ],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /north/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /south/ })).toBeInTheDocument();
    });
    expect(screen.getAllByRole("link", { name: /shop/ })).toHaveLength(1);
  });

  it("redirects to a single member-placed Tenant", async () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTenants.mockResolvedValue({
      items: [{ name: "member-a", cluster: "member-c", placed: true, cell: "cell-m", cellNamespace: "cells" }],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("tenant page: member-a")).toBeInTheDocument();
    });
  });

  it("shows a recovery state when no Tenant grants pipelines access", async () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTenants.mockResolvedValue({ items: [], recordStore: true });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("No pipelines access")).toBeInTheDocument();
    });
  });

  it("shows an error state with Retry when listPipelineTenants fails", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTenants.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load tenants")).toBeInTheDocument();
    });
    expect(screen.queryByRole("link", { name: /acme/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await waitFor(() => {
      expect(mockListPipelineTenants).toHaveBeenCalledTimes(2);
    });
  });
});
