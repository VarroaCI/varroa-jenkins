import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { TenantAgentApprovals } from "./TenantAgentApprovals";
import * as client from "../../api/client";
import { ApiError } from "../../hooks/useApi";

const mockList = vi.fn();
const mockCanWrite = vi.fn();
vi.mock("../../api/client", () => ({
  listTenantAgentSessions: (...a: unknown[]) => mockList(...a),
  approveAssistantApproval: vi.fn(),
  denyAssistantApproval: vi.fn(),
}));
vi.mock("../../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: undefined }),
  canAccessTenantPipeline: (...a: unknown[]) => mockCanWrite(...a),
}));

function renderApprovals() {
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <TenantAgentApprovals tenant="acme" />
    </QueryClientProvider>,
  );
}

const pending = {
  items: [{
    id: "s1", kind: "tenant_agent", state: "waiting_approval", startedAt: "2026-10-18T10:00:00Z",
    pendingApprovals: [{
      id: "a1", sessionId: "s1", callId: "c1", tool: "cancel_run", arguments: {}, targetKind: "run",
      targetRef: "r1", state: "pending", requestedAt: "2026-10-18T10:00:00Z",
      expiresAt: "2099-01-01T00:00:00Z", reason: "",
    }],
  }],
  nextCursor: "",
};

beforeEach(() => {
  mockList.mockReset();
  mockCanWrite.mockReset();
  mockList.mockResolvedValue(pending);
});

describe("TenantAgentApprovals", () => {
  it("shows no decision button to a reader", async () => {
    mockCanWrite.mockReturnValue(false);
    renderApprovals();
    expect(await screen.findByText("cancel_run")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
  });

  it("shows Approve and Deny to a tenant writer", async () => {
    mockCanWrite.mockReturnValue(true);
    renderApprovals();
    expect(await screen.findByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Deny" })).toBeInTheDocument();
  });

  it("names an expired approval and reloads the list", async () => {
    mockCanWrite.mockReturnValue(true);
    vi.mocked(client.approveAssistantApproval).mockRejectedValueOnce(
      new ApiError(409, "conflict", { error: "the approval expired", code: "approval_expired" }),
    );
    renderApprovals();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByText("This approval expired. Ask the assistant again.")).toBeInTheDocument();
    await waitFor(() => expect(mockList.mock.calls.length).toBeGreaterThan(1));
  });
});
