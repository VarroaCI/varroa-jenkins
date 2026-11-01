import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { installMockEventSource } from "../../test/mockEventSource";
import { AssistantProvider, useAssistant } from "./AssistantProvider";
import AssistantDrawer from "./AssistantDrawer";
import { DOCKED_QUERY, DRAWER_WIDTH_KEY } from "./drawerWidth";

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

/** matchMediaStub answers the docked query with the given match. */
function matchMediaStub(docked: boolean) {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: docked && query === DOCKED_QUERY,
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

/** openDrawer opens the drawer and returns the resize separator. A beforeOpen hook runs after the render and before the drawer reads storage. */
async function openDrawer(beforeOpen?: () => void) {
  renderWithProviders(<AssistantProvider><Probe /></AssistantProvider>, { permissions: { global: { "*": { "*": true } } } as never });
  beforeOpen?.();
  fireEvent.click(screen.getByText("open"));
  return screen.findByRole("separator", { name: "Resize the assistant drawer" });
}

installMockEventSource();

beforeEach(() => {
  matchMediaStub(true);
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1024, writable: true });
  window.localStorage.clear();
});
afterEach(() => {
  vi.restoreAllMocks();
  document.body.style.overflow = "";
});

describe("AssistantDrawer resize at 1024px and wider", () => {
  it("exposes a separator with the bounds at 1024px", async () => {
    const sep = await openDrawer();
    expect(sep.getAttribute("aria-valuenow")).toBe("320");
    expect(sep.getAttribute("aria-valuemin")).toBe("320");
    expect(sep.getAttribute("aria-valuemax")).toBe("409");
  });

  it("ArrowLeft widens the drawer and stores the width", async () => {
    const sep = await openDrawer();
    fireEvent.keyDown(sep, { key: "ArrowLeft" });
    expect(sep.getAttribute("aria-valuenow")).toBe("336");
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe("336");
    const section = screen.getByRole("complementary", { name: "Assistant" });
    expect(section.style.getPropertyValue("--drawer-w")).toBe("336px");
  });

  it("opens with the stored width and moves to the limits with End and Home", async () => {
    window.localStorage.setItem(DRAWER_WIDTH_KEY, "400");
    const sep = await openDrawer();
    expect(sep.getAttribute("aria-valuenow")).toBe("400");
    fireEvent.keyDown(sep, { key: "End" });
    expect(sep.getAttribute("aria-valuenow")).toBe("409");
    fireEvent.keyDown(sep, { key: "Home" });
    expect(sep.getAttribute("aria-valuenow")).toBe("320");
  });

  it("has no handle and no inline width below the docked range", async () => {
    matchMediaStub(false);
    renderWithProviders(<AssistantProvider><Probe /></AssistantProvider>, { permissions: { global: { "*": { "*": true } } } as never });
    fireEvent.click(screen.getByText("open"));
    await screen.findByRole("complementary", { name: "Assistant" });
    expect(screen.queryByRole("separator", { name: "Resize the assistant drawer" })).toBeNull();
    const section = screen.getByRole("complementary", { name: "Assistant" });
    expect(section.style.getPropertyValue("--drawer-w")).toBe("");
  });

  it("still opens and resizes when the store is blocked", async () => {
    const sep = await openDrawer(() => {
      vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("blocked"); });
      vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("blocked"); });
    });
    expect(sep.getAttribute("aria-valuenow")).toBe("320");
    fireEvent.keyDown(sep, { key: "ArrowLeft" });
    expect(sep.getAttribute("aria-valuenow")).toBe("336");
  });
});
