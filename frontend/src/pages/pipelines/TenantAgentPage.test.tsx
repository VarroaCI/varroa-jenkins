import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TenantAgentPage from "./TenantAgentPage";

const mockGetTenantAgent = vi.fn();
const mockListSessions = vi.fn();
const mockCapabilities = vi.fn();
vi.mock("../../api/client", () => ({
  getTenantAgent: (...a: unknown[]) => mockGetTenantAgent(...a),
  listTenantAgentSessions: (...a: unknown[]) => mockListSessions(...a),
  getTenantAgentCapabilities: (...a: unknown[]) => mockCapabilities(...a),
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
    tenant: "acme",
    cluster: "core",
    spec: { tenantRef: { name: "acme" }, enabled: true },
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
          <Route path="/pipelines/:tenant/agent" element={<TenantAgentPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGetTenantAgent.mockReset();
  mockListSessions.mockReset();
  mockCapabilities.mockReset();
  mockListSessions.mockResolvedValue({ items: [], nextCursor: "" });
});

describe("TenantAgentPage", () => {
  it("shows the not-found state when the Tenant has no Tenant agent", async () => {
    mockGetTenantAgent.mockRejectedValue(new ApiError(404, "not found"));
    renderPage("/pipelines/acme/agent");
    expect(await screen.findByText("This Tenant has no Tenant agent.")).toBeInTheDocument();
  });

  it("opens the sessions tab by default", async () => {
    mockGetTenantAgent.mockResolvedValue(agent());
    renderPage("/pipelines/acme/agent");
    expect(await screen.findByText("No sessions yet.")).toBeInTheDocument();
    expect(mockCapabilities).not.toHaveBeenCalled();
  });

  it("routes to the capabilities tab from the query", async () => {
    mockGetTenantAgent.mockResolvedValue(agent());
    mockCapabilities.mockResolvedValue({ reads: [], writesByApproval: [], unattended: [], never: [] });
    renderPage("/pipelines/acme/agent?tab=capabilities");
    await waitFor(() => expect(mockCapabilities).toHaveBeenCalledWith("acme"));
    expect(await screen.findByRole("list", { name: "Reads" })).toBeInTheDocument();
  });
});
