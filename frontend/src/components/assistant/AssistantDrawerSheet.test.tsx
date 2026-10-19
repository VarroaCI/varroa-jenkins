import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { installMockEventSource } from "../../test/mockEventSource";
import { AssistantProvider, useAssistant } from "./AssistantProvider";
import AssistantDrawer from "./AssistantDrawer";
import { useNarrowViewport, NARROW_QUERY } from "../../hooks/useNarrowViewport";

vi.mock("../../hooks/useApi", async (orig) => {
  const actual = await orig<typeof import("../../hooks/useApi")>();
  return { ...actual, bffFetch: vi.fn(async () => ({ ticket: "t", expiresInSeconds: 30 })) };
});
vi.mock("../../api/client", async (orig) => {
  const actual = await orig<typeof import("../../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(async () => ({ available: true, enabled: true, sessionKey: null })),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    getAssistantSession: vi.fn(async () => ({ session: { id: "s1", state: "running", context: {} }, approvals: [] })),
    listAssistantSessions: vi.fn(async () => ({ items: [], nextCursor: null })),
  };
});

/** matchMediaStub answers the narrow query with the given match. */
function matchMediaStub(narrow: boolean) {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: narrow && query === NARROW_QUERY,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    onchange: null,
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

function Probe() {
  const a = useAssistant();
  return (
    <>
      <button type="button" onClick={() => a.openSession("s1")}>open</button>
      {a.drawerOpen && <AssistantDrawer />}
    </>
  );
}

installMockEventSource();

beforeEach(() => {
  matchMediaStub(true);
});
afterEach(() => {
  document.body.style.overflow = "";
});

describe("AssistantDrawer below 640px", () => {
  it("is a modal dialog with a scrim that closes it", async () => {
    renderWithProviders(<AssistantProvider><Probe /></AssistantProvider>, { permissions: { global: { "*": { "*": true } } } as never });
    fireEvent.click(screen.getByText("open"));
    const dialog = await screen.findByRole("dialog", { name: "Assistant" });
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(document.body.style.overflow).toBe("hidden");
    fireEvent.click(document.querySelector("[role=presentation]") as HTMLElement);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.style.overflow).toBe("");
  });

  it("closes on Escape and cycles Tab inside the sheet", async () => {
    renderWithProviders(<AssistantProvider><Probe /></AssistantProvider>, { permissions: { global: { "*": { "*": true } } } as never });
    fireEvent.click(screen.getByText("open"));
    const dialog = await screen.findByRole("dialog", { name: "Assistant" });
    const buttons = Array.from(dialog.querySelectorAll<HTMLElement>("button, textarea")).filter((el) => !el.hasAttribute("disabled"));
    const last = buttons[buttons.length - 1];
    last.focus();
    fireEvent.keyDown(dialog, { key: "Tab" });
    expect(document.activeElement).toBe(buttons[0]);
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("reports the narrow query as false without matchMedia", () => {
    window.matchMedia = undefined as unknown as typeof window.matchMedia;
    const Hook = () => <span>{String(useNarrowViewport())}</span>;
    renderWithProviders(<Hook />);
    expect(screen.getByText("false")).toBeTruthy();
  });
});
