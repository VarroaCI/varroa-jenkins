import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { ProfileAssistantTab, keyStatus } from "./ProfileAssistantTab";
import { AssistantProvider } from "../components/assistant/AssistantProvider";
import * as client from "../api/client";
import { ApiError } from "../hooks/useApi";
import { createMyAssistant } from "../test/assistantFactories";

vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    listAssistantProviders: vi.fn(async () => ({
      items: [{ name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://x" }],
    })),
    putMyAssistant: vi.fn(async () => createMyAssistant()),
    putMyAssistantKey: vi.fn(async () => createMyAssistant({ sessionKey: { provider: "smoke" } })),
    deleteMyAssistantKey: vi.fn(async () => undefined),
  };
});

const perms = { global: { "*": { "*": true } } } as never;

function renderTab(route = "/profile?tab=assistant", state?: unknown) {
  return renderWithProviders(
    <AssistantProvider>
      <ProfileAssistantTab />
    </AssistantProvider>,
    { permissions: perms, route, routerState: state },
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
});

describe("ProfileAssistantTab", () => {
  it("saves the switch with all four fields", async () => {
    renderTab();
    const sw = await screen.findByRole("switch", { name: "Assistant on" });
    await waitFor(() => expect((sw as HTMLInputElement).checked).toBe(true));
    fireEvent.click(sw);
    await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({
      enabled: false, provider: "smoke", model: "varroa-smoke", dataSharing: "logs",
    })));
  });

  it("shows the server message when a save is refused", async () => {
    vi.mocked(client.putMyAssistant).mockRejectedValueOnce(new ApiError(400, "bad", { error: "model is not offered" }));
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    expect(await screen.findByText("model is not offered")).toBeTruthy();
  });

  it("states the spend today", async () => {
    renderTab();
    expect(await screen.findByText("0 of 100000 tokens used today (UTC)")).toBeTruthy();
  });

  it("explains the explain notice when opened from a failed build", async () => {
    renderTab("/profile?tab=assistant", { from: "explain" });
    expect(await screen.findByText("Turn on the assistant to explain a failed build.")).toBeTruthy();
  });

  it("sets a session key directly and clears the field, with no key in any storage or cache", async () => {
    renderTab();
    const input = await screen.findByLabelText("API key");
    fireEvent.change(input, { target: { value: "sk-test-123" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    await waitFor(() => expect(client.putMyAssistantKey).toHaveBeenCalledWith({ provider: "smoke", apiKey: "sk-test-123" }));
    await waitFor(() => expect((screen.getByLabelText("API key") as HTMLInputElement).value).toBe(""));
    expect(JSON.stringify(window.localStorage)).not.toContain("sk-test-123");
    expect(JSON.stringify(window.sessionStorage)).not.toContain("sk-test-123");
  });

  it("explains a 409 for an API key session", async () => {
    vi.mocked(client.putMyAssistantKey).mockRejectedValueOnce(new ApiError(409, "conflict"));
    renderTab();
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: "k" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    expect(await screen.findByText("A key needs a browser login. An API key session cannot hold one.")).toBeTruthy();
  });

  it("names the unavailable key error", async () => {
    vi.mocked(client.putMyAssistantKey).mockRejectedValueOnce(new ApiError(503, "x", { code: "assistant_key_unavailable" }));
    renderTab();
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: "k" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    expect(await screen.findByText(/not available in this login/)).toBeTruthy();
  });

  it("clears the key and reports the status", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ sessionKey: { provider: "smoke" } }));
    renderTab();
    expect(await screen.findByText("Key set for smoke, held for this login only")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear key" }));
    await waitFor(() => expect(client.deleteMyAssistantKey).toHaveBeenCalled());
  });

  it("reports no key when none is set", () => {
    expect(keyStatus(createMyAssistant())).toBe("No key set");
  });
});
