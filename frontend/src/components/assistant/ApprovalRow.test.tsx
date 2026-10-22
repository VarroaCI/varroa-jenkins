import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { ApprovalRow } from "./ApprovalRow";
import { AssistantProvider } from "./AssistantProvider";
import type { ApprovalView } from "./transcript";
import * as client from "../../api/client";
import { ApiError } from "../../hooks/useApi";

vi.mock("../../api/client", async (orig) => {
  const actual = await orig<typeof import("../../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(async () => ({ available: true, enabled: true, sessionKey: null })),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    approveAssistantApproval: vi.fn(async () => ({ approval: {}, result: { content: "ok", isError: false } })),
    denyAssistantApproval: vi.fn(async () => ({ approval: {}, result: { content: "no", isError: true } })),
  };
});

const pending: ApprovalView = {
  id: "a1",
  sessionId: "s1",
  tool: "restart_run",
  arguments: { run: "r1" },
  targetKind: "none",
  targetRef: "",
  state: "pending",
  expiresAt: "2099-01-01T00:00:00Z",
};

const perms = { global: { "*": { "*": true } } } as never;

function renderRow(a: ApprovalView, viewer = "alice", principal = "alice", run?: { tenant: string; tenantFolder: string; jobPath: string }) {
  return renderWithProviders(
    <AssistantProvider>
      <ApprovalRow approval={a} viewer={viewer} principal={principal} run={run} />
    </AssistantProvider>,
    { permissions: perms },
  );
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("ApprovalRow", () => {
  it("approves a pending approval the viewer started", async () => {
    renderRow(pending);
    const btn = await screen.findByRole("button", { name: "Approve" });
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(btn);
    await waitFor(() => expect(client.approveAssistantApproval).toHaveBeenCalledWith("a1"));
  });

  it("disables Approve with the reason when the viewer did not start the conversation", async () => {
    renderRow(pending, "bob", "alice");
    const btn = await screen.findByRole("button", { name: "Approve" });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    expect(btn.getAttribute("title")).toMatch(/Only the person/);
  });

  it("denies with a reason and sends it", async () => {
    renderRow(pending);
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "not now" } });
    fireEvent.click(screen.getByRole("button", { name: "Confirm deny" }));
    await waitFor(() => expect(client.denyAssistantApproval).toHaveBeenCalledWith("a1", { reason: "not now" }));
  });

  it("shows the resolved line for an approved approval and no controls", async () => {
    renderRow({ ...pending, state: "approved", resolvedBy: "alice" });
    expect(await screen.findByText("Approved by alice")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("names a 409 conflict and keeps the row", async () => {
    vi.mocked(client.approveAssistantApproval).mockRejectedValueOnce(new ApiError(409, "conflict"));
    renderRow(pending);
    const btn = await screen.findByRole("button", { name: "Approve" });
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(btn);
    expect(await screen.findByText("This approval changed. Reload and try again.")).toBeTruthy();
  });

  it("names an expired approval", async () => {
    vi.mocked(client.approveAssistantApproval).mockRejectedValueOnce(
      new ApiError(409, "conflict", { error: "the approval expired", code: "approval_expired" }),
    );
    renderRow(pending);
    const btn = await screen.findByRole("button", { name: "Approve" });
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(btn);
    expect(await screen.findByText("This approval expired. Ask the assistant again.")).toBeTruthy();
  });

  it("names a generic failure", async () => {
    vi.mocked(client.approveAssistantApproval).mockRejectedValueOnce(new Error("network"));
    renderRow(pending);
    const btn = await screen.findByRole("button", { name: "Approve" });
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(btn);
    expect(await screen.findByText("The decision was not recorded.")).toBeTruthy();
  });

  it("renders file contents from the arguments", async () => {
    renderRow({ ...pending, arguments: { files: [{ path: "a.txt", content: "hello" }] } });
    expect(await screen.findByText("a.txt")).toBeTruthy();
  });

  it("renders a file that holds a fence as plain text", async () => {
    const content = "<script>alert(1)</script>\n```\n# h";
    const { container } = renderRow({ ...pending, arguments: { files: [{ path: "Jenkinsfile", content }] } });
    const pre = await waitFor(() => {
      const el = container.querySelector("pre.file, [class*='file']");
      expect(el).not.toBeNull();
      return el as HTMLElement;
    });
    expect(pre.textContent).toBe(content);
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("h1")).toBeNull();
  });
});
