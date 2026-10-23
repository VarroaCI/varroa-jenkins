import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { installMockEventSource, MockEventSource } from "../../test/mockEventSource";
import { AssistantProvider, useAssistant } from "./AssistantProvider";
import AssistantDrawer from "./AssistantDrawer";
import * as client from "../../api/client";
import type { AssistantSession } from "../../types/assistant";

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
});
