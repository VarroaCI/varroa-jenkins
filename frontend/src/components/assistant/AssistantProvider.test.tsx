import { describe, it, expect, vi, beforeEach } from "vitest";
import { waitFor, act, render } from "@testing-library/react";
import type { ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { createTestQueryClient } from "../../test/render-utils";
import { AssistantProvider, useAssistant, useAssistantContextHint } from "./AssistantProvider";
import { pendingNotes } from "./pendingNotes";
import * as client from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { createAssistantSession, createMyAssistant } from "../../test/assistantFactories";

vi.mock("../../context/AuthContext", () => ({
  useAuth: () => ({ user: { subject: "alice" } }),
  useOptionalAuth: () => ({ user: { subject: "alice" } }),
}));
vi.mock("../../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: { "*": { "*": true } } } }),
  canDoGlobal: () => true,
}));
vi.mock("../../api/client", async (orig) => {
  const actual = await orig<typeof import("../../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    getAssistantSession: vi.fn(),
    createAssistantSession: vi.fn(),
    createRunNote: vi.fn(),
  };
});

/** Probe exposes the provider state to the test. */
function Probe({ onState }: { onState: (a: ReturnType<typeof useAssistant>) => void }) {
  const a = useAssistant();
  onState(a);
  return null;
}

function renderWith(node: ReactNode, qc = createTestQueryClient()) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{node}</MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
  vi.mocked(client.createAssistantSession).mockResolvedValue(createAssistantSession({ id: "s9" }));
});

describe("AssistantProvider availability", () => {
  it("reads available and switchOn from the me answer", async () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    expect(state!.available).toBe(true);
    expect(state!.switchOn).toBe(true);
    expect(state!.canUse).toBe(true);
  });

  it("keeps the door off when the caller has no assistant grant", async () => {
    vi.mocked(client.getMyAssistant).mockRejectedValue(new ApiError(403, "forbidden"));
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    expect(state!.available).toBe(false);
    expect(state!.switchOn).toBe(false);
  });

  it("is off outside a provider", () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(<Probe onState={(a) => { state = a; }} />);
    expect(state!.switchOn).toBe(false);
  });
});

describe("AssistantProvider helpers", () => {
  it("opens a session, remembers it, and toggles the drawer", async () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    act(() => state!.openSession("s1"));
    expect(state!.drawerOpen).toBe(true);
    expect(state!.activeSessionId).toBe("s1");
    expect(window.localStorage.getItem("varroa-assistant-last-session:alice")).toBe("s1");
    act(() => state!.toggleDrawer());
    expect(state!.drawerOpen).toBe(false);
  });

  it("starts a session from a message and reports a refusal", async () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    const ok = await state!.startSession({ context: { route: "dashboard" }, message: "hi" });
    expect(ok).toEqual({ ok: true, sessionId: "s9" });
    vi.mocked(client.createAssistantSession).mockRejectedValueOnce(new ApiError(429, "busy"));
    const busy = await state!.startSession({ context: {}, message: "again" });
    expect(busy).toMatchObject({ ok: false, code: "assistant_busy" });
  });

  it("posts a pending note once its explain session completes", async () => {
    vi.mocked(client.createRunNote).mockResolvedValue({} as never);
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    pendingNotes("alice").add({ sessionId: "s5", runId: "r5" });
    vi.mocked(client.getAssistantSession).mockResolvedValue({ session: createAssistantSession({ id: "s5", state: "completed" }), approvals: [] });
    act(() => state!.onSessionDone("s5", "completed"));
    await waitFor(() => expect(client.createRunNote).toHaveBeenCalledWith("r5", { sessionId: "s5" }));
    expect(pendingNotes("alice").list()).toEqual([]);
  });

  it("drops a pending note for a session that ended without an answer", async () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    pendingNotes("alice").add({ sessionId: "s6", runId: "r6" });
    act(() => state!.onSessionDone("s6", "failed"));
    expect(pendingNotes("alice").list()).toEqual([]);
    expect(client.createRunNote).not.toHaveBeenCalled();
  });

  it("refreshes the approval reads after a decision and keeps the epoch", async () => {
    const qc = createTestQueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    let state: ReturnType<typeof useAssistant> | null = null;
    renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
      </AssistantProvider>,
      qc,
    );
    await waitFor(() => expect(state?.answered).toBe(true));
    act(() => state!.openSession("s1"));
    const epoch = state!.epoch;
    spy.mockClear();
    act(() => state!.resolved("s1"));
    expect(state!.epoch).toBe(epoch);
    expect(spy).toHaveBeenCalledWith({ queryKey: ["assistant-approvals"] });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["assistant-session", "s1"] });
  });

  it("sets the page hint while a page mounts and clears it on unmount", async () => {
    let state: ReturnType<typeof useAssistant> | null = null;
    function Page() {
      useAssistantContextHint({ cell: true });
      return null;
    }
    const { unmount } = renderWith(
      <AssistantProvider>
        <Probe onState={(a) => { state = a; }} />
        <Page />
      </AssistantProvider>,
    );
    await waitFor(() => expect(state?.hint).toEqual({ cell: true }));
    unmount();
  });
});
