import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import PipelinesIndex from "./PipelinesIndex";

const mockGetMyPermissions = vi.fn();
const mockListTeams = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
  listTeams: (...args: unknown[]) => mockListTeams(...args),
}));

function renderPage() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={["/pipelines"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines" element={<PipelinesIndex />} />
          <Route path="/pipelines/:team" element={<div>team page: acme</div>} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("PipelinesIndex", () => {
  beforeEach(() => {
    mockGetMyPermissions.mockReset();
    mockListTeams.mockReset();
  });

  it("redirects to the single Team the caller may open", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }],
    });
    mockListTeams.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("team page: acme")).toBeInTheDocument();
    });
    expect(mockListTeams).not.toHaveBeenCalled();
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
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /beta/ })).toBeInTheDocument();
    });
  });

  it("adds every Team from listTeams for a caller with a global pipelines verb", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [],
    });
    mockListTeams.mockResolvedValue([
      { name: "north", namespaces: [] },
      { name: "south", namespaces: [] },
    ]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /north/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /south/ })).toBeInTheDocument();
    });
  });

  it("shows a recovery state when no Team grants pipelines access", async () => {
    mockGetMyPermissions.mockResolvedValue({ global: {}, scopes: [], tenants: [] });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("No pipelines access")).toBeInTheDocument();
    });
  });

  it("still renders the caller's tenant Teams when listTeams fails", async () => {
    mockGetMyPermissions.mockResolvedValue({
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [
        { tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } },
        { tenant: "gamma", jobs: [], capabilities: { pipelines: { read: true } } },
      ],
    });
    mockListTeams.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /acme/ })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /gamma/ })).toBeInTheDocument();
    });
  });
});
