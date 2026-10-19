import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TeamAgentCard from "./TeamAgentCard";

const mockGetTeamAgent = vi.fn();
vi.mock("../../api/client", () => ({
  getTeamAgent: (...a: unknown[]) => mockGetTeamAgent(...a),
}));

function renderCard() {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={createTestQueryClient()}>
        <TeamAgentCard team="acme" />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGetTeamAgent.mockReset();
});

describe("TeamAgentCard", () => {
  it("renders nothing when the Team has no Team agent", async () => {
    mockGetTeamAgent.mockRejectedValue(new ApiError(404, "not found"));
    const { container } = renderCard();
    await vi.waitFor(() => expect(mockGetTeamAgent).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByText("Team agent")).not.toBeInTheDocument();
  });

  it("shows the enabled state, pending approvals, and spend today", async () => {
    mockGetTeamAgent.mockResolvedValue({
      namespace: "varroa-system", name: "ta-one", team: "acme", cluster: "core",
      spec: { teamRef: { name: "acme" }, enabled: true }, status: {},
      accepted: true, pendingApprovals: 2, spendToday: 40, dayLimit: 100,
    });
    renderCard();
    expect(await screen.findByText("Team agent")).toBeInTheDocument();
    expect(screen.getByText("Yes")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.getByText("40 tokens")).toBeInTheDocument();
  });
});
