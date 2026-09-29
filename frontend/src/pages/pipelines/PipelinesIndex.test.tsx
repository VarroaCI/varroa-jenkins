import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useParams } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import PipelinesIndex from "./PipelinesIndex";

const mockGetMyPermissions = vi.fn();
const mockListPipelineTeams = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
  listPipelineTeams: (...args: unknown[]) => mockListPipelineTeams(...args),
}));

function TeamPage() {
  const { team } = useParams();
  return <div>team page: {team}</div>;
}

function renderPage() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={["/pipelines"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines" element={<PipelinesIndex />} />
          <Route path="/pipelines/:team" element={<TeamPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

const noGrants = { global: {}, scopes: [], tenants: [] };

describe("PipelinesIndex", () => {
  beforeEach(() => {
    mockGetMyPermissions.mockReset();
    mockListPipelineTeams.mockReset();
  });

  it("redirects to the single Team the caller may open", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTeams.mockResolvedValue({
      items: [{ name: "acme", cluster: "core", placed: true, cell: "cell-a", cellNamespace: "cells" }],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("team page: acme")).toBeInTheDocument();
    });
  });

  it("lists every Team the caller's tenant grants name", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [
        { tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } },
        { tenant: "beta", jobs: [], capabilities: { pipelines: { read: true } } },
      ],
    });
    mockListPipelineTeams.mockResolvedValue({ items: [], recordStore: true });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /beta/ })).toBeInTheDocument();
    });
  });

  it("lists a Team that a member cluster holds", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTeams.mockResolvedValue({
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
    mockListPipelineTeams.mockResolvedValue({
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

  it("redirects to a single member-placed Team", async () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTeams.mockResolvedValue({
      items: [{ name: "member-a", cluster: "member-c", placed: true, cell: "cell-m", cellNamespace: "cells" }],
      recordStore: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("team page: member-a")).toBeInTheDocument();
    });
  });

  it("shows a recovery state when no Team grants pipelines access", async () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTeams.mockResolvedValue({ items: [], recordStore: true });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("No pipelines access")).toBeInTheDocument();
    });
  });

  it("shows the error state when the permissions read fails, while the team list is pending", async () => {
    mockGetMyPermissions.mockRejectedValue(new Error("boom"));
    mockListPipelineTeams.mockReturnValue(new Promise(() => {}));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load page")).toBeInTheDocument();
    });
  });

  it("still renders the caller's tenant Teams when listPipelineTeams fails", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [
        { tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } },
        { tenant: "gamma", jobs: [], capabilities: { pipelines: { read: true } } },
      ],
    });
    mockListPipelineTeams.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /gamma/ })).toBeInTheDocument();
    });
  });
});
