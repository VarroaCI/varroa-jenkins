import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import Agents from "./Agents";

const mockList = vi.fn();
const mockPut = vi.fn();
const mockCanToggle = vi.fn();
vi.mock("../api/client", () => ({
  listAgents: (...a: unknown[]) => mockList(...a),
  putAgentUnattended: (...a: unknown[]) => mockPut(...a),
}));
vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: undefined }),
  canDoGlobal: (...a: unknown[]) => mockCanToggle(...a),
}));

function renderAgents() {
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <Agents />
    </QueryClientProvider>,
  );
}

const item = {
  namespace: "varroa-system", name: "ta-one", team: "acme", cluster: "core", enabled: true,
  paused: null, sessionsToday: 3, spendToday: 12, failedToday: 1, conditions: [],
  unattendedRequested: ["restart_run"], unattendedGranted: [],
};

beforeEach(() => {
  mockList.mockReset();
  mockPut.mockReset();
  mockCanToggle.mockReset();
  mockList.mockResolvedValue({ items: [item] });
  mockPut.mockResolvedValue({ namespace: "varroa-system", name: "ta-one", enabled: true, unattendedGranted: ["restart_run"] });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Agents", () => {
  it("hides the unattended toggle without the teamagents:unattended grant", async () => {
    mockCanToggle.mockReturnValue(false);
    renderAgents();
    expect(await screen.findByText("varroa-system/ta-one")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Grant" })).not.toBeInTheDocument();
  });

  it("asks before it grants, and does nothing when the operator declines", async () => {
    mockCanToggle.mockReturnValue(true);
    vi.spyOn(window, "confirm").mockReturnValue(false);
    renderAgents();
    fireEvent.click(await screen.findByRole("button", { name: "Grant" }));
    expect(window.confirm).toHaveBeenCalled();
    expect(mockPut).not.toHaveBeenCalled();
  });

  it("grants after the operator confirms", async () => {
    mockCanToggle.mockReturnValue(true);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    renderAgents();
    fireEvent.click(await screen.findByRole("button", { name: "Grant" }));
    await waitFor(() => expect(mockPut).toHaveBeenCalledWith("varroa-system", "ta-one", true));
  });
});
