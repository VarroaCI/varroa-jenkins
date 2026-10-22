import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import type { AssistantApproval, AssistantSessionDetail } from "../../types/assistant";
import ProposePipelineCard from "./ProposePipelineCard";

const mockList = vi.fn();
const mockGet = vi.fn();
const mockPropose = vi.fn();
const mockApprove = vi.fn();
const mockDeny = vi.fn();
const mockCancel = vi.fn();
const mockPost = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: async () => ({ global: {}, scopes: [] }),
  listAssistantSessions: (...a: unknown[]) => mockList(...a),
  getAssistantSession: (...a: unknown[]) => mockGet(...a),
  proposePipeline: (...a: unknown[]) => mockPropose(...a),
  approveAssistantApproval: (...a: unknown[]) => mockApprove(...a),
  denyAssistantApproval: (...a: unknown[]) => mockDeny(...a),
  cancelAssistantSession: (...a: unknown[]) => mockCancel(...a),
  postAssistantMessage: (...a: unknown[]) => mockPost(...a),
}));

const mockOpen = vi.fn();
const mockResolved = vi.fn();
const mockAssistant = { switchOn: true, available: true, openSession: mockOpen, resolved: mockResolved };
vi.mock("../../components/assistant/AssistantProvider", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../components/assistant/AssistantProvider")>()),
  useAssistant: () => mockAssistant,
}));

function pending(overrides: Partial<AssistantApproval> = {}): AssistantApproval {
  return {
    id: "ap-1",
    sessionId: "s-1",
    callId: "c-1",
    tool: "open_pull_request",
    arguments: {
      repo: "https://git.example.org/acme/payments",
      branch: "varroa/propose-pipeline",
      title: "Add a Jenkinsfile proposed by Varroa",
      body: "why",
      files: [{ path: "Jenkinsfile", content: "<script>alert(1)</script>\n```\n# h" }],
    },
    targetKind: "none",
    targetRef: "",
    state: "pending",
    requestedAt: "2026-10-19T10:00:00Z",
    expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
    resolvedBy: null,
    resolvedAt: null,
    reason: null,
    result: null,
    resultWithheld: false,
    ...overrides,
  };
}

function detail(state: AssistantSessionDetail["session"]["state"], approvals: AssistantApproval[] = []): AssistantSessionDetail {
  return {
    session: { id: "s-1", state, steps: 2, skipReason: null } as unknown as AssistantSessionDetail["session"],
    approvals,
  } as AssistantSessionDetail;
}

function renderCard() {
  return renderWithProviders(<ProposePipelineCard team="payments" />);
}

beforeEach(() => {
  mockList.mockReset().mockResolvedValue({ items: [], nextCursor: null });
  mockGet.mockReset();
  mockPropose.mockReset();
  mockApprove.mockReset();
  mockDeny.mockReset();
  mockCancel.mockReset();
  mockPost.mockReset();
  mockOpen.mockReset();
  mockResolved.mockReset();
  mockAssistant.switchOn = true;
});

describe("ProposePipelineCard", () => {
  it("asks for the assistant when the switch is off", () => {
    mockAssistant.switchOn = false;
    renderCard();
    expect(screen.getByText(/Turn on the assistant to propose a pipeline/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open your profile" })).toHaveAttribute("href", "/profile?tab=assistant");
  });

  it("proposes a repository and opens the drawer on the new session", async () => {
    mockPropose.mockResolvedValue({
      sessionId: "s-9", state: "running", repo: "https://git.example.org/acme/payments",
      forge: "gitea", baseBranch: "main", credentialId: "forge-cred",
    });
    mockGet.mockResolvedValue(detail("running"));
    renderCard();
    fireEvent.change((await screen.findByLabelText("Repository URL")), { target: { value: "https://git.example.org/acme/payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Propose" }));
    await waitFor(() => expect(mockOpen).toHaveBeenCalledWith("s-9"));
    expect(mockPropose).toHaveBeenCalledWith("payments", "https://git.example.org/acme/payments");
  });

  it("names the refusal of a code", async () => {
    mockPropose.mockRejectedValue(new ApiError(409, "conflict", { error: "exists", code: "jenkinsfile_exists" }));
    renderCard();
    fireEvent.change((await screen.findByLabelText("Repository URL")), { target: { value: "https://git.example.org/acme/payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Propose" }));
    expect(await screen.findByText("The repository already holds a Jenkinsfile.")).toBeInTheDocument();
  });

  it("names an operator timeout", async () => {
    mockPropose.mockRejectedValue(new ApiError(504, "timeout", { error: "late", code: "operator_timeout" }));
    renderCard();
    fireEvent.change((await screen.findByLabelText("Repository URL")), { target: { value: "https://git.example.org/acme/payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Propose" }));
    expect(await screen.findByText("The operator did not answer in time. Wait, then try again.")).toBeInTheDocument();
  });

  it("names an unavailable operator", async () => {
    mockPropose.mockRejectedValue(new ApiError(503, "unavailable", { error: "none", code: "operator_unavailable" }));
    renderCard();
    fireEvent.change((await screen.findByLabelText("Repository URL")), { target: { value: "https://git.example.org/acme/payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Propose" }));
    expect(await screen.findByText("No operator received the check. Wait, then try again.")).toBeInTheDocument();
  });

  it("shows the reading state of a running session", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("running"));
    renderCard();
    expect(await screen.findByText("The assistant is reading the repository. Steps: 2.")).toBeInTheDocument();
  });

  it("renders each file as text, never as markup", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("waiting_approval", [pending()]));
    const { container } = renderCard();
    const pre = await waitFor(() => {
      const el = container.querySelector("pre[class*='file']");
      expect(el).not.toBeNull();
      return el as HTMLElement;
    });
    expect(pre.textContent).toBe("<script>alert(1)</script>\n```\n# h");
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("h1")).toBeNull();
  });

  it("approves the pending proposal", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("waiting_approval", [pending()]));
    mockApprove.mockResolvedValue({ approval: pending({ state: "approved" }), result: { content: "ok", isError: false } });
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await waitFor(() => expect(mockApprove).toHaveBeenCalledWith("ap-1"));
    expect(mockResolved).toHaveBeenCalledWith("s-1");
  });

  it("denies with a reason", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("waiting_approval", [pending()]));
    mockDeny.mockResolvedValue({ approval: pending({ state: "denied" }), result: { content: "denied", isError: true } });
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "not now" } });
    fireEvent.click(screen.getByRole("button", { name: "Confirm deny" }));
    await waitFor(() => expect(mockDeny).toHaveBeenCalledWith("ap-1", { reason: "not now" }));
  });

  it("shows the opened pull request after a reload", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(
      detail("completed", [pending({ state: "approved", result: JSON.stringify({ url: "https://git.example.org/acme/payments/pulls/4", number: 4 }) })]),
    );
    renderCard();
    const link = await screen.findByRole("link", { name: "Pull request #4 opened" });
    expect(link).toHaveAttribute("href", "https://git.example.org/acme/payments/pulls/4");
  });

  it("offers Start over after a pull request opened", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(
      detail("completed", [pending({ state: "approved", result: JSON.stringify({ url: "https://git.example.org/acme/payments/pulls/4", number: 4 }) })]),
    );
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Start over" }));
    expect((await screen.findByLabelText("Repository URL"))).toBeInTheDocument();
  });

  it("shows no form while the session is loading", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockReturnValue(new Promise(() => {}));
    renderCard();
    expect(await screen.findByText("Loading the proposal.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Repository URL")).not.toBeInTheDocument();
  });

  it("asks again when no proposal opened", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("completed"));
    mockPost.mockResolvedValue({ state: "running" });
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Ask again" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("s-1", { message: "Propose the Jenkinsfile again and call open_pull_request." }));
  });

  it("starts over with the form", async () => {
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("completed"));
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: "Start over" }));
    expect((await screen.findByLabelText("Repository URL"))).toBeInTheDocument();
  });

  it("writes nothing to browser storage", async () => {
    const set = vi.spyOn(Storage.prototype, "setItem");
    mockList.mockResolvedValue({ items: [{ id: "s-1" }], nextCursor: null });
    mockGet.mockResolvedValue(detail("waiting_approval", [pending()]));
    renderCard();
    await screen.findByRole("button", { name: "Approve" });
    // The page's own providers may write their keys. The card's data must not.
    expect(set.mock.calls.flat().join(" ")).not.toMatch(/propose|Jenkinsfile|s-1|payments/);
    set.mockRestore();
  });
});
