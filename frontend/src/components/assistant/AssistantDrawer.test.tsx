import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent, act, within } from "@testing-library/react";
import { Routes, Route, useNavigate } from "react-router-dom";
import { renderWithProviders } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import { installMockEventSource, MockEventSource } from "../../test/mockEventSource";
import { AssistantProvider, useAssistant } from "./AssistantProvider";
import AssistantDrawer from "./AssistantDrawer";
import * as client from "../../api/client";
import type { AssistantSession } from "../../types/assistant";
import { contextLabel } from "./routeContext";
import { STATE_LABELS, SKIP_LABELS } from "./labels";
import { NARROW_QUERY } from "../../hooks/useNarrowViewport";
import { DOCKED_QUERY } from "./drawerWidth";

vi.mock("../../hooks/useApi", async (orig) => {
  const actual = await orig<typeof import("../../hooks/useApi")>();
  return {
    ...actual,
    bffFetch: vi.fn(async () => ({ ticket: "vst_test", expiresInSeconds: 30 })),
  };
});

vi.mock("../../api/client", async (orig) => {
  const actual = await orig<typeof import("../../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(async () => ({ available: true, enabled: true, sessionKey: null })),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    getAssistantSession: vi.fn(),
    postAssistantMessage: vi.fn(async () => ({ state: "running" })),
    cancelAssistantSession: vi.fn(async () => ({ state: "cancelled" })),
    createAssistantSession: vi.fn(),
    listAssistantSessions: vi.fn(async () => ({ items: [], nextCursor: null })),
    createRunNote: vi.fn(),
    approveAssistantApproval: vi.fn(async () => ({ approval: {}, result: { content: "ok", isError: false } })),
    denyAssistantApproval: vi.fn(async () => ({ approval: {}, result: { content: "no", isError: true } })),
  };
});

const session: AssistantSession = {
  id: "s1",
  kind: "assistant",
  principal: "alice",
  tenant: "",
  cluster: "core",
  trigger: "manual",
  context: { route: "dashboard" },
  firstMessage: "",
  provider: "smoke",
  model: "varroa-smoke",
  keySource: "chart",
  dataSharing: "logs",
  state: "running",
  skipReason: null,
  promptTokens: 0,
  completionTokens: 0,
  steps: 0,
  startedAt: "2026-10-18T10:00:00Z",
  endedAt: null,
};

/** Probe opens a session from the outside and shows the drawer when it is open. */
function Probe() {
  const a = useAssistant();
  return (
    <>
      <button type="button" onClick={() => a.openSession("s1")}>open</button>
      <button type="button" onClick={() => a.toggleDrawer()}>toggle</button>
      <span>{a.drawerOpen ? "drawer open" : "drawer closed"}</span>
      {a.drawerOpen && <AssistantDrawer />}
    </>
  );
}

function renderDrawer() {
  return renderWithProviders(
    <AssistantProvider>
      <Probe />
    </AssistantProvider>,
    { permissions: { global: { "*": { "*": true } } } as never },
  );
}

beforeEach(() => {
  vi.mocked(client.getAssistantSession).mockResolvedValue({ session, approvals: [] });
});

installMockEventSource();

describe("AssistantDrawer", () => {
  it("opens a session, streams its frames, and shows the transcript", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    expect(await screen.findByText("drawer open")).toBeTruthy();
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    expect(es.url).toContain("/assistant/sessions/s1/events?after=0");

    act(() => {
      es.emitNamedEvent("user", JSON.stringify({ text: "why did it fail", created_at: "2026-10-18T10:00:00Z" }), "1");
      es.emitNamedEvent("assistant", JSON.stringify({ turn: 1, text: "It **timed out**.", final: true, created_at: "2026-10-18T10:00:01Z" }), "2");
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", stop: "", usage_estimated: false, created_at: "2026-10-18T10:00:02Z" }), "3");
    });

    expect(await screen.findByText("why did it fail")).toBeTruthy();
    expect(await screen.findByText("timed out")).toBeTruthy();
    expect(await screen.findByText("State: Completed")).toBeTruthy();
  });

  it("sends a follow-up message to a completed session", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: { ...session, state: "completed" }, approvals: [] });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: "2026-10-18T10:00:02Z" }), "1");
    });
    const box = await screen.findByLabelText("Message");
    fireEvent.change(box, { target: { value: "and the fix?" } });
    const send = screen.getByRole("button", { name: "Send" });
    await waitFor(() => expect((send as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(send);
    await waitFor(() => expect(client.postAssistantMessage).toHaveBeenCalledWith("s1", { message: "and the fix?" }));
  });

  it("closes on Escape and stops a running session", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
    await waitFor(() => expect(client.cancelAssistantSession).toHaveBeenCalledWith("s1"));
    fireEvent.keyDown(screen.getByRole("complementary", { name: "Assistant" }), { key: "Escape" });
    await waitFor(() => expect(screen.getByText("drawer closed")).toBeTruthy());
  });

  it("says when there are no sessions", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    fireEvent.click(await screen.findByRole("button", { name: "Sessions" }));
    expect(await screen.findByText("No sessions yet.")).toBeTruthy();
  });

  it("lists sessions with a relative age and marks the current one", async () => {
    const startedAt = new Date(Date.now() - 5 * 60_000).toISOString();
    vi.mocked(client.listAssistantSessions).mockResolvedValueOnce({
      items: [{ ...session, startedAt, firstMessage: "why did it fail" }],
      nextCursor: null,
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    fireEvent.click(await screen.findByRole("button", { name: "Sessions" }));
    const list = await screen.findByRole("list", { name: "Sessions" });
    const row = within(list).getByRole("button");
    const title = row.querySelector("span");
    expect(title?.textContent).toBe("why did it fail");
    expect(row.textContent).toContain(contextLabel({ route: "dashboard" }));
    expect(row.textContent).toMatch(/5m ago · Current/);
    expect(row.getAttribute("aria-current")).toBe("true");
  });

  it("shows the state label on a session row, not the raw state", async () => {
    vi.mocked(client.listAssistantSessions).mockResolvedValueOnce({
      items: [{ ...session, id: "s9", state: "waiting_approval", firstMessage: "why did it fail" }],
      nextCursor: null,
    });
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    fireEvent.click(await screen.findByRole("button", { name: "Sessions" }));
    const list = await screen.findByRole("list", { name: "Sessions" });
    const row = within(list).getByRole("button");
    expect(row.textContent).toContain("Waiting for approval · ");
    expect(row.textContent).not.toContain("waiting_approval");
  });

  it("shows the token counts of the session in the footer", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({
      session: { ...session, state: "completed", promptTokens: 1200, completionTokens: 340 },
      approvals: [],
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    expect(await screen.findByText("Tokens: 1200 prompt · 340 completion")).toBeTruthy();
  });

  it("marks an estimated token count after a done frame", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({
      session: { ...session, state: "completed", promptTokens: 1200, completionTokens: 340 },
      approvals: [],
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", stop: "", usage_estimated: true, created_at: "2026-10-18T10:00:02Z" }), "1");
    });
    expect(await screen.findByText("Tokens: 1200 prompt · 340 completion (estimated)")).toBeTruthy();
  });

  it("reads the token counts again after a done frame", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    expect(await screen.findByText("Tokens: 0 prompt · 0 completion")).toBeTruthy();
    vi.mocked(client.getAssistantSession).mockResolvedValue({
      session: { ...session, state: "completed", promptTokens: 50, completionTokens: 20 },
      approvals: [],
    });
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", stop: "", usage_estimated: false, created_at: "2026-10-18T10:00:02Z" }), "1");
    });
    expect(await screen.findByText("Tokens: 50 prompt · 20 completion")).toBeTruthy();
  });

  it("shows the skip reason label of a skipped session", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({
      session: { ...session, state: "skipped", skipReason: "budget_day" },
      approvals: [],
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    expect(await screen.findByText(`State: ${STATE_LABELS.skipped} · ${SKIP_LABELS.budget_day}`)).toBeTruthy();
    expect(screen.queryByText(/budget_day/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  });

  it("shows no token line without a session", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    expect(await screen.findByText("No session")).toBeTruthy();
    expect(screen.queryByText(/^Tokens:/)).toBeNull();
  });

  it("uses the context label as the row title when the first message is empty", async () => {
    vi.mocked(client.listAssistantSessions).mockResolvedValueOnce({
      items: [{ ...session, firstMessage: "" }],
      nextCursor: null,
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    fireEvent.click(await screen.findByRole("button", { name: "Sessions" }));
    const list = await screen.findByRole("list", { name: "Sessions" });
    const row = within(list).getByRole("button");
    const title = row.querySelector("span");
    expect(title?.textContent).toBe(contextLabel({ route: "dashboard" }));
    expect(row.querySelectorAll("span")).toHaveLength(2);
  });

  it("the header close control is a labelled button", async () => {
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    const close = await screen.findByRole("button", { name: "Close" });
    expect(close.tagName).toBe("BUTTON");
    fireEvent.click(close);
    await waitFor(() => expect(screen.getByText("drawer closed")).toBeTruthy());
  });

  it("starts a new session from an empty composer", async () => {
    vi.mocked(client.createAssistantSession).mockResolvedValue({ ...session, id: "s2" });
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    const box = await screen.findByLabelText("Message");
    fireEvent.change(box, { target: { value: "hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(client.createAssistantSession).toHaveBeenCalled());
    expect(vi.mocked(client.createAssistantSession).mock.calls[0][0]).toMatchObject({ message: "hello" });
  });

  it("shows a sent message at once and names each step of the run", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: { ...session, state: "completed" }, approvals: [] });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    const at = "2026-10-31T10:00:00Z";
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: at }), "1");
    });
    const box = await screen.findByLabelText("Message");
    const count = MockEventSource.instances.length;

    let release: () => void = () => undefined;
    vi.mocked(client.postAssistantMessage).mockImplementationOnce(
      () => new Promise((resolve) => { release = () => resolve({ state: "running" } as never); }),
    );
    fireEvent.change(box, { target: { value: "and the fix?" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText("and the fix?")).toBeTruthy();
    expect(screen.getByRole("status").textContent).toContain("Sending…");
    expect((box as HTMLTextAreaElement).value).toBe("");

    act(() => release());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Thinking…"));

    act(() => {
      es.emitNamedEvent("user", JSON.stringify({ text: "and the fix?", created_at: at }), "2");
    });
    await waitFor(() => expect(screen.getAllByText("and the fix?")).toHaveLength(1));

    act(() => {
      es.emitNamedEvent("tool_call", JSON.stringify({ call_id: "c1", name: "get_run", arguments: {}, class: "read", created_at: at }), "3");
    });
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("get_run · In progress"));

    act(() => {
      es.emitNamedEvent("tool_result", JSON.stringify({ call_id: "c1", content: "ok", is_error: false, data_level: "logs", created_at: at }), "4");
    });
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Thinking…"));

    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: at }), "5");
    });
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe(""));
    expect(MockEventSource.instances.length).toBe(count);
  });

  /** openWaitingApproval opens s1 with one approval waiting and returns the stream count. */
  async function openWaitingApproval() {
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: { ...session, state: "waiting_approval" }, approvals: [] });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    const at = "2026-10-31T10:00:00Z";
    act(() => {
      es.emitNamedEvent("user", JSON.stringify({ text: "restart it", created_at: at }), "1");
      es.emitNamedEvent("tool_call", JSON.stringify({ call_id: "c1", name: "restart_controller", arguments: {}, class: "write", created_at: at }), "2");
      es.emitNamedEvent("approval_requested", JSON.stringify({ approval_id: "ap1", call_id: "c1", created_at: at }), "3");
      es.emitNamedEvent("done", JSON.stringify({ state: "waiting_approval", created_at: at }), "4");
    });
    await screen.findByText("restart it");
    return { es, count: MockEventSource.instances.length, at };
  }

  it("resolves an approval in place without a new stream", async () => {
    const { es, count, at } = await openWaitingApproval();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await waitFor(() => expect(client.approveAssistantApproval).toHaveBeenCalledWith("ap1"));
    await act(async () => { await new Promise((r) => setTimeout(r, 20)); });
    expect(screen.getByText("restart it")).toBeTruthy();
    expect(MockEventSource.instances.length).toBe(count);
    expect(MockEventSource.instances.filter((i) => i.url.includes("after=0"))).toHaveLength(1);

    act(() => {
      es.emitNamedEvent("approval_resolved", JSON.stringify({ approval_id: "ap1", state: "approved", created_at: at }), "5");
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("restart_controller · In progress"));

    act(() => {
      es.emitNamedEvent("tool_result", JSON.stringify({ call_id: "c1", content: "ok", is_error: false, data_level: "logs", created_at: at }), "6");
      es.emitNamedEvent("assistant", JSON.stringify({ turn: 2, text: "Restarted.", final: true, created_at: at }), "7");
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: at }), "8");
    });
    expect(await screen.findByText("Restarted.")).toBeTruthy();
    expect(MockEventSource.instances.length).toBe(count);
  });

  it("resolves a denial in place without a new stream", async () => {
    const { es, count, at } = await openWaitingApproval();
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    await waitFor(() => expect(client.denyAssistantApproval).toHaveBeenCalledWith("ap1", {}));
    await act(async () => { await new Promise((r) => setTimeout(r, 20)); });
    expect(screen.getByText("restart it")).toBeTruthy();
    expect(MockEventSource.instances.length).toBe(count);

    act(() => {
      es.emitNamedEvent("approval_resolved", JSON.stringify({ approval_id: "ap1", state: "denied", created_at: at }), "5");
    });
    expect(await screen.findByText("restart_controller · Denied")).toBeTruthy();
    expect(screen.getByText("restart it")).toBeTruthy();
    expect(MockEventSource.instances.length).toBe(count);
  });

  it("moves focus from Approve to the message box when the approval resolves", async () => {
    const { es, at } = await openWaitingApproval();
    const approve = await screen.findByRole("button", { name: "Approve" });
    act(() => approve.focus());
    fireEvent.click(approve);
    await waitFor(() => expect(client.approveAssistantApproval).toHaveBeenCalledWith("ap1"));

    act(() => {
      es.emitNamedEvent("approval_resolved", JSON.stringify({ approval_id: "ap1", state: "approved", created_at: at }), "5");
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByLabelText("Message"));
  });

  it("moves focus from Deny to the message box when the approval resolves", async () => {
    const { es, at } = await openWaitingApproval();
    const deny = await screen.findByRole("button", { name: "Deny" });
    act(() => deny.focus());
    fireEvent.click(deny);
    await waitFor(() => expect(client.denyAssistantApproval).toHaveBeenCalledWith("ap1", {}));

    act(() => {
      es.emitNamedEvent("approval_resolved", JSON.stringify({ approval_id: "ap1", state: "denied", created_at: at }), "5");
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Deny" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByLabelText("Message"));
  });

  it("restores the message when the send fails", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: { ...session, state: "completed" }, approvals: [] });
    vi.mocked(client.postAssistantMessage).mockRejectedValueOnce(new Error("network down"));
    const { container } = renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: "2026-10-31T10:00:00Z" }), "1");
    });
    const box = await screen.findByLabelText("Message");
    fireEvent.change(box, { target: { value: "retry me" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The message was not sent.");
    expect((box as HTMLTextAreaElement).value).toBe("retry me");
    expect(Array.from(container.querySelectorAll("p")).some((p) => p.textContent === "retry me")).toBe(false);
    expect(screen.getByRole("status").textContent).toBe("");
  });

  /** openCompleted opens s1 as a completed session and returns the message box. */
  async function openCompleted() {
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: { ...session, state: "completed" }, approvals: [] });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: "2026-11-05T10:00:00Z" }), "1");
    });
    return (await screen.findByLabelText("Message")) as HTMLTextAreaElement;
  }

  it.each([
    ["Send has focus", true],
    ["a click on Send does not focus it", false],
  ])("moves focus to the message box when a send starts and %s", async (_label, focusSend) => {
    const box = await openCompleted();
    fireEvent.change(box, { target: { value: "and the fix?" } });
    const send = screen.getByRole("button", { name: "Send" }) as HTMLButtonElement;
    await waitFor(() => expect(send.disabled).toBe(false));
    if (focusSend) act(() => send.focus());
    else act(() => (document.activeElement as HTMLElement | null)?.blur());
    fireEvent.click(send);
    await waitFor(() => expect(client.postAssistantMessage).toHaveBeenCalledWith("s1", { message: "and the fix?" }));
    expect(send.disabled).toBe(true);
    expect(document.activeElement).toBe(box);
  });

  it("offers a new conversation when a follow-up answers a plain 409", async () => {
    vi.mocked(client.postAssistantMessage).mockRejectedValueOnce(
      new ApiError(409, "conflict", { error: "the session token budget is spent", code: "assistant_busy" }),
    );
    vi.mocked(client.createAssistantSession).mockResolvedValue({ ...session, id: "s2" });
    const box = await openCompleted();
    fireEvent.change(box, { target: { value: "one more" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(false));
    const reads = vi.mocked(client.getAssistantSession).mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("This conversation cannot continue. Start a new conversation.");
    await waitFor(() => expect(vi.mocked(client.getAssistantSession).mock.calls.length).toBeGreaterThan(reads));
    expect(box.value).toBe("one more");
    expect(screen.queryByRole("button", { name: "New conversation for this page" })).toBeNull();

    const startOver = screen.getByRole("button", { name: "Start a new conversation" });
    act(() => startOver.focus());
    fireEvent.click(startOver);
    expect(screen.queryByRole("button", { name: "Start a new conversation" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("No session")).toBeTruthy();
    expect(box.value).toBe("one more");
    expect(document.activeElement).toBe(box);

    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(client.createAssistantSession).toHaveBeenLastCalledWith({ context: { route: "dashboard" }, message: "one more" }));
  });

  it("shows a settings link when a create is refused with assistant_off", async () => {
    vi.mocked(client.createAssistantSession).mockRejectedValueOnce(
      new ApiError(409, "conflict", { error: "the assistant is off for this user", code: "assistant_off" }),
    );
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    const box = (await screen.findByLabelText("Message")) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "why did it fail" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The assistant is off.");
    const link = screen.getByRole("link", { name: "Open assistant settings" });
    expect(link.getAttribute("href")).toBe("/profile?tab=assistant");
    expect(box.value).toBe("why did it fail");
  });

  it("opens the session list when a create is refused with 429 assistant_busy", async () => {
    vi.mocked(client.createAssistantSession).mockRejectedValueOnce(
      new ApiError(429, "busy", { error: "too many sessions are running", code: "assistant_busy" }),
    );
    vi.mocked(client.listAssistantSessions).mockResolvedValueOnce({ items: [{ ...session, id: "s9", firstMessage: "" }], nextCursor: null } as never);
    renderDrawer();
    fireEvent.click(screen.getByText("toggle"));
    const box = (await screen.findByLabelText("Message")) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "one more" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The assistant is busy. Try again soon.");
    expect(await screen.findByRole("list", { name: "Sessions" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sessions" }).getAttribute("aria-expanded")).toBe("true");
    expect(box.value).toBe("one more");
  });
});

const originalMatchMedia = window.matchMedia;
const originalWidth = window.innerWidth;
const mediaListeners = new Set<() => void>();
let viewportWidth = 1280;

/** stubViewport sets the width and answers the shell's two media queries from it. */
function stubViewport(width: number) {
  viewportWidth = width;
  Object.defineProperty(window, "innerWidth", { configurable: true, value: width, writable: true });
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    get matches() {
      if (query === NARROW_QUERY) return viewportWidth < 640;
      if (query === DOCKED_QUERY) return viewportWidth >= 1024;
      return false;
    },
    media: query,
    addEventListener: (_type: string, fn: () => void) => mediaListeners.add(fn),
    removeEventListener: (_type: string, fn: () => void) => mediaListeners.delete(fn),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    onchange: null,
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

/** resizeViewport changes the width and tells every media listener and resize listener. */
function resizeViewport(width: number) {
  act(() => {
    viewportWidth = width;
    Object.defineProperty(window, "innerWidth", { configurable: true, value: width, writable: true });
    for (const fn of Array.from(mediaListeners)) fn();
    window.dispatchEvent(new Event("resize"));
  });
}

/** FocusProbe holds a page button, a top-bar door, and two routes. */
function FocusProbe() {
  const a = useAssistant();
  const navigate = useNavigate();
  return (
    <>
      <button ref={a.registerDoor} type="button" onClick={(e) => a.toggleDrawer(e.currentTarget)}>door</button>
      <Routes>
        <Route
          path="/"
          element={
            <>
              <button type="button" onClick={(e) => a.openSession("s1", e.currentTarget)}>explain</button>
              <button type="button" onClick={(e) => a.explainRun("r1", { route: "dashboard" }, e.currentTarget)}>explain run</button>
            </>
          }
        />
        <Route path="*" element={<p>other page</p>} />
      </Routes>
      <button type="button" onClick={() => navigate("/other")}>go</button>
      <span>{a.drawerOpen ? "drawer open" : "drawer closed"}</span>
      {a.drawerOpen && <AssistantDrawer />}
    </>
  );
}

function renderFocusProbe() {
  return renderWithProviders(
    <AssistantProvider>
      <FocusProbe />
    </AssistantProvider>,
    { permissions: { global: { "*": { "*": true } } } as never },
  );
}

/** openFrom opens the drawer from the named button and waits for focus on Close. */
async function openFrom(name: string) {
  const opener = screen.getByRole("button", { name });
  act(() => opener.focus());
  fireEvent.click(opener);
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" })));
  return opener;
}

describe("AssistantDrawer focus return", () => {
  afterEach(() => {
    window.matchMedia = originalMatchMedia;
    Object.defineProperty(window, "innerWidth", { configurable: true, value: originalWidth, writable: true });
    mediaListeners.clear();
    document.body.style.overflow = "";
  });

  it.each([
    [1280, "complementary"],
    [800, "complementary"],
    [500, "dialog"],
  ])("returns focus to the opener on Close at %i px", async (width, role) => {
    stubViewport(width);
    renderFocusProbe();
    const opener = await openFrom("explain");
    expect(screen.getByText("drawer open")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(screen.queryByRole(role, { name: "Assistant" })).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it.each([
    [1280, "complementary"],
    [800, "complementary"],
    [500, "dialog"],
  ])("returns focus to the opener on Escape at %i px", async (width, role) => {
    stubViewport(width);
    renderFocusProbe();
    const opener = await openFrom("explain");
    fireEvent.keyDown(screen.getByRole(role, { name: "Assistant" }), { key: "Escape" });
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(opener);
  });

  it("returns focus to the opener on a scrim tap at 500px", async () => {
    stubViewport(500);
    renderFocusProbe();
    const opener = await openFrom("explain");
    const scrim = screen.getByRole("dialog", { name: "Assistant" }).previousElementSibling as HTMLElement;
    fireEvent.click(scrim);
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(opener);
  });

  it("keeps focus in place on a route change and falls back to the door", async () => {
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    expect(await screen.findByText("other page")).toBeTruthy();
    expect(screen.getByText("drawer open")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));
    fireEvent.keyDown(screen.getByRole("complementary", { name: "Assistant" }), { key: "Escape" });
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "door" }));
  });

  it("keeps focus on the door when the door closes the drawer", async () => {
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    const door = screen.getByRole("button", { name: "door" });
    act(() => door.focus());
    fireEvent.click(door);
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(door);
  });

  it("keeps focus on Close when the viewport grows from 500px to 800px", async () => {
    stubViewport(500);
    renderFocusProbe();
    await openFrom("explain");
    const close = screen.getByRole("button", { name: "Close" });
    resizeViewport(800);
    expect(screen.getByRole("complementary", { name: "Assistant" })).toBeTruthy();
    expect(screen.getByText("drawer open")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Close" })).toBe(close);
    expect(document.activeElement).toBe(close);
  });

  it.each([1280, 800, 500])("keeps focus on the door when a click on it does not focus it at %i px", async (width) => {
    stubViewport(width);
    renderFocusProbe();
    const door = screen.getByRole("button", { name: "door" });
    await openFrom("explain");
    // jsdom, like Safari, does not focus a button on click.
    fireEvent.click(door);
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(door);
  });

  it.each([800, 500])("moves focus from the docked handle to Close when the viewport shrinks to %i px", async (width) => {
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    const handle = screen.getByRole("separator", { name: "Resize the assistant drawer" });
    act(() => handle.focus());
    resizeViewport(width);
    expect(screen.queryByRole("separator", { name: "Resize the assistant drawer" })).toBeNull();
    expect(screen.getByText("drawer open")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));
  });

  it("keeps focus outside the drawer when the docked handle unmounts", async () => {
    stubViewport(1280);
    renderFocusProbe();
    const opener = await openFrom("explain");
    act(() => opener.focus());
    resizeViewport(800);
    expect(screen.queryByRole("separator", { name: "Resize the assistant drawer" })).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it.each(["completed", "cancelled"])("moves focus from Stop to the message box when the session ends as %s", async (state) => {
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    const stop = await screen.findByRole("button", { name: "Stop" });
    act(() => stop.focus());
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state, created_at: "2026-11-05T10:00:00Z" }), "1");
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByLabelText("Message"));
  });

  it("keeps focus on Close when Stop goes away without focus", async () => {
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    await screen.findByRole("button", { name: "Stop" });
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];
    act(() => {
      es.emitNamedEvent("done", JSON.stringify({ state: "completed", created_at: "2026-11-05T10:00:00Z" }), "1");
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop" })).toBeNull());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));
  });

  it("moves focus from New conversation for this page to the message box", async () => {
    vi.mocked(client.getAssistantSession).mockResolvedValue({
      session: { ...session, context: { route: "tenant", tenant: "payments" } },
      approvals: [],
    });
    stubViewport(1280);
    renderFocusProbe();
    await openFrom("explain");
    const newConversation = await screen.findByRole("button", { name: "New conversation for this page" });
    act(() => newConversation.focus());
    fireEvent.click(newConversation);
    expect(screen.queryByRole("button", { name: "New conversation for this page" })).toBeNull();
    expect(screen.getByText("No session")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByLabelText("Message"));
  });

  it.each([
    [1280, "complementary"],
    [800, "complementary"],
    [500, "dialog"],
  ])("returns focus to a page button that a click did not focus at %i px", async (width, role) => {
    stubViewport(width);
    renderFocusProbe();
    const opener = screen.getByRole("button", { name: "explain" });
    // jsdom, like Safari, does not focus a button on click.
    fireEvent.click(opener);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" })));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(screen.queryByRole(role, { name: "Assistant" })).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("returns focus to the page button after an explain run that a click did not focus", async () => {
    vi.mocked(client.createAssistantSession).mockResolvedValue({ ...session, id: "s1" });
    stubViewport(1280);
    renderFocusProbe();
    const opener = screen.getByRole("button", { name: "explain run" });
    fireEvent.click(opener);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" })));
    fireEvent.keyDown(screen.getByRole("complementary", { name: "Assistant" }), { key: "Escape" });
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(opener);
  });

  it("keeps the opener when a page button opens a session while the drawer is open", async () => {
    stubViewport(1280);
    renderFocusProbe();
    const door = await openFrom("door");
    fireEvent.click(screen.getByRole("button", { name: "explain" }));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByText("drawer closed")).toBeTruthy();
    expect(document.activeElement).toBe(door);
    // The next open does not reuse the page button that named itself while the drawer was open.
    await openFrom("door");
    fireEvent.keyDown(screen.getByRole("complementary", { name: "Assistant" }), { key: "Escape" });
    expect(document.activeElement).toBe(door);
  });
});
