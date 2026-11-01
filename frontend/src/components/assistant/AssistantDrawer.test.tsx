import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act, within } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { installMockEventSource, MockEventSource } from "../../test/mockEventSource";
import { AssistantProvider, useAssistant } from "./AssistantProvider";
import AssistantDrawer from "./AssistantDrawer";
import * as client from "../../api/client";
import type { AssistantSession } from "../../types/assistant";
import { contextLabel } from "./routeContext";

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
      <button type="button" onClick={a.toggleDrawer}>toggle</button>
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
    expect(await screen.findByText("State: completed")).toBeTruthy();
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
      items: [{ ...session, startedAt }],
      nextCursor: null,
    });
    renderDrawer();
    fireEvent.click(screen.getByText("open"));
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    fireEvent.click(await screen.findByRole("button", { name: "Sessions" }));
    const list = await screen.findByRole("list", { name: "Sessions" });
    const row = within(list).getByRole("button");
    expect(row.textContent).toContain(contextLabel({ route: "dashboard" }));
    expect(row.textContent).toMatch(/5m ago · Current/);
    expect(row.getAttribute("aria-current")).toBe("true");
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
});
