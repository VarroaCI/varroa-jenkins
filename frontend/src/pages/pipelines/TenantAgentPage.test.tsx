import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TenantAgentPage from "./TenantAgentPage";
import { A1_VALUES, STATE_LABELS, SKIP_LABELS } from "../../components/assistant/labels";

const mockGetTenantAgent = vi.fn();
const mockListSessions = vi.fn();
const mockCapabilities = vi.fn();
const mockGetSession = vi.fn();
vi.mock("../../components/assistant/TranscriptStream", () => ({ TranscriptStream: () => null }));
vi.mock("../../api/client", () => ({
  getTenantAgent: (...a: unknown[]) => mockGetTenantAgent(...a),
  listTenantAgentSessions: (...a: unknown[]) => mockListSessions(...a),
  getTenantAgentCapabilities: (...a: unknown[]) => mockCapabilities(...a),
  getAssistantSession: (...a: unknown[]) => mockGetSession(...a),
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

function sessionItem(state: string) {
  return {
    id: "s1", kind: "tenant_agent", trigger: "manual", tenant: "acme", context: {}, state,
    skipReason: null, promptTokens: 10, completionTokens: 5, steps: 2,
    startedAt: new Date().toISOString(), pendingApprovals: [],
  };
}

describe("TenantAgentPage", () => {
  it("shows the state label in the session table, not the raw state", async () => {
    mockGetTenantAgent.mockResolvedValue(agent());
    mockListSessions.mockResolvedValue({ items: [sessionItem("waiting_approval")], nextCursor: "" });
    renderPage("/pipelines/acme/agent");
    expect(await screen.findByRole("cell", { name: STATE_LABELS.waiting_approval })).toBeInTheDocument();
    expect(screen.queryByText("waiting_approval")).toBeNull();
  });

  it("offers every session state in the filter", async () => {
    mockGetTenantAgent.mockResolvedValue(agent());
    renderPage("/pipelines/acme/agent");
    const select = await screen.findByLabelText("Filter by state");
    const texts = within(select).getAllByRole("option").map((o) => o.textContent);
    expect(texts).toEqual(["All states", ...A1_VALUES.sessionStates.map((v) => STATE_LABELS[v])]);
  });

  it("shows the state and skip reason labels on the full transcript", async () => {
    mockGetTenantAgent.mockResolvedValue(agent());
    mockGetSession.mockResolvedValue({
      session: { ...sessionItem("skipped"), skipReason: "budget_day", firstMessage: "" },
      approvals: [],
    });
    renderPage("/pipelines/acme/agent?session=s1");
    expect(await screen.findByText(`State: ${STATE_LABELS.skipped} · ${SKIP_LABELS.budget_day}`)).toBeInTheDocument();
  });

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
