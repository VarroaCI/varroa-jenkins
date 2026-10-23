import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TenantAgentCard from "./TenantAgentCard";

const mockGetTenantAgent = vi.fn();
vi.mock("../../api/client", () => ({
  getTenantAgent: (...a: unknown[]) => mockGetTenantAgent(...a),
}));

function renderCard() {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={createTestQueryClient()}>
        <TenantAgentCard tenant="acme" />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGetTenantAgent.mockReset();
});

describe("TenantAgentCard", () => {
  it("renders nothing when the Tenant has no Tenant agent", async () => {
    mockGetTenantAgent.mockRejectedValue(new ApiError(404, "not found"));
    const { container } = renderCard();
    await vi.waitFor(() => expect(mockGetTenantAgent).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByText("Tenant agent")).not.toBeInTheDocument();
  });

  it("shows the enabled state, pending approvals, and spend today", async () => {
    mockGetTenantAgent.mockResolvedValue({
      namespace: "varroa-system", name: "ta-one", tenant: "acme", cluster: "core",
      spec: { tenantRef: { name: "acme" }, enabled: true }, status: {},
      accepted: true, pendingApprovals: 2, spendToday: 40, dayLimit: 100,
    });
    renderCard();
    expect(await screen.findByText("Tenant agent")).toBeInTheDocument();
    expect(screen.getByText("Yes")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.getByText("40 tokens")).toBeInTheDocument();
  });
});
