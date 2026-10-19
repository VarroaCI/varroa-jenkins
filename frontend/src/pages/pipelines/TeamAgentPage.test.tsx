import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TeamAgentPage from "./TeamAgentPage";

const mockGetTeamAgent = vi.fn();
const mockListSessions = vi.fn();
const mockCapabilities = vi.fn();
vi.mock("../../api/client", () => ({
  getTeamAgent: (...a: unknown[]) => mockGetTeamAgent(...a),
  listTeamAgentSessions: (...a: unknown[]) => mockListSessions(...a),
  getTeamAgentCapabilities: (...a: unknown[]) => mockCapabilities(...a),
  getAssistantSession: vi.fn(),
  approveAssistantApproval: vi.fn(),
  denyAssistantApproval: vi.fn(),
}));
vi.mock("../../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: undefined }),
  canAccessTenantPipeline: () => false,
}));

function agent() {
  return {
    namespace: "varroa-system",
    name: "ta-one",
    team: "acme",
    cluster: "core",
    spec: { teamRef: { name: "acme" }, enabled: true },
    status: {},
    accepted: true,
    pendingApprovals: 0,
    spendToday: 10,
    dayLimit: 100,
  };
}

function renderPage(path: string) {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines/:team/agent" element={<TeamAgentPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGetTeamAgent.mockReset();
  mockListSessions.mockReset();
  mockCapabilities.mockReset();
  mockListSessions.mockResolvedValue({ items: [], nextCursor: "" });
});

describe("TeamAgentPage", () => {
  it("shows the not-found state when the Team has no Team agent", async () => {
    mockGetTeamAgent.mockRejectedValue(new ApiError(404, "not found"));
    renderPage("/pipelines/acme/agent");
    expect(await screen.findByText("This Team has no Team agent.")).toBeInTheDocument();
  });

  it("opens the sessions tab by default", async () => {
    mockGetTeamAgent.mockResolvedValue(agent());
    renderPage("/pipelines/acme/agent");
    expect(await screen.findByText("No sessions yet.")).toBeInTheDocument();
    expect(mockCapabilities).not.toHaveBeenCalled();
  });

  it("routes to the capabilities tab from the query", async () => {
    mockGetTeamAgent.mockResolvedValue(agent());
    mockCapabilities.mockResolvedValue({ reads: [], writesByApproval: [], unattended: [], never: [] });
    renderPage("/pipelines/acme/agent?tab=capabilities");
    await waitFor(() => expect(mockCapabilities).toHaveBeenCalledWith("acme"));
    expect(await screen.findByRole("list", { name: "Reads" })).toBeInTheDocument();
  });
});
