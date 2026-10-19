import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { TeamAgentCapabilities } from "./TeamAgentCapabilities";

const mockCaps = vi.fn();
vi.mock("../../api/client", () => ({
  getTeamAgentCapabilities: (...a: unknown[]) => mockCaps(...a),
}));

beforeEach(() => {
  mockCaps.mockReset();
});

describe("TeamAgentCapabilities", () => {
  it("renders the four groups with their tools", async () => {
    mockCaps.mockResolvedValue({
      reads: [{ name: "get_run", description: "Reads a run.", kind: "read" }],
      writesByApproval: [],
      unattended: [{ name: "restart_run", description: "Restarts.", kind: "write" }],
      never: [{ name: "release_fleet_lock", description: "Releases.", kind: "write" }],
    });
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <TeamAgentCapabilities team="acme" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("get_run")).toBeInTheDocument();
    expect(screen.getByText("restart_run")).toBeInTheDocument();
    expect(screen.getByText("release_fleet_lock")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Writes by approval" })).toHaveTextContent("None");
  });
});
