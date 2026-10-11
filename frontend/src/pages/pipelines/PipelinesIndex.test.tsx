import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

  it("sets the document title to Pipelines while the team list loads", () => {
    mockGetMyPermissions.mockResolvedValue(noGrants);
    mockListPipelineTeams.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(document.title).toBe("Pipelines");
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
    mockListPipelineTeams.mockResolvedValue({
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

  it("shows an error state with Retry when listPipelineTeams fails", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListPipelineTeams.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load teams")).toBeInTheDocument();
    });
    expect(screen.queryByRole("link", { name: /acme/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await waitFor(() => {
      expect(mockListPipelineTeams).toHaveBeenCalledTimes(2);
    });
  });
});
